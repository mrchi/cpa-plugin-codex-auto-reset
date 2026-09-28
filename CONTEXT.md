# cpa-auto-reset

CLIProxyAPI 插件：codex 订阅渠道的周限额用尽且账号持有 reset credit 时，自动消耗一张 credit 重置限额并恢复该凭证可用。

## Language

**CLIProxyAPI (CPA)**:
托管本插件的 LLM API 代理（Go 编写），插件以 c-shared 动态库形式进程内加载。

**Codex 订阅渠道**:
CPA 中走 ChatGPT OAuth（Plus/Pro 账号）的上游渠道，请求发往 chatgpt.com 后端，按模型名路由到具体凭证。
_Avoid_: OpenAI API 渠道（api.openai.com 按量计费，与本插件无关）

**凭证 (Credential)**:
CPA 中一个 codex 账号的 OAuth 凭证（auth 文件），含 access token 与 account ID，是限额的承载单位。

**用量窗口 (Usage window)**:
OpenAI 对 codex 订阅的限额窗口，分 5 小时窗（`limit_window_minutes=300`）和周窗（`limit_window_minutes=10080`）两种，各自独立计数。

**限额用尽**:
上游返回 429 且错误 `type=usage_limit_reached`，错误体带 `resets_at`/`resets_in_seconds` 与窗口标识。

**Reset credit**:
OpenAI 官方发放给账号的限额重置额度（官方名 rate-limit reset credit），发放后 30 天过期；消耗一张会同时重置 5 小时窗和周窗。余额随账号，无需外部映射。
_Avoid_: 重置卡、卡密

**消耗 (Consume)**:
调用上游接口使用一张 reset credit；以客户端生成的幂等键防重复消耗，`already_redeemed` 视为成功，`nothing_to_reset`/`no_credit` 不消耗。

**冷却 (Cooldown)**:
CPA 在凭证请求失败后对该凭证的本地锁定，按上游给的 reset 时间到期才恢复调度；与上游实际限额状态无关，需显式清除。

**自然恢复**:
用量窗口到期后限额自动归零，不消耗 reset credit。
