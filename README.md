# cpa-stego-guard

[CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI)（CPA）v8 插件：清洗 Claude Code 请求里的隐写标记。

Claude Code **2.1.91–2.1.196** 在 `ANTHROPIC_BASE_URL` 不是官方地址时，会在首条用户消息 `<system-reminder>` 块的日期行里，用不同的撇号和日期分隔符记下“这个 base URL 命中了哪些名单、用户是不是在中国时区”。经过 CPA 转发时，这一行会原样到达 Anthropic。本插件在 CPA 转发前把它改回干净的样子，或者直接拒绝这类请求。

2.1.293 起 Claude Code 已移除这套代码，但旧版本仍在流传。

## 标记含义

日期行长这样（干净时）：

```text
<system-reminder>
...
# currentDate
Today's date is 2026-10-09.
...
</system-reminder>
```

| 位置 | 字符 | 含义 |
|---|---|---|
| `Today` 后面的撇号 | U+0027 `'` | 未命中（干净） |
| | U+2019 `’` | 命中域名名单 |
| | U+02BC `ʼ` | 命中 AI 实验室关键词 |
| | U+02B9 `ʹ` | 两者都命中 |
| 日期分隔符 | `-` | 干净 |
| | `/`（`2026/10/09`） | 中国时区 |

规则参考 [MoTeam-cn/ClaudeGate](https://github.com/MoTeam-cn/ClaudeGate) 的 `docs/fingerprint.md`「隐写拦截」一节（Apache-2.0）。本项目只参考规则，没有复制代码。

## 插件做什么

- 只看 Anthropic Messages 格式的请求（`/v1/messages`、`/v1/messages/count_tokens`），只在**首条用户消息**里 `<system-reminder>…</system-reminder>` 内、行首为 `Today?s date is YYYY?MM?DD` 的日期行上判定和修改：
  - `Today` 后面是 U+2019 / U+02BC / U+02B9 → 改回 U+0027；
  - 日期是 `YYYY/MM/DD` → 改成 `YYYY-MM-DD`。
- Claude Code 系统提示词里本来就有的 U+2019（如 `it’s`）不会被改；用户正文、后续消息、system 字段里的同样文字都不会被改。
- 日期行以外出现的 U+02BC / U+02B9 只记日志，不改（避免改动用户正文）。
- 只做字节级最小替换：撇号（UTF-8 或 `\uXXXX` 转义）换成 `'`，分隔符（`/` 或 `\/`）换成 `-`。不重新序列化 JSON，干净的请求体一个字节都不变；同样输入每次结果一致，清洗后的请求再过一遍不会再改。
- CPA 用 Claude OAuth 订阅账号转发时，会在插件之后对最终请求体重新计算 `cch` 签名，所以清洗不会让 `cch` 对不上。e2e 测试里，带标记的请求清洗后发往上游的字节（含 `cch`）与本来就干净的请求完全一致。
- 不影响 CPA 对 Claude Code 客户端的识别（识别看请求头和 `metadata.user_id`，不看日期行）。

## 安装

从 Release 下载对应平台的压缩包（发版后提供），或自己构建：

```bash
CGO_ENABLED=1 go build -buildvcs=false -tags cshared -buildmode=c-shared \
  -o cpa-stego-guard.so ./cmd/cpa-stego-guard
```

把 `cpa-stego-guard.so`（macOS 为 `.dylib`，Windows 为 `.dll`）放进 CPA 的插件目录。**文件名决定插件 id**，必须是 `cpa-stego-guard`。

## 配置

```yaml
plugins:
  enabled: true
  dir: "plugins"
  configs:
    cpa-stego-guard:
      enabled: true
      mode: clean                 # clean | block | log | off
      clean_quote_markers: true   # 日期行撇号 U+2019 / U+02BC / U+02B9
      clean_region_marker: true   # 斜杠日期（中国时区）
```

| 配置项 | 默认值 | 说明 |
|---|---|---|
| `mode` | `clean` | `clean`：清洗后照常转发。`block`：不转发，返回 400，响应体说明命中了哪一条并建议把 Claude Code 升级到 2.1.293 以上。`log`：只记日志不改。`off`：关闭。写错的值按 `clean` 处理并记一条警告。 |
| `clean_quote_markers` | `true` | 是否处理日期行撇号标记（域名名单、AI 实验室关键词）。 |
| `clean_region_marker` | `true` | 是否处理斜杠日期（中国时区标记）。 |

两个开关只在 `mode` 为 `clean` / `block` 时起作用。关掉某一类时，该类标记**不清洗、不拦截，只记日志**。例如 `mode: block` 加 `clean_region_marker: false` 时，只有斜杠日期的请求会照常转发（不改），撇号标记仍然拦截。

改配置后 CPA 热重载即生效，无需重启。

### 各模式的行为

| 请求 | clean | block | log | off |
|---|---|---|---|---|
| 干净 | 原样转发 | 原样转发 | 原样转发 | 原样转发 |
| 有启用类别的标记 | 清洗后转发 | 400，不转发 | 原样转发，记日志 | 原样转发 |
| 只有关掉类别的标记 | 原样转发，记日志 | 原样转发，记日志 | 原样转发，记日志 | 原样转发 |
| 日期行外有 U+02BC / U+02B9 | 原样转发，记日志 | 原样转发，记日志 | 原样转发，记日志 | 原样转发 |

`block` 模式返回的响应体是 Anthropic 错误格式：

```json
{"type":"error","error":{"type":"invalid_request_error","message":"请求已被 cpa-stego-guard 拦截，未转发上游。检测到 Claude Code 隐写标记：日期行 Today 后的撇号是 U+2019（命中域名名单）。…请把 Claude Code 升级到 2.1.293 或更高版本后重试。 / Blocked by cpa-stego-guard: …"}}
```

## 日志

通过 CPA 的 host log 输出，只有命中类型和计数，不含提示词内容：

```text
[info ] cpa-stego-guard configured mode=clean hook=request.patch_body_before clean_quote_markers=true clean_region_marker=true
[info ] cpa-stego-guard found steganographic markers action=cleaned mode=clean hook=request.patch_body_before date_lines=1 quote_u2019_domain=1 quote_u02bc_lab=0 quote_u02b9_both=0 region_slash_date=1 stray_u02bc=0 stray_u02b9=0 edits=3 ...
[warn ] cpa-stego-guard found steganographic markers action=blocked mode=block hook=request.intercept_before ...
```

CPA 的文本日志只打印它自己白名单里的字段，所以计数直接写在消息里。

## 局限

- **只认 Claude Code 的日期行格式。** 日期行必须在首条用户消息的 `<system-reminder>` 块内、位于行首，日期两个分隔符相同且月日合法。格式不同的变体不会被识别。
- **`clean` 模式按请求体形状判断协议。** `request.patch_body_before` 钩子只拿到请求体、没有协议信息，插件把“顶层是对象、有 `messages` 数组、没有 `contents` / `input`、没有 `developer` / `tool` / `function` 角色”的请求当作 Anthropic Messages。理论上，OpenAI Chat 格式里如果恰好带着 Claude Code 式的日期行也会被清洗，但实际只有转发 Claude Code 请求时才会出现，清掉同样无害。`block` 模式用的钩子带协议信息，只处理 `/v1/messages` 这一类请求。
- **`block` 模式更重一些。** 能拒绝请求的只有 `request.intercept_before`，CPA 会把请求体 JSON + Base64 编码后传给插件；`clean` / `log` 用的 `request.patch_body_before` 直接传原始字节。2 MB 的请求体扫描约 3 ms。
- **不处理其他身份信息。** 系统提示词里的平台、系统版本、工作目录、git 状态、`CLAUDE.md` 内容、用户邮箱，以及请求头、`metadata.user_id` 等都不在本插件范围内。
- **API key 账号的特殊情况。** 如果上游是 API key 且没有开启 CPA 的 claude-code-cli 指纹档，CPA 不重签 `cch`；此时若客户端自己带了 `cch`，清洗后会对不上。真 Claude Code 指向 CPA 时不发 `cch`，OAuth 订阅账号总会重签，这种情况通常不会出现。
- 只支持 CLIProxyAPI v8。插件不起 goroutine、不用定时器。

## 开发

```bash
go test -race ./...                                   # 单元测试
go test -run '^$' -fuzz FuzzScan -fuzztime 60s ./internal/stego
scripts/e2e.sh ../CLIProxyAPI                         # e2e（Linux，需要 root 以使用 unshare）
```

e2e 在私有的 mount + network 命名空间里运行：`api.anthropic.com` 被解析到本地的假上游（`scripts/fake_anthropic.py`，用临时 CA 签发证书，只有 CPA 进程信任），CPA 使用 Claude OAuth 形态的凭据，走与生产相同的 OAuth / `cch` 签名路径。脚本逐字节比对上游收到的请求体，命名空间外的网络和文件都不受影响。

## 许可

MIT，见 [LICENSE](LICENSE)。
