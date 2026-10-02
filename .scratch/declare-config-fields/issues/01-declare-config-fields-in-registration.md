# 01: 在注册响应声明三个配置字段

**What to build:** 面板能列出本插件的三个可配置项。插件在 `plugin.register` 与 `plugin.reconfigure` 返回的 `metadata.ConfigFields` 中声明 `management_key`（string）、`management_base_url`（string）、`include_credentials`（array）；描述用中文，写清 `management_base_url` 的默认值、`management_key` "留空则不消耗任何 credit"、`include_credentials` 的元素含义（auth 文件名或 auth_index）。`enabled` 与 `priority` 由 host 注入，不声明。此改动只影响注册响应的 metadata，不改配置解析、判定或重置行为。

**Blocked by:** None (can start immediately)

**Status:** ready-for-review

- [x] 注册与改配置两条路径返回的 metadata 都含恰好三个配置字段，顺序为 `management_key`、`management_base_url`、`include_credentials`
- [x] 三者类型分别为 `string`、`string`、`array`，且不属于 enum（`EnumValues` 为空）
- [x] 每条字段的 `Description` 非空且为中文，包含上述各自语义
- [x] 不存在名为 `enabled` 或 `priority` 的配置字段
- [x] `schema_version` 仍为 `1`，`metadata` 必填四项与 `capabilities` 不变
- [x] 既有 `TestLifecycleReturnsRegistration` 扩展后覆盖以上断言，`go test ./...` 通过；测试不新增接缝，沿用 `handleMethod` + `decodeResult`
