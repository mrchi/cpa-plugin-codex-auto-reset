# 03: 自动重置与冷却清除

**What to build:** 判定命中后执行完整重置流程：经 host.auth.get 取凭证的 access token 与 account ID；GET /wham/rate-limit-reset-credits 确认余额并选 expires_at 最早的 available credit；POST consume 携带新生成的 UUID v4 幂等键与显式 credit_id（流程内重试复用同一幂等键）；reset/already_redeemed 视为成功，随后经 host.http.do 调本机 management API 清该凭证冷却；nothing_to_reset/no_credit/HTTP 错误视为失败。任何失败静默回落 CPA 默认行为只记日志。并发触发由 per-credential 互斥锁串行化，重置成功后同凭证 5 分钟内不再触发。当前失败请求不重试，照常返回错误给客户端。

**Blocked by:** 02（周限额用尽判定）

**Status:** ready-for-review

- [x] 命中且有可用 credit 时：选 expires_at 最早的可用 credit 完成 consume，随后清冷却，全过程日志可审计（`TestResetFlowConsumesEarliestCreditAndClearsCooldown`、`TestResetCreditSelection`、`TestResetCredentialHeaderFallbacks`）
- [x] 幂等键在流程级生成一次，不写在任何重试循环里（每流程只发一次 consume，D18）；already_redeemed 按成功处理并继续清冷却（`TestIdempotencyKeyIsGeneratedPerFlow`、`TestResetConsumeOutcomes`）
- [x] 无可用 credit（含只有已过期 credit 的情形）、nothing_to_reset、no_credit、网络/HTTP 错误时：不消耗、不清冷却、只记日志，CPA 默认冷却不受影响（`TestResetCreditSelection`、`TestResetConsumeOutcomes`、`TestResetListingFailuresAbortTheFlow`、`TestResetWithoutACredentialAborts`）
- [x] consume 成功但清冷却失败时记日志，不重试（`TestCooldownClearFailureIsLoggedAndNeverRetried`）
- [x] 未配置 management_key 时命中即中止、不消耗任何 credit（`TestMissingManagementKeyConsumesNothing`）
- [x] 并发命中同凭证只执行一次完整流程；成功后 5 分钟内同凭证不再触发，窗口过后恢复可触发（`TestConcurrentSignalsStartOneFlow`、`TestUsageHandleDispatchesOnceAndReturnsImmediately`、`TestResetSuppressionBlocksRetriggerWithinFiveMinutes`）
- [x] 当前请求的错误照常透传给客户端，插件不做请求重放：`usage.handle` 只回 `{}`（每个用例都断言）且不重放客户端请求（`TestUsageHandleDispatchesOnceAndReturnsImmediately`）
- [x] 以上行为均有经 fake host 接缝的外部行为测试：全部用例经 `handleMethod(host, "plugin.register"|"usage.handle", payload)` 进入，断言 host 调用序列、请求体与日志；时间相关行为由假时钟驱动（`useFakeClock`），不依赖真实时钟
