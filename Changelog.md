# Changelog

## v0.1.0

首个版本。2.1.293 实测已没有日期行隐写代码（2.1.197–2.1.292 未经确认）。

### 功能

- 清洗 Claude Code 2.1.91–2.1.196 写在首条用户消息 `<system-reminder>` 日期行里的隐写标记：`Today` 后面的 U+2019 / U+02BC / U+02B9 改回 U+0027，斜杠日期 `YYYY/MM/DD` 改回 `YYYY-MM-DD`。
- 四种模式：`clean`（默认，清洗后转发）、`block`（返回 400，不转发上游）、`log`（只记日志）、`off`。
- 两个开关：`clean_quote_markers`（撇号标记）、`clean_region_marker`（斜杠日期）；关掉的一类只记日志。
- 日期行以外出现的 U+02BC / U+02B9 只记日志，不改用户正文。
- 只做字节级最小替换，不重新序列化 JSON；干净请求的请求体字节不变，结果确定、幂等。
- 日志通过 CPA host log 输出命中类型和计数，不含提示词内容。
- `clean` / `log` / `off` 使用 `request.patch_body_before`（原始字节、原地拼接），`block` 使用 `request.intercept_before`（唯一能拒绝请求的钩子）；改配置后 CPA 热重载即切换钩子。
- `cc_version` 配置项（默认空，不内置版本号）：把 Claude Code billing 头里的 `cc_version` 统一成 CPA 实际发出的 UA 版本，并按 Claude Code 的算法重算 3 位指纹；`cch` 由 CPA 重签。版本已等于目标的 billing 头同样校验指纹，一致才原样放行。原指纹校验不通过（包括版本已等于目标但指纹被改过）或格式认不出时，删掉 billing 头和 `metadata.user_id`，让 CPA 按普通 API 客户端处理（cloak 并生成自己的 billing 头）。`clean` / `block` 生效，`log` 只记日志。留空时注册记警告。
- 只支持 CLIProxyAPI v8。
