# 向管理面板声明插件配置字段

Status: ready-for-agent

## Problem Statement

我用 CPA 面板管理插件，但这个插件在注册响应里没有声明任何配置字段，面板不知道它有哪些可配置项。于是我只能手动去改 config yaml 才能设置 `management_key`、`management_base_url`、`include_credentials`——容易写错、不直观，也发现不了"装好了却什么配置都没有"这种状态。尤其 `management_key` 是硬前提：它缺失时插件命中后不消耗任何 credit，只记一条 warn；如果面板能直接列出这个键，运维一眼就能看出没填。

## Solution

在插件注册响应的 `metadata.ConfigFields` 里声明三个私有配置键，面板据此渲染表单：`management_key`（string）、`management_base_url`（string）、`include_credentials`（array）。字段描述用中文，并写清各自语义（默认值、"留空即不消耗 credit"、列表元素含义）。`enabled` 由 host 注入、面板另有启用开关，不作为插件自有字段声明。声明只影响面板呈现，不改任何判定或重置行为。

## User Stories

1. 作为 CPA 运维者，我希望面板能列出这个插件的所有可配置项，这样我不必去翻 README 才知道能配什么。
2. 作为 CPA 运维者，我希望 `management_key` 出现在面板表单里，这样我可以直接粘贴 CPA 的 `management.secret-key`，不必手改 yaml。
3. 作为 CPA 运维者，我希望 `management_key` 的描述写明"留空则插件不消耗任何 credit"，这样我一眼就明白这个键是硬前提。
4. 作为 CPA 运维者，我希望 `management_base_url` 出现在面板里，这样需要指向非默认地址时能在界面上改。
5. 作为 CPA 运维者，我希望 `management_base_url` 的描述写明默认值是 `http://127.0.0.1:8317`，这样我不填也知道会发生什么。
6. 作为 CPA 运维者，我希望 `include_credentials` 出现在面板里，这样我能看到当前哪些凭证被纳入了自动重置。
7. 作为 CPA 运维者，我希望 `include_credentials` 的描述说明元素是 auth 文件名或 auth_index，这样即使面板渲染不完美我也知道该填什么。
8. 作为 CPA 运维者，我希望配置字段的描述是中文，与我读的其余文档语言一致。
9. 作为 CPA 运维者，我希望面板不重复声明 `enabled`，因为它由 host 注入、面板已有独立开关，重复只会造成两处相互矛盾。
10. 作为 CPA 运维者，我希望这次改动只是把配置项"显式声明"出来，不改变任何配置键的名字、类型或取值语义，这样我现有的 yaml 配置继续有效。
11. 作为 CPA 运维者，我希望面板若把 `management_base_url` 回写成空串，插件仍落到默认地址，这样清空输入不会让清冷却指向错误地址。
12. 作为 CPA 运维者，我希望 `enabled` 为真但 `include_credentials` 为空时那条 warn 依旧存在（不因面板填写而消失），这样"装好了却不动作"仍能被日志发现。
13. 作为 CPA 运维者，我希望从商店安装的插件装上后，面板立即能看到这些字段，这样可视化配置对新用户默认可用。
14. 作为 CPA 运维者，我希望这次声明随一个新版本发布、`pluginVersion` 与商店清单 `version` 同步，这样商店安装的用户拿到一致版本。
15. 作为未来维护者，我希望有一条 ADR 说明"为什么 `management_key` 以明文 string 声明、`include_credentials` 用 array 声明"，这样不会有人把这当成疏漏去"修复"。
16. 作为未来维护者，我希望术语表 `CONTEXT.md` 不被这次改动污染，因为它只收业务领域词汇，而"配置字段"是实现/接口细节。
17. 作为未来维护者，我希望有测试盯住注册响应里这三个字段的存在与类型，这样以后重构注册逻辑时不会悄悄把面板配置项弄丢。
18. 作为未来维护者，我希望不新增任何测试接缝，沿用既有的生命周期注册接缝，这样测试面不扩大。

## Implementation Decisions

- **声明载体与位置**：在插件注册响应（`plugin.register` 与 `plugin.reconfigure` 共用）的 `metadata.ConfigFields` 中填充字段列表。这是 host 提供的唯一可视化配置声明机制；商店清单 `registry.json` 没有任何配置槽位，因此声明只能放在运行时注册里，且必须在 `metadata` 的必填四项之外新增。
- **声明的字段**（顺序即面板呈现顺序）：
  - `management_key`，`string`，描述含"CPA 的 `management.secret-key`；留空则插件不消耗任何 credit"。
  - `management_base_url`，`string`，描述含默认值 `http://127.0.0.1:8317`。
  - `include_credentials`，`array`，描述含"元素为 auth 文件名或 auth_index；仅列出的凭证会被消耗 credit"。
- **不声明的键**：`enabled` 与 `priority` 由 host 注入，非插件自有键，不进入 `ConfigFields`；`Logo` 本次不做。
- **类型能力约束**：`ConfigField` 结构只有 `Name`/`Type`/`EnumValues`/`Description` 四项，没有 `defaultValue`、没有 `required`、没有 secret/password 类型、没有 array 的元素类型。默认值与"留空即禁用"的语义只能写进 `Description`。`EnumValues` 本次不使用（无枚举键）。
- **刻意取舍（见 ADR-0005）**：`management_key` 以明文 `string` 声明，接受面板明文显示与回写——该值本就明文存在于 config yaml，声明不新增暴露面，但换掉了手改 yaml 的门槛；`include_credentials` 以 `array` 声明，尽管无元素类型、面板渲染不可控，可见性优先，元素含义靠描述兜底。
- **schema_version 不变**：仍为 `1`。`ConfigFields` 在 `schema_version: 1` 下即被 host 识别（官方示例佐证），不递增。
- **配置解析零改动**：`pluginConfig` 结构、`decodeConfig`、`parseConfig`、`classify` 与重置流程全部不动。面板回写的空 `management_base_url` 已由现有逻辑归一为默认地址，回写的空 `management_key` 已由现有逻辑触发"不消耗 credit + warn"。
- **版本**：`main.go` 的 `pluginVersion` 与 `registry.json` 的 `version` 同步升到 `0.2.1`（`TestRegistryMatchesPlugin` 盯住两者一致）。声明只随新二进制生效，必须发版。
- **文档**：新增 `docs/adr/0005-declare-config-fields-for-management-panel.md`（已写）；`CONTEXT.md` 不动（无新领域术语）；README 的"配置"一节补一句"这些键会在面板里以表单呈现"。
- **不新增模块、不新增 host capability**：改动限于注册响应的 metadata 构造，以及既有测试与文档。

## Testing Decisions

- **好的测试只断言外部行为**：给定生命周期请求，断言插件返回的注册响应里 `metadata` 的内容（有哪些字段、类型为何、描述非空）。不断言内部状态、不测私有构造函数以外的实现细节，沿用既有 table-driven、无框架、无 fixture 风格。
- **单一接缝：生命周期注册响应**（沿用，不新增）。经 `handleMethod(fakeHost, plugin.register | plugin.reconfigure, payload)` 取回 envelope，`decodeResult` 解出 `metadata`。这是本次唯一受影响的接缝。
- **受测模块**：注册响应的 metadata 构造。既有测试 `TestLifecycleReturnsRegistration`（`main_test.go`）已在读 `metadata` 并断言必填四项，本次在同一用例内扩到也断言 `ConfigFields`。
- **断言内容**：注册（register 与 reconfigure 两条路径）都返回恰好三个配置字段，名称为 `management_key` / `management_base_url` / `include_credentials`，类型分别为 `string` / `string` / `array`，每条 `Description` 非空；且不存在名为 `enabled` 或 `priority` 的字段。
- **不需要动的接缝**：`config_test.go` / `classify_test.go` / `reset_test.go` 使用的 host-callback 接缝（`registerConfig`、`sendUsage`、`logsWithMessage`、`hitRecord`）完全不受影响，因为本次不改配置解析与判定行为；这些用例保持原样。
- **先验**：`main_test.go` 的 `TestLifecycleReturnsRegistration` 即先验，沿用其 helper（`decodeResult`、`lifecyclePayload`、`newFakeHost`）。

## Out of Scope

- `Logo`（面板图标）等其它 `Metadata` 字段。
- 商店清单 `registry.json` 侧的配置声明（host 的清单结构无此槽位）。
- 面板本身的渲染、表单校验、保存语义与 i18n（属于 CPA 前端，不在本仓库）。
- 为 secret 值引入遮罩，或给 `ConfigField` 增加 `defaultValue`/`required`/array 元素类型（属于 host SDK 的结构变更）。
- 配置键的新增、重命名、类型或取值语义的任何改动；判定、重置、冷却清除、去抖行为的任何改动。
- 声明 `enabled`/`priority`。

## Further Notes

- 本次决策与两处取舍见 `docs/adr/0005-declare-config-fields-for-management-panel.md`；术语表 `CONTEXT.md` 按定位保持不变。
- 声明只在装上新二进制后生效，因此 `0.2.1` 是必需的发布动作，而不是可选的文档同步。
- 残余风险：面板保存配置时对未声明键的处理未知（可能整体覆盖）。本插件读取的全部键都已被声明，且 host 注入的 `enabled` 由面板单独开关管理，故无未声明键被覆盖的风险。
- `include_credentials` 无元素类型，面板可能渲染成裸 JSON 框或纯文本；这是 host 结构的限制，已在 ADR 中记为已知代价。
