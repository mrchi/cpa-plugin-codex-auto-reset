# 03: README 破坏性变更迁移说明与发版同步

**What to build:** README 的配置示例、触发条件段与相关说明改用 `include_credentials`（含逐账号纳入的语义与默认空），并新增一段"升级必读"：默认不再作用于任何凭证，升级后必须显式列出纳入凭证，否则插件启用却不动作。发版时把 `main.go` 的 `pluginVersion` 与 `registry.json` 的 `version` 同步到同一个新版本号。

**Blocked by:** 01 (翻转为纳入语义)

**Status:** ready-for-agent

- [ ] README 配置示例用 `include_credentials`，示例条目同时展示 auth 文件名与 auth index 两种写法
- [ ] README 触发条件段的"凭证不在 `exclude_credentials` 中"改为"凭证在 `include_credentials` 中"
- [ ] README 新增"升级必读/破坏性变更"说明：默认不纳入任何凭证，需显式配置
- [ ] README 全文无 `exclude_credentials` 残留
- [ ] 发版时 `pluginVersion` 与 `registry.json` 的 `version` 同步（`TestRegistryMatchesPlugin` 通过）
