# 02: 文档与发版对齐

**What to build:** 让这次声明随一个新版本真正生效，并把用法写进 README。README 的"配置"一节注明这些键会在面板里以表单呈现；`main.go` 的 `pluginVersion` 与 `registry.json` 的 `version` 同步升到 `0.2.1`，使商店安装的用户拿到一致版本。

**Blocked by:** 01

**Status:** ready-for-agent

- [ ] README"配置"段补一句：这些配置键会在面板中渲染为表单
- [ ] `pluginVersion` 与 `registry.json` 的 `version` 均为 `0.2.1`，`TestRegistryMatchesPlugin` 通过
- [ ] `go test ./...` 全绿
