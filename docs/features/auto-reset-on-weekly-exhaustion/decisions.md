# 实现决策（01/02/03 共用的技术契约）

本文件只记决策（代码注释以 `D<n>` 引用），不重复证据。原始的两份探索报告（CPA 插件 ABI 与 host 回调、上游 reset-credit 接口，含 `文件:行号` 证据）只存在于本地、未入库，其结论已并入下列各条。

配置键与清冷却方式随后续 ADR 变更（ADR-0004 改纳入名单、ADR-0006 改 host 回调清冷却），相应条目已就地更新，以本文件与 ADR 为准。

## 仓库与构建

- D1 module 路径 `github.com/mrchi/cpa-plugin-codex-auto-reset`，单模块、单包 `main`，源码放仓库根目录，按关注点分文件（`main.go` ABI shim、`host.go` 接缝、`config.go`、`classify.go`、`reset.go`、`state.go`）。测试为同包 `*_test.go`。
- D2 依赖：`github.com/router-for-me/CLIProxyAPI/v8`（只用 `sdk/pluginabi` + `sdk/pluginapi`，当前 v8.0.21）、`gopkg.in/yaml.v3`。**不写 replace**；SDK 已发布到 proxy。已实测：import 这两个包可正常 `-buildmode=c-shared` 构建。UUID v4 由 `crypto/rand` 手写（版本位 `raw[6]=(raw[6]&0x0f)|0x40`、变体位 `raw[8]=(raw[8]&0x3f)|0x80`），**不引入 `github.com/google/uuid`**——只用到唯一性，标准库够。
- D3 RPC 契约一律用 SDK 的 struct（`pluginapi.UsageRecord`、`Metadata`、`Capabilities` 无 JSON tag ⇒ PascalCase key；`pluginapi.HTTPResponse`、`HostAuthFileEntry` 有 tag ⇒ snake_case）。**不要手抄这些 struct**——字段名写错是静默失效。
- D4 cgo ABI shim 需手写，照 CPA 仓库 `examples/plugin/usage/go/main.go` 的 C typedef 与 envelope：导出 `cliproxy_plugin_init` / `cliproxyPluginCall` / `cliproxyPluginFree` / `cliproxyPluginShutdown`；`abi_version` 必须精确等于 1（`pluginabi.ABIVersion`）；响应 envelope `{"ok":true,"result":{...}}` / `{"ok":false,"error":{...}}`。
- D5 构建：`CGO_ENABLED=1 go build -trimpath -buildmode=c-shared -o dist/cpa-plugin-codex-auto-reset.dylib .`（darwin 后缀必须 `.dylib`；linux `.so`）。产物旁会多出 `.h`，加进 `.gitignore`。
- D6 方法分发只处理 `plugin.register`、`plugin.reconfigure`（与 register 同 schema）、`plugin.shutdown`、`usage.handle`，其余返回 `unknown_method`。

## 注册与配置

- D7 `capabilities` 只置 `usage_plugin: true`；`metadata` 的 `Name` / `Version` / `Author` / `GitHubRepository` 四项**都必填非空**，否则注册被拒（`Name=cpa-plugin-codex-auto-reset`、`GitHubRepository=https://github.com/mrchi/cpa-plugin-codex-auto-reset`）。响应 `schema_version` 填 1（0 或缺失按 1 处理，不可大于 6）。
- D8 配置来源只有 register/reconfigure 请求的 `config_yaml`（base64 → `[]byte` → `yaml.Unmarshal`）。键：
  - `enabled` bool，host 会强制补齐该键（值取自 `plugins.configs.<id>.enabled`）；插件侧缺省视为 `true`
  - `include_credentials` []string（auth id / auth 文件名 / auth_index，任一匹配即纳入）。匹配只用 `usage.handle` 记录自带的 `AuthID`、`path.Base(AuthID)` 与 `AuthIndex`，**不调 `host.auth.list`**：记录里的字段足够，多一次 host 往返只增加失败面。默认空 ⇒ 不对任何凭证动作并记 warn（ADR-0004）。
  - `management_key` / `management_base_url` 曾用于调 management API 清冷却，**0.3.0 起已移除**（ADR-0006）；旧配置里的这两个键不再被读取，也没有旧 HTTP 回退。
- D9 配置解析失败不 panic（panic 会让插件被 host fuse），退回默认值并记日志。

## 观测与并发（ADR-0001 约束）

- D10 `usage.handle` 输入是 `pluginapi.UsageRecord`。codex 订阅凭证判定：`Provider == "codex"` && `AuthType == "oauth"`。失败判定：`Failed == true` && `Failure.StatusCode == 429` && `Failure.Body`（字符串，上游错误体原文）中 `error.type == "usage_limit_reached"`。
- D11 **`usage.handle` 必须立即返回**，重置流程放到独立 goroutine。理由：host 的 usage 派发是单 worker 队列，且 `host.http.do` 无超时字段、无 ctx deadline——在 handler 里同步做 HTTP 会阻塞 CPA 的整条 usage 队列。
- D24 触发时长的来源有两个：错误体的 `resets_in_seconds`（正整数优先）与 `resets_at`。**`resets_at` 的真实格式是整数 Unix 秒**（openai/codex 的 wire struct 建模为 `i64`，CPA 自己用 `quota.Get("resets_at").Int()` 读，fixture 形如 `"resets_at":1700000300`）；旧报文里出现过 RFC3339 字符串，所以两种都接受，非正数与解析失败一律视为没有该字段。`resets_in_seconds` 缺失或非正数时用 `resets_at - 当前时间` 推算秒数；两者都取不到（或推算结果非正）才算"未知窗"不命中。**不能只认 `resets_in_seconds`**：缺失时整条记录不命中，失败模式是"功能静默永不生效"而非降级。窗口兜底判定不变（`limit_window_minutes` 缺失时 `>18000`），但输入用推算后的秒数。`limit_window_minutes` 字段本身已被一手来源证实存在（openai/codex `api_bridge.rs` 的 `UsageErrorBody`，单位为分钟），`== 10080` 的用法成立。
- D12 并发控制用内存态 `map[authIndex]credentialState{inFlight bool, suppressedUntil time.Time}` + 一把 `sync.Mutex`，**不持久化**：
  - 收到命中信号时若 `inFlight` 或 `now < suppressedUntil` → 记 debug 日志后直接丢弃（不做阻塞等待，避免 goroutine 堆积）。两种丢弃各记各的日志文案，日志里能区分"流程进行中"与"刚重置过"。
  - 否则置 `inFlight`，起 goroutine；流程结束清 `inFlight`，流程**有定论**时置 `suppressedUntil = now + 5m`。有定论 = 5 分钟内重来结果也不会变：credit 已消耗、无可用 credit、token 失效（无 access token 或列表 401/403）、列表重试耗尽、consume 已发出（不论返回什么——结果未知时重来可能烧第二张卡）。只有凭证自身读不到（`host.auth.get` 失败）与 UUID 生成失败不抑制，下一条信号可重试。
    - 列表重试耗尽计入定论，是因为上游列表接口是唯一会连续打三次外呼的一步：若它长期故障而流程不加抑制，每条持续到达的 429 记录都会再触发三次外呼，永不收敛。抑制 5 分钟的代价对比以"天"计的周窗可以忽略。
  - `AuthIndex` 为空的记录直接 warn 丢弃：`host.auth.get` 只认 auth index，也不能让这类记录共用一个 debounce 条目。
- D25 时间来源只有一个：`state.go` 的进程时钟（`now()` 读、`setClock()` 换）。debounce 抑制窗口、credit 过期判断与 `resets_at` 推算全部读它；测试用 `useFakeClock` 换成假时钟推进时间，不依赖真实时钟、也不真的等 5 分钟。时钟会被重置流程的 goroutine 读取（D11），所以换表加锁。
- D26 卸载前必须等在跑的流程结束：`cliproxyPluginShutdown` 等在跑的流程全部结束（上限 `shutdownGrace` = 5s）再返回。host 侧顺序是「调插件 shutdown 钩子 → `free` 掉传给插件的 `cliproxy_host_api` → `dlclose`」（pluginhost loader_unix.go `Shutdown`），而 host 的调用守卫只统计**它自己发起**的调用（client_guard.go 的 `calls`），看不见插件自己起的 goroutine——钩子返回时若还有流程在跑，该流程恢复执行时读到的是已 free 的 host api 与已 unmap 的代码段，整个 CPA 进程崩溃。触发场景：热重载插件（运维按升级说明替换 `.so`）、面板停用插件、CPA 重启。
  - `resetFlows.Add(1)` 写在起 goroutine 之前，且一定发生在某个 host→plugin 回调内部；host 的守卫会先排空这些回调再调 shutdown 钩子，所以 `Wait()` 开始前计数必然已经加上，WaitGroup 的「Add 必须早于 Wait」约束成立。
  - 上限是上限而非计划：流程通常停在两次 host 调用之间，立刻结束。例外是卡在挂死的 `host.http.do` 里（D20 无 cancel），超过上限 host 照样卸载——这个等待只能收窄窗口，堵死它要等 D20 的 operation/cancel 升级。
- D13 host 调用统一经一个接缝接口（ticket 01 建立），生产实现走 C 回调，测试用 fake。`host.log` 的 level 只认 `trace`/`info`/`warn`/`error`，其它值（含 `debug`）落到 debug 级别；`fields` 固定带 `{"plugin":"cpa-plugin-codex-auto-reset"}`（host 不自动补插件标识）。

## 上游调用（ticket 03）

- D14 base URL `https://chatgpt.com/backend-api`。凭证取自 `host.auth.get`（入参**只能是 `{"auth_index": ...}`**，即 `UsageRecord.AuthIndex`），其 `json` 字段是 auth 文件原文：扁平结构 `access_token` / `account_id` / `id_token`。`account_id` 缺失时依次回退 `chatgpt_account_id`、`id_token` 的 JWT claim `https://api.openai.com/auth` 下的 `chatgpt_account_id`；若值是 `email_` / `local_` 开头的占位值则**不发**该 header。
- D15 必需 header：`Authorization: Bearer <access_token>`、`ChatGPT-Account-ID: <account_id>`、POST 加 `Content-Type: application/json`，另带 `User-Agent: codex_cli_rs/<ver>` 与 `originator: codex_cli_rs`。官方不发 `OpenAI-Beta`，不要加。
- D16 `GET /wham/rate-limit-reset-credits` → 顶层 `credits[]`（元素主键是 **`id`**，**不是 `credit_id`**；另有 `status` / `expires_at`，`expires_at` 是 RFC3339 字符串或 `null`）。选 available 且 `expires_at` 最早者；`expires_at` 早于当前时间（或正好等于）的 credit **跳过**——消耗它只会换回一个 `no_credit`，让流程白白放弃旁边可用的 credit；`expires_at` 为 null 或解析失败视为永不过期排最后。解析必须容忍未知字段（真实响应字段多于官方 struct）。
- D17 `POST /wham/rate-limit-reset-credits/consume`，body `{"redeem_request_id":"<uuid v4>","credit_id":"<id>"}`（`credit_id` 是 **string**）。响应 `code` ∈ `reset` / `already_redeemed` / `nothing_to_reset` / `no_credit`。非 2xx 一律硬失败。`no_credit` / `nothing_to_reset` 是 **200 + code**，不是 4xx。
- D18 幂等键：一次流程内重试复用同一 `redeem_request_id`；新触发（过抑制期后）生成新的。（本实现每流程只发一次 consume，无内部重试，但幂等键必须由流程级生成，不得写在循环里。只读的 credits 列表可以重试：401/403 视为 token 失效直接结束，其它失败最多共试 3 次，间隔 1s。）
- D19 清冷却：`host.routing.reset_cooldown`，body `{"auth_index":"<authIndex>"}`（**是 auth_index 不是 auth id**），无 error 返回视为成功（host 侧带 `WithSkipPersist`，不重写 auth 文件）。仅在 consume 返回 `reset`/`already_redeemed` 后调用；失败只记日志、不重试、不补偿。需 CPA ≥ v8.0.12（该版本起才有这个回调；v8.0.11 及更早没有）。0.3.0 前是 `POST {management_base_url}/v0/management/reset-quota`（200 视为成功），见 ADR-0003 / ADR-0006。

## 已知取舍（显式记录，不要"顺手修"）

- D20 `host.http.do` 无超时，本实现不引入 `host.http.operation_open`/`cancel`。上限：上游挂死会占住该凭证的 `inFlight`（单凭证，不阻塞 CPA 队列），升级路径是改用 operation/cancel 加 deadline。代码里以 `ponytail:` 注释标注。
- D21 不做 token 刷新（spec Out of Scope）：access token 过期就是一次失败，回落默认行为。
- D22 当前客户端请求不重试、不重放（spec 决策）：插件对 `usage.handle` 只回 `{}`。
- D23 `pluginapi.HTTPRequest` 无 JSON tag，`host.http.do` 的请求用 `host.go` 里手写的 wire struct（`httpRequest`，snake_case），与 host 侧 `rpcHostHTTPRequest` 声明的键名逐字对齐；**保留手写 struct，不要"顺手统一"成 SDK struct**。理由是让 wire 契约显式可见——注意这不是正确性问题：`encoding/json` 匹配 tag 时大小写不敏感，PascalCase 的 key 也能落进 `json:"method"` 这类字段，所以「直接 marshal SDK struct 会失效」的说法不成立。响应方向 `HTTPResponse` 的 `StatusCode`/`Headers`/`Body` 可直接反序列化。
