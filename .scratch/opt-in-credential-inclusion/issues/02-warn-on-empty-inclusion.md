# 02: 注册期空名单 warn

**What to build:** 当 `enabled` 为真、但纳入名单过滤空串后为空时，插件在配置解析成功后经 `host.log` 记一条 warn，指明"已启用但未纳入任何凭证，插件不会消耗 credit"。该 warn 在 register 与 reconfigure 两条路径上都会出现（二者共用解析逻辑），且每次加载/改配置都会出现——它精确表达"当前配置下插件不动作"。`enabled: false` 时不记。

**Blocked by:** 01 (翻转为纳入语义)

**Status:** ready-for-agent

- [ ] `enabled` 为真且纳入名单（过滤空串后）为空 → 注册后恰好一条 `levelWarn`，文案说明未纳入任何凭证
- [ ] `enabled: false`（无论名单是否为空）→ 无此 warn
- [ ] 纳入名单非空 → 无此 warn
- [ ] reconfigure 路径与 register 行为一致（同样产出该 warn）
- [ ] 默认配置（无 YAML，`enabled` 缺省为真、名单为空）注册后即产出该 warn
