# 通过 host 回调 reset_cooldown 清除冷却

弃用 `/v0/management` 前缀后，插件清冷却的途径从本机 management HTTP API 迁移到 CPA 的 host 回调 `host.routing.reset_cooldown`（v8.0.12 起），随之删除 `management_key`/`management_base_url` 配置，最小兼容版本升到 v8.0.12。ADR-0003 的前提——「host 回调全集不含清冷却能力」——自 v8.0.12 起失效，本决定取代 ADR-0003。

## Considered Options

- **`/v8/management/routing/cooldown/reset`**：与旧 `POST /v0/management/reset-quota` 绑定同一 handler，改动最小（一个常量），仍走 `host.http.do`。但保留 `management_key` 这份运维负担，且仍依赖 management API 的可用性与 secret 配置；脱离弃用面而不解决本质，排除。
- **保持 `/v0/management/reset-quota`**：仍可用，但整条 `/v0/management` 前缀已被 CPA 标记 deprecated、不再维护，未来可能被删，不符合「适配最新版本」。
- **`host.auth.save` 副作用**：ADR-0003 已排除（模型级冷却被继承、行为脆弱未文档化）。

## Consequences

- 最小兼容版本 v8.0.12；低于该版本的 host 上没有该回调，调用被 host 报为不支持的 callback（`host_call_failed`），现成失败路径将其记为「清冷却失败」并回落默认冷却，不做运行时版本探测。
- 已在 CPA v8.0.21 的 SDK 与 `internal/pluginhost` 实现上核对请求/响应结构与 `WithSkipPersist` 行为；v8.0.11 及更早的 SDK 无此常量。
- 破坏性配置变更：`management_key`、`management_base_url` 移除，ADR-0005 声明的「三个私有键」减为 `include_credentials` 一个。
- 清冷却改为进程内 RPC，附带 `WithSkipPersist`（不重写 auth/token 文件），成功判定由「HTTP 200」变为「无 error 返回」。
