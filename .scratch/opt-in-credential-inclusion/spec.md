# 逐凭证纳入的 reset credit 作用范围

Status: ready-for-agent

## Problem Statement

我用 CPA 跑 codex 订阅渠道，装了这个插件来自动兑换 reset credit。但现有配置是"排除名单"（`exclude_credentials`）：没有列出的凭证一律在插件的管辖范围内。于是任何一次配置疏漏——新加的账号、改名后的 auth 文件、忘了同步的名单——都会让插件在那些账号上周限额用尽时**自动消耗 reset credit**。reset credit 是不可逆、稀缺、30 天到期的资源，被误消耗无法撤销。我要的是：**没被我显式点名的凭证，插件一律不碰**。

## Solution

把逐凭证的作用范围从排除名单改成纳入名单：配置键 `include_credentials` 列出**唯一**会被插件消耗 reset credit 的凭证，默认空——即默认不作用任何凭证。插件在 `enabled` 为真但名单为空时，于加载/改配置阶段记一条 warn，避免"装好了却静默不动作"这种难以排查的失败。

## User Stories

1. 作为 CPA 运维者，我希望逐凭证的作用范围是**纳入名单**而非排除名单，这样只有我显式授权的账号会被消耗 reset credit。
2. 作为 CPA 运维者，我希望纳入名单默认为空，这样在我不配置任何东西时插件不会消耗任何 credit。
3. 作为 CPA 运维者，我希望纳入名单能按 auth 文件名匹配，这样我可以用最容易辨认的标识配置。
4. 作为 CPA 运维者，我希望纳入名单能按 auth 全路径 ID 匹配，这样与其它工具的配置保持一致。
5. 作为 CPA 运维者，我希望纳入名单能按运行时 auth index 匹配，这样在不知道文件名时也能指定。
6. 作为 CPA 运维者，我希望纳入名单中的空串条目不匹配任何凭证，这样手滑写出的空行不会意外把凭证纳入。
7. 作为 CPA 运维者，我希望缺失或为空的纳入名单等于"不纳入任何凭证"，而不是"纳入全部"，这样失败方向永远是不动作。
8. 作为 CPA 运维者，我希望纳入名单的匹配沿用原有的三标识精确匹配，不引入通配或域名后缀，这样"未列出即不动作"的语义保持无歧义。
9. 作为 CPA 运维者，我希望在 `enabled` 为真但纳入名单为空时，注册/改配置阶段就收到一条 warn，这样我立刻知道当前配置下插件不会动作。
10. 作为 CPA 运维者，我希望该 warn 在每次插件加载和每次改配置后都会出现，这样它始终反映"当前配置下插件不动作"这一事实。
11. 作为 CPA 运维者，我希望 `enabled: false` 时不再输出上面那条 warn，这样关掉插件时日志是干净的。
12. 作为 CPA 运维者，我希望全局开关 `enabled` 仍然生效且优先级更高，这样我能一次性停掉整个插件。
13. 作为 CPA 运维者，我希望凭证未被纳入时的决策日志明确指出原因是"未纳入"，这样审计时能区分"没纳入"与"窗口不符"等其它不触发原因。
14. 作为 CPA 运维者，我希望未纳入的凭证在判定阶段就被拦下、不发起任何上游请求，这样未授权账号绝不会被消耗 credit。
15. 作为 CPA 运维者，我希望纳入名单之外的凭证在限额用尽时完全按 CPA 默认行为回落，这样插件对它们等同于不存在。
16. 作为 CPA 运维者，我希望非 codex、非 oauth、非 429、非 `usage_limit_reached` 的记录仍被完全忽略，这样纳入名单的改动不影响既有过滤。
17. 作为 CPA 运维者，我希望周窗口判定、一天守卫、credit 选择、幂等键、冷却清除、去抖等既有行为完全不变，这样这次改动只动作用范围这一个维度。
18. 作为 CPA 运维者，我希望旧的 `exclude_credentials` 被彻底移除，不做兼容、不做过渡期提示，这样不存在两种"未列出"含义相反的语义共存。
19. 作为 CPA 运维者，我希望 README 明确写出这是**破坏性变更**（升级后需显式列出纳入凭证，否则插件不再动作），这样我升级前就知道要改配置。
20. 作为 CPA 运维者，我希望这次改动随下一次 minor 版本发布，且 `pluginVersion` 与商店清单版本同步，这样商店安装的用户拿到一致版本。
21. 作为 CPA 运维者，我希望新加一个账号时它不会自动落入插件管辖，这样我可以先观察其用量，再决定是否纳入。
22. 作为未来维护者，我希望有一条 ADR 说明"为什么默认不对任何凭证动作"，这样不会把"enabled 为真却什么都不干"当成 bug 去"修复"。

## Implementation Decisions

- **配置契约**：`pluginConfig` 中的 `exclude_credentials`（yaml: `exclude_credentials`）替换为 `include_credentials`（yaml: `include_credentials`）。语义为"纳入名单"，零值（缺失/空）表示不纳入任何凭证。`defaultConfig` 的 `enabled=true` 与 `management_base_url` 默认值保持不变；`include_credentials` 无默认值，因为其默认就是零值。
- **匹配语义**：沿用原有的三标识精确匹配，反转判定方向。匹配对象为 `UsageRecord` 的 `AuthID`、`path.Base(AuthID)`（auth 文件名）与 `AuthIndex`。空串或纯空白的 entry 跳过（永不匹配），因此空名单不会意外纳入全部。匹配只看 usage record 本身，不调用 `host.auth.list`。不引入通配或后缀匹配。
- **函数与常量重命名**：`isExcluded` → `isIncluded`（返回值语义反转）；`reasonExcluded` → `reasonNotIncluded`，日志文案改为 "credential not included in auto-reset: ignoring exhaustion signal"，级别保持 debug。
- **判定顺序**：`classify` 的 gate 顺序保持为 `!Enabled` → 纳入判定 → 窗口判定。即 `enabled: false` 时报告 `reasonDisabled`（优先于未纳入）；未纳入时报告 `reasonNotIncluded`，且不进入后续窗口判定、不发起任何流程。
- **注册期 warn**：`parseConfig` 在配置解析成功后、`enabled` 为真且纳入名单过滤空串后为空时，经 `host.log` 记一条 `levelWarn`，说明"已启用但未纳入任何凭证，插件不会消耗 credit"。该 warn 在 register 与 reconfigure 两条路径上都会出现（二者共用解析逻辑）。
- **不做兼容**：`exclude_credentials` 的键与字段一并移除，不解析、不检测、不做废弃提示。理由：目前尚无已安装用户，无需背负兼容包袱；两种语义共存时"未列出"的含义相反，无法解释也无法审计（见 ADR-0004）。
- **文档同步**：README 的配置示例与触发条件段改为 `include_credentials`，并新增"升级必读"的破坏性变更说明；`CONTEXT.md` 已新增术语**纳入凭证 (Included credential)**；决策记入 `docs/adr/0004-opt-in-credential-inclusion.md`。
- **发版**：随下一次 minor 发布，`main.go` 的 `pluginVersion` 与 `registry.json` 的 `version` 同步（`TestRegistryMatchesPlugin` 已盯住两者一致）。
- **不新增模块、不新增 host capability**：改动限于配置结构体、判定函数与其常量、以及既有测试与文档。

## Testing Decisions

- **好的测试只断言外部行为**：给定一条 usage record 与脚本化的 host 响应，断言"发生了哪些 host 调用、顺序如何、记了什么日志"。不断言内部状态、不测私有函数。沿用既有的 table-driven、无框架、无 fixture 风格。
- **单一接缝：host-callback 边界**（沿用，不新增）。配置经 `registerConfig` 注入，判定经 `sendUsage` 注入 usage record，通过 fake host 观察 `host.auth.get` / `host.http.do` / `host.log`。
- **受测模块**：配置解析与注册期 warn（`enabled`×名单是否为空的组合）；判定的纳入闸门（三标识命中、空条目、空名单、`enabled: false` 优先、未纳入不发请求）。
- **既有测试的改动范围（复核后确认，比初看大）**：翻转默认语义会让所有"依赖默认全开"的用例失效。需要在测试里引入一个共享的"命中配置"——`enabled: true` + `management_key` + 纳入名单列出命中凭证标识——并替换掉现有散落的 `"enabled: true\n"`。具体已知点：
  - 重置流程测试的共享配置（现为 `enabled: true` + `management_key`）必须补上纳入条目，否则其全部 `driveHit` 用例不再命中；`TestMissingManagementKeyConsumesNothing` 用的裸 `enabled: true` 同理。
  - 分类测试表格中所有期望 `reasonHit` 且配置为 `"enabled: true\n"` 的用例（约 20 个）需改用命中配置；`window_source`/`resets_source` 那张表同样。
  - 分类测试中原"排除"四个用例与"空条目不排除"用例反转为"纳入"语义：命中条目 → 未纳入时反而不动作（期望 `reasonNotIncluded`），空条目 → 仍不纳入（期望 `reasonNotIncluded`）。
  - 注册测试：默认配置（无 YAML）现在会同时产出空名单 warn，且 record 判为 `reasonNotIncluded`；`TestRegisterWithoutConfigKeepsTheDefaults` 需改写为断言这条 warn 与"不命中/不消耗"。
  - 失效配置测试现断言"恰好一条 warn"，翻转后默认配置路径会多出空名单 warn，需改为断言两条或改用带纳入条目的配置。
  - 生命周期路由测试注册 `enabled: true`（空名单）会新增一条 warn，需确认其未断言"无日志"，否则一并调整。
- **断言取舍**：不要在每个用例都断言那条 warn 的存在与否；只在专测它的用例里断言。其余用例配置里带上纳入条目，避免 warn 干扰对决策行的断言。
- **先验**：仓库既有的 `config_test.go` / `classify_test.go` / `reset_test.go` 即先验，本次沿用其 helper（`registerConfig`、`sendUsage`、`logsWithMessage`、`hitRecord`）。

## Out of Scope

- `exclude_credentials` 的任何向后兼容：不保留旧键、不做废弃检测、不做过渡期双语义。
- 通配、正则、域名后缀等模糊匹配。
- `enabled` 语义、窗口判定、一天守卫、credit 选择、幂等键、冷却清除、去抖的任何改动。
- 纳入名单之外的每账号额度策略（如 burn-rate 上限）。
- 运行时校验纳入条目是否对应真实存在的凭证（插件是响应式的，不持有凭证清单）。

## Further Notes

- 默认配置（无 YAML）现在会在加载时立即产出空名单 warn——这是**有意**的：它精确表达"当前配置下插件不会动作"，且 `enabled` 默认仍为 true，所以这个 warn 是新装用户的正常首条日志。
- 这次翻转的理由与被否方案见 `docs/adr/0004-opt-in-credential-inclusion.md`；术语见 `CONTEXT.md` 的**纳入凭证 (Included credential)**。
- 失败方向的不对称是决策核心：opt-out 下漏配会变现为不可逆的 credit 损失；opt-in 下漏配只是"插件没帮上忙"。
