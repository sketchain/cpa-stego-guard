# cpa-stego-guard

[CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI)（CPA）v8 插件：清洗 Claude Code 请求里的隐写标记，并把请求体里的 `cc_version` 统一成同一个版本。

Claude Code **2.1.91–2.1.196** 在 `ANTHROPIC_BASE_URL` 不是官方地址时，会在首条用户消息 `<system-reminder>` 块的日期行里，用不同的撇号和日期分隔符记下“这个 base URL 命中了哪些名单、用户是不是在中国时区”。经过 CPA 转发时，这一行会原样到达 Anthropic。本插件在 CPA 转发前把它改回干净的样子，或者直接拒绝这类请求。

2.1.293 实测已没有这套代码（2.1.197–2.1.292 未经确认），但旧版本仍在流传。扫描不看版本号，只看内容。

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
- 配置了 `cc_version` 时，把 billing 头里的 `cc_version` 统一成这个版本（见下一节）。

## 统一 cc_version

CPA v8 打开 `claude-header-defaults.stabilize-device-profile` 后，客户端的 UA 版本、`X-Stainless-Package-Version`、`X-Stainless-Runtime-Version` 只要和基线不完全相同，整组就换成基线（内置 `claude-cli/2.1.321`、`0.128.0`、`v26.12.1`，或 `claude-header-defaults` 里配置的值）。但真 Claude Code 透传过来的请求体里，`system` 首块 billing 头的 `cc_version` 仍是客户端原值，于是请求头和请求体版本不一致，不同版本客户端发出去的 `cc_version` 也各不相同。

配置 `cc_version` 后，插件对每个 Messages 形状的请求：

1. 找到 `system` 数组里以 `x-anthropic-billing-header:` 开头的文本块，读出 `cc_version=<版本>.<3 位十六进制指纹>;`。没有 billing 头（不是 Claude Code）就不动。
2. 不论版本是否已等于目标，都用**原版本号**按 Claude Code 的算法重算指纹：`SHA256("59cf53e54c78" + 指纹文本的 UTF-16 第 4、7、20 个单元（越界补 '0'，孤立代理项按 U+FFFD 编码）+ 版本号)` 的十六进制前 3 位。指纹文本按 CPA 的取法：首条用户消息的 content 是字符串时取整个字符串，是数组时取第一个不是 `<system-reminder>…</system-reminder>` 块（也不是 currentDate 提醒）的 text 块。
3. **校验通过**：版本已等于目标时原样放行；否则把 `<版本>.<指纹>` 改成 `<目标版本>.<按目标版本重算的指纹>`，其余字节（包括 `cch`）不动。CPA 在插件之后对最终请求体重签 `cch`。
4. **校验不通过**（指纹对不上、billing 头格式认不出、或指纹文本无法按 CPA 的方式确定；版本已等于目标但指纹被改过也算）：不原样透传，而是删掉 billing 文本块和 `metadata.user_id`。CPA 识别 Claude Code 时 `/v1/messages` 必须有合法的 `metadata.user_id`，删掉后请求被当作普通 API 客户端：走 CPA 的 cloak 路径，由 CPA 自己生成 billing 头（版本取 CPA 的 `DefaultClaudeVersion`，即基线版本），并用 CPA 自己的 `metadata.user_id`。对未识别的请求 CPA 本来就忽略客户端的 `metadata.user_id`，所以删除它不损失信息。

只改请求体（继续用 `request.patch_body_before`，`block` 模式用 `request.intercept_before`），不改请求头，不增加钩子；未配置 `cc_version` 时连 billing 头都不读。改写是字节区间补丁，长度可以变化，不重新序列化 JSON；再过一遍不会再改。

与日期行清洗互不干扰：日期提醒块不进指纹文本；首条用户消息是单个字符串时，日期行一定在第 20 个 UTF-16 单元之后，清洗又是一个单元换一个单元，指纹不变。

`cc_version` **必须填 CPA 实际发出的 UA 版本**：没配 `claude-header-defaults.user-agent` 时就是 CPA 内置基线（v8.0.80 为 `2.1.321`）。插件 API 读不到 CPA 的 header-defaults，所以插件不内置任何默认版本；**CPA 升级基线或你改了 user-agent 时要跟着改**。留空时不做对齐（也不做回退），注册时记一条警告。

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
      cc_version: "2.1.321"       # CPA 实际发出的 UA 版本；留空则不统一 cc_version
```

| 配置项 | 默认值 | 说明 |
|---|---|---|
| `mode` | `clean` | `clean`：清洗后照常转发。`block`：不转发，返回 400，响应体说明命中了哪一条并建议把 Claude Code 升级到 2.1.293 以上。`log`：只记日志不改。`off`：关闭。写错的值按 `clean` 处理并记一条警告。 |
| `clean_quote_markers` | `true` | 是否处理日期行撇号标记（域名名单、AI 实验室关键词）。 |
| `clean_region_marker` | `true` | 是否处理斜杠日期（中国时区标记）。 |
| `cc_version` | 空 | 把 billing 头 `cc_version` 统一成的版本，格式 `x.y.z`。必须等于 CPA 发出的 UA 版本。留空或格式不对时不对齐（格式不对另记一条警告）。 |

两个开关只在 `mode` 为 `clean` / `block` 时起作用。`cc_version` 在 `clean` 和 `block` 下都执行对齐和回退（`block` 只因隐写标记拦截，没被拦截的请求照样对齐）；`log` 只记日志不改；`off` 什么都不做。关掉某一类时，该类标记**不清洗、不拦截，只记日志**。例如 `mode: block` 加 `clean_region_marker: false` 时，只有斜杠日期的请求会照常转发（不改），撇号标记仍然拦截。

改配置后 CPA 热重载即生效，无需重启。

### 各模式的行为

| 请求 | clean | block | log | off |
|---|---|---|---|---|
| 干净 | 原样转发 | 原样转发 | 原样转发 | 原样转发 |
| 有启用类别的标记 | 清洗后转发 | 400，不转发 | 原样转发，记日志 | 原样转发 |
| 只有关掉类别的标记 | 原样转发，记日志 | 原样转发，记日志 | 原样转发，记日志 | 原样转发 |
| 日期行外有 U+02BC / U+02B9 | 原样转发，记日志 | 原样转发，记日志 | 原样转发，记日志 | 原样转发 |
| `cc_version` 等于目标，指纹校验通过 | 原样转发 | 原样转发 | 原样转发 | 原样转发 |
| `cc_version` 与目标不同，指纹校验通过 | 改写成目标版本 | 改写成目标版本 | 原样转发，记日志 | 原样转发 |
| `cc_version` 指纹校验不通过（含版本等于目标时） | 删 billing 头和 `metadata.user_id`，CPA cloak | 同左 | 原样转发，记日志 | 原样转发 |

`block` 模式返回的响应体是 Anthropic 错误格式：

```json
{"type":"error","error":{"type":"invalid_request_error","message":"请求已被 cpa-stego-guard 拦截，未转发上游。检测到 Claude Code 隐写标记：日期行 Today 后的撇号是 U+2019（命中域名名单）。…请把 Claude Code 升级到 2.1.293 或更高版本后重试。 / Blocked by cpa-stego-guard: …"}}
```

## 日志

通过 CPA 的 host log 输出，只有命中类型和计数，不含提示词内容：

```text
[info ] cpa-stego-guard configured mode=clean hook=request.patch_body_before clean_quote_markers=true clean_region_marker=true cc_version=2.1.321
[debug] cpa-stego-guard aligned cc_version action=aligned mode=clean hook=request.patch_body_before cc_version=align cc_version_applied=true cc_version_from=2.1.330 cc_version_to=2.1.321 edits=1
[info ] cpa-stego-guard could not verify the cc_version fingerprint, removed the billing header and metadata.user_id so CLIProxyAPI cloaks the request action=fallback ... cc_version_reason=fingerprint_mismatch
[info ] cpa-stego-guard found steganographic markers action=cleaned mode=clean hook=request.patch_body_before date_lines=1 quote_u2019_domain=1 quote_u02bc_lab=0 quote_u02b9_both=0 region_slash_date=1 stray_u02bc=0 stray_u02b9=0 edits=3 ...
[warn ] cpa-stego-guard found steganographic markers action=blocked mode=block hook=request.intercept_before ...
```

CPA 的文本日志只打印它自己白名单里的字段，所以计数直接写在消息里。对齐是新版本客户端的常态，记在 debug 级；回退记在 info 级。日志里只有版本号，不含提示词。

## 局限

- **只认 Claude Code 的日期行格式。** 日期行必须在首条用户消息的 `<system-reminder>` 块内、位于行首，日期两个分隔符相同且月日合法。格式不同的变体不会被识别。
- **`clean` 模式按请求体形状判断协议。** `request.patch_body_before` 钩子只拿到请求体、没有协议信息，插件把“顶层是对象、有 `messages` 数组、没有 `contents` / `input`、没有 `developer` / `tool` / `function` 角色”的请求当作 Anthropic Messages。理论上，OpenAI Chat 格式里如果恰好带着 Claude Code 式的日期行也会被清洗，但实际只有转发 Claude Code 请求时才会出现，清掉同样无害。`block` 模式用的钩子带协议信息，只处理 `/v1/messages` 这一类请求。
- **`block` 模式更重一些。** 能拒绝请求的只有 `request.intercept_before`，CPA 会把请求体 JSON + Base64 编码后传给插件；`clean` / `log` 用的 `request.patch_body_before` 直接传原始字节。2 MB 的请求体扫描约 3 ms。
- **`cc_version` 回退依赖 CPA 的 cloak。** OAuth 订阅账号总会 cloak；API key 账号只有开启 claude-code-cli 指纹档或配置了 cloak 时才会。CPA 不 cloak 时，回退后的请求不带 billing 头转发。`count_tokens` 的识别不看 `metadata.user_id`，回退只删掉 billing 头（真 Claude Code 的 `count_tokens` 本来就不带 system）。
- **`cc_version` 只认 `x.y.z.<3 位小写十六进制>;`。** 其他写法一律回退。连续多个孤立高位代理项（`\ud800\ud800`）按 Node 的语义每个算一个 UTF-16 单元，与 CPA 自己的 JSON 解码略有不同，只影响这种极端输入的指纹。
- **不处理其他身份信息。** 系统提示词里的平台、系统版本、工作目录、git 状态、`CLAUDE.md` 内容、用户邮箱，以及请求头都不在本插件范围内；`metadata.user_id` 只在 `cc_version` 回退时整体删除，不做其他处理。
- **API key 账号的特殊情况。** 如果上游是 API key 且没有开启 CPA 的 claude-code-cli 指纹档，CPA 不重签 `cch`；此时若客户端自己带了 `cch`，清洗后会对不上。真 Claude Code 指向 CPA 时不发 `cch`，OAuth 订阅账号总会重签，这种情况通常不会出现。
- 只支持 CLIProxyAPI v8。插件不起 goroutine、不用定时器。

## 开发

```bash
go test -race ./...                                   # 单元测试
go test -run '^$' -fuzz FuzzScan -fuzztime 60s ./internal/stego
go test -run '^$' -fuzz FuzzAlignCCVersion -fuzztime 60s ./internal/stego
scripts/e2e.sh ../CLIProxyAPI                         # e2e（Linux，需要 root 以使用 unshare）
```

e2e 在私有的 mount + network 命名空间里运行：`api.anthropic.com` 被解析到本地的假上游（`scripts/fake_anthropic.py`，用临时 CA 签发证书，只有 CPA 进程信任），CPA 使用 Claude OAuth 形态的凭据，走与生产相同的 OAuth / `cch` 签名路径。脚本逐字节比对上游收到的请求体，命名空间外的网络和文件都不受影响。

## 许可

MIT，见 [LICENSE](LICENSE)。
