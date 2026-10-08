# 向管理面板声明插件配置字段

## Status

Amended by ADR-0006：键集从三个减为 `include_credentials` 一个，本 ADR 关于声明载体的取舍不变。

插件的私有配置此前只能手改 config yaml，面板对其一无所知。改为在注册响应的 `metadata.ConfigFields` 里声明私有键，面板据此渲染表单。声明载体只能是这个运行时字段：商店清单 `registry.json` 没有任何配置槽位，`enabled`/`priority` 由 host 注入、面板另有启用开关，都不是插件自有键。字段描述用中文，与仓库面向用户文档一致。

两处刻意偏离直觉的取舍：

- **`management_key` 声明为 `string` 明文**（该键已由 ADR-0006 移除，此处保留原始取舍记录）。CPA 的类型系统没有 secret/password，声明它意味着面板明文显示并回写该值。之所以仍要声明：该值本来就以明文存在 config yaml，声明只是把手改 yaml 换成面板填写，不新增暴露面；不声明的代价是保留手工编辑门槛，与"方便配置"的目标相悖。
- **`include_credentials` 声明为 `array` 而类型无元素描述**。`ConfigField` 结构不给 array 定义元素类型，面板渲染成什么样不可控（可能只是裸 JSON 框）。仍声明：这个键是插件的核心作用范围，可见性优先于渲染完美，元素含义（auth 文件名或 auth_index）写进 `Description` 兜底。

## Consequences

`ConfigFields` 只出现在注册响应里，必须发一个新版本、装上新二进制才生效。结构里没有 `defaultValue`/`required`，默认值与"留空即禁用"的语义只能写进 `Description`。
