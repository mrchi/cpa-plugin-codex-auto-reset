# 01: 翻转为纳入语义

**What to build:** 插件对凭证的作用范围从排除名单改为纳入名单。配置键 `include_credentials`（默认空）列出唯一会被消耗 reset credit 的凭证；未列出的凭证即使周限额用尽，插件只记一条 debug 级决策日志、不发起任何上游请求。`enabled` 全局开关仍优先于纳入判定。匹配沿用原有三标识精确匹配（auth 全路径 ID、auth 文件名、运行时 auth index），空串条目永不匹配，因此空名单不会意外纳入全部。相关函数与常量随语义改名（`isExcluded`→`isIncluded`，`reasonExcluded`→`reasonNotIncluded`）。

**Blocked by:** None (can start immediately)

**Status:** ready-for-agent

- [ ] `pluginConfig` 的 `exclude_credentials` 字段与 YAML 键替换为 `include_credentials`；`defaultConfig` 不再为它设默认值（空即默认）
- [ ] 判定闸门在 `!Enabled` 之后、窗口判定之前按"未纳入即不动作"拦截，报告 `reasonNotIncluded`；`enabled: false` 时仍优先报告 `reasonDisabled`
- [ ] 纳入匹配命中 `UsageRecord.AuthID`、`path.Base(AuthID)`、`AuthIndex` 三者之一；空串/纯空白条目跳过；匹配不调用 `host.auth.list`
- [ ] 未纳入的记录不调用 `host.auth.get`、不调用 `host.http.do`
- [ ] 引入共享的"命中配置"测试 fixture（`enabled: true` + `management_key` + 纳入命中凭证标识），并替换既有测试中所有依赖"默认全开"的裸 `enabled: true` 配置；原排除用例反转为纳入语义
- [ ] `registerConfig`/`sendUsage`/`logsWithMessage`/`hitRecord` 等既有 helper 保持不变，全部测试沿用
- [ ] `go test ./...` 全绿；判定顺序（`enabled` → 纳入 → 窗口）、credit 选择、幂等键、冷却清除、去抖等既有行为不变
