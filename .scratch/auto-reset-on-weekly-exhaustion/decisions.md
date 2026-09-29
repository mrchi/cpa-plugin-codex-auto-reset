# 实现决策（01/02/03 共用的技术契约）

两份探索报告（基于本地 CLIProxyAPI v8 源码与上游接口一手资料整理，含 `文件:行号` 证据，未入库）：
- CPA 插件 ABI 与 host 回调（`01-cpa-plugin-abi.md`）
- 上游 reset-credit 接口（`02-upstream-reset-credit-api.md`）

实现前先读这两份报告；本文件只记决策，不重复证据。**报告与 spec 冲突处以报告为准**（已在下文标注）。

## 仓库与构建

- D1 module 路径 `github.com/mrchi/cpa-plugin-codex-auto-reset`，单模块、单包 `main`，源码放仓库根目录，按关注点分文件（`main.go` ABI shim、`host.go` 接缝、`config.go`、`classify.go`、`reset.go`、`state.go`）。测试为同包 `*_test.go`。
- D2 依赖：`github.com/router-for-me/CLIProxyAPI/v8`（只用 `sdk/pluginabi` + `sdk/pluginapi`，v8.0.3）、`gopkg.in/yaml.v3`。**不写 replace**；SDK 已发布到 proxy。已实测：import 这两个包可正常 `-buildmode=c-shared` 构建。UUID v4 由 `crypto/rand` 手写（版本位 `raw[6]=(raw[6]&0x0f)|0x40`、变体位 `raw[8]=(raw[8]&0x3f)|0x80`），**不引入 `github.com/google/uuid`**——只用到唯一性，标准库够。
- D3 RPC 契约一律用 SDK 的 struct（`pluginapi.UsageRecord`、`Metadata`、`Capabilities` 无 JSON tag ⇒ PascalCase key；`pluginapi.HTTPResponse`、`HostAuthFileEntry` 有 tag ⇒ snake_case）。**不要手抄这些 struct**——字段名写错是静默失效。
- D4 cgo ABI shim 需手写，照 `~/Playground/CLIProxyAPI/examples/plugin/usage/go/main.go` 的 C typedef 与 envelope：导出 `cliproxy_plugin_init` / `cliproxyPluginCall` / `cliproxyPluginFree` / `cliproxyPluginShutdown`；`abi_version` 必须精确等于 1（`pluginabi.ABIVersion`）；响应 envelope `{"ok":true,"result":{...}}` / `{"ok":false,"error":{...}}`。
- D5 构建：`CGO_ENABLED=1 go build -trimpath -buildmode=c-shared -o dist/cpa-plugin-codex-auto-reset.dylib .`（darwin 后缀必须 `.dylib`；linux `.so`）。产物旁会多出 `.h`，加进 `.gitignore`。
- D6 方法分发只处理 `plugin.register`、`plugin.reconfigure`（与 register 同 schema）、`plugin.shutdown`、`usage.handle`，其余返回 `unknown_method`。

## 注册与配置

- D7 `capabilities` 只置 `usage_plugin: true`；`metadata` 的 `Name` / `Version` / `Author` / `GitHubRepository` 四项**都必填非空**，否则注册被拒（`Name=cpa-plugin-codex-auto-reset`、`Version=0.1.0`、`GitHubRepository=https://github.com/mrchi/cpa-plugin-codex-auto-reset`）。响应 `schema_version` 填 1（0 或缺失按 1 处理，不可大于 6）。
- D8 配置来源只有 register/reconfigure 请求的 `config_yaml`（base64 → `[]byte` → `yaml.Unmarshal`）。键：
  - `enabled` bool，host 会强制补齐该键（值取自 `plugins.configs.<id>.enabled`）；插件侧缺省视为 `true`
  - `exclude_credentials` []string（auth id / auth 文件名 / auth_index，任一匹配即排除）。匹配只用 `usage.handle` 记录自带的 `AuthID`、`path.Base(AuthID)` 与 `AuthIndex`，**不调 `host.auth.list`**：记录里的字段足够，多一次 host 往返只增加失败面。
  - `management_key` string（**硬前提**：为空则命中后立即中止、不消耗任何 credit，只记 warn。清不掉冷却就消费 credit 等于白烧一张卡——凭证仍被锁到旧的 reset 时间，比不装插件更糟，违反 story 16）
  - `management_base_url` string，默认 `http://127.0.0.1:8317`
- D9 配置解析失败不 panic（panic 会让插件被 host fuse），退回默认值并记日志。

## 观测与并发（ADR-0001 约束）

- D10 `usage.handle` 输入是 `pluginapi.UsageRecord`。codex 订阅凭证判定：`Provider == "codex"` && `AuthType == "oauth"`。失败判定：`Failed == true` && `Failure.StatusCode == 429` && `Failure.Body`（字符串，上游错误体原文）中 `error.type == "usage_limit_reached"`。
- D11 **`usage.handle` 必须立即返回**，重置流程放到独立 goroutine。理由：host 的 usage 派发是单 worker 队列，且 `host.http.do` 无超时字段、无 ctx deadline——在 handler 里同步做 HTTP 会阻塞 CPA 的整条 usage 队列。
- D24 触发时长的来源有两个：错误体的 `resets_in_seconds`（正整数优先）与 `resets_at`（RFC3339 字符串，错误体同时携带）。`resets_in_seconds` 缺失或非正数时用 `resets_at - 当前时间` 推算秒数；两者都取不到（或推算结果非正）才算"未知窗"不命中。**不能只认 `resets_in_seconds`**：缺失时整条记录不命中，失败模式是"功能静默永不生效"而非降级。窗口兜底判定不变（`limit_window_minutes` 缺失时 `>18000`），但输入用推算后的秒数。
- D12 并发控制用内存态 `map[authIndex]credentialState{inFlight bool, suppressedUntil time.Time}` + 一把 `sync.Mutex`，**不持久化**：
  - 收到命中信号时若 `inFlight` 或 `now < suppressedUntil` → 记 debug 日志后直接丢弃（不做阻塞等待，避免 goroutine 堆积）。两种丢弃各记各的日志文案，日志里能区分"流程进行中"与"刚重置过"。
  - 否则置 `inFlight`，起 goroutine；流程结束清 `inFlight`，成功时置 `suppressedUntil = now + 5m`
- D25 时间来源只有一个：`state.go` 的进程时钟（`now()` 读、`setClock()` 换）。debounce 抑制窗口、credit 过期判断与 `resets_at` 推算全部读它；测试用 `useFakeClock` 换成假时钟推进时间，不依赖真实时钟、也不真的等 5 分钟。时钟会被重置流程的 goroutine 读取（D11），所以换表加锁。
- D13 host 调用统一经一个接缝接口（ticket 01 建立），生产实现走 C 回调，测试用 fake。`host.log` 的 level 只认 `trace`/`info`/`warn`/`error`，其它值（含 `debug`）落到 debug 级别；`fields` 固定带 `{"plugin":"cpa-plugin-codex-auto-reset"}`（host 不自动补插件标识）。

## 上游调用（ticket 03）

- D14 base URL `https://chatgpt.com/backend-api`。凭证取自 `host.auth.get`（入参**只能是 `{"auth_index": ...}`**，即 `UsageRecord.AuthIndex`），其 `json` 字段是 auth 文件原文：扁平结构 `access_token` / `account_id` / `id_token`。`account_id` 缺失时依次回退 `chatgpt_account_id`、`id_token` 的 JWT claim `https://api.openai.com/auth` 下的 `chatgpt_account_id`；若值是 `email_` / `local_` 开头的占位值则**不发**该 header。
- D15 必需 header：`Authorization: Bearer <access_token>`、`ChatGPT-Account-ID: <account_id>`、POST 加 `Content-Type: application/json`，另带 `User-Agent: codex_cli_rs/<ver>` 与 `originator: codex_cli_rs`。官方不发 `OpenAI-Beta`，不要加。
- D16 `GET /wham/rate-limit-reset-credits` → 顶层 `credits[]`（元素主键是 **`id`**，**不是 `credit_id`**；另有 `status` / `expires_at`，`expires_at` 是 RFC3339 字符串或 `null`）。选 available 且 `expires_at` 最早者；`expires_at` 早于当前时间（或正好等于）的 credit **跳过**——消耗它只会换回一个 `no_credit`，让流程白白放弃旁边可用的 credit；`expires_at` 为 null 或解析失败视为永不过期排最后。解析必须容忍未知字段（真实响应字段多于官方 struct）。
- D17 `POST /wham/rate-limit-reset-credits/consume`，body `{"redeem_request_id":"<uuid v4>","credit_id":"<id>"}`（`credit_id` 是 **string**）。响应 `code` ∈ `reset` / `already_redeemed` / `nothing_to_reset` / `no_credit`。非 2xx 一律硬失败。`no_credit` / `nothing_to_reset` 是 **200 + code**，不是 4xx。
- D18 幂等键：一次流程内重试复用同一 `redeem_request_id`；新触发（过抑制期后）生成新的。（本实现每流程只发一次 consume，无内部重试，但幂等键必须由流程级生成，不得写在循环里。）
- D19 清冷却：`POST {management_base_url}/v0/management/reset-quota`，header `Authorization: Bearer <management_key>`，body `{"auth_index":"<authIndex>"}`（**是 auth_index 不是 auth id**），200 视为成功。仅在 consume 成功后调用；失败只记日志、不重试。

## 已知取舍（显式记录，不要"顺手修"）

- D20 `host.http.do` 无超时，本实现不引入 `host.http.operation_open`/`cancel`。上限：上游挂死会占住该凭证的 `inFlight`（单凭证，不阻塞 CPA 队列），升级路径是改用 operation/cancel 加 deadline。代码里以 `ponytail:` 注释标注。
- D21 不做 token 刷新（spec Out of Scope）：access token 过期就是一次失败，回落默认行为。
- D22 当前客户端请求不重试、不重放（spec 决策）：插件对 `usage.handle` 只回 `{}`。
- D23 `pluginapi.HTTPRequest` / `HTTPResponse` 无 JSON tag，直接 marshal 会得到 host 读不到的 PascalCase key。`host.http.do` 的请求必须用 `host.go` 里已有的手写 wire struct（`httpRequest`，snake_case）；响应方向 `HTTPResponse` 的 `StatusCode`/`Headers`/`Body` 恰好就是 PascalCase，可直接反序列化。不要"顺手统一"成 SDK struct。
