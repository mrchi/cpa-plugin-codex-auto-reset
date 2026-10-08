# cpa-plugin-codex-auto-reset

[CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) 插件：当 codex 订阅渠道的**周用量窗口**用尽、且账号持有 reset credit 时，自动消耗一张 credit 重置限额并清除 CPA 对该凭证的本地冷却，无需人工介入。

判定与流程的取舍见 `docs/adr/`，术语见 `CONTEXT.md`，需求与实现决策见 `docs/features/auto-reset-on-weekly-exhaustion/`（`spec.md` 的用户故事编号、`decisions.md` 的 `D<n>` 编号被代码注释直接引用）。

## 行为

触发条件（全部满足）：

- 凭证为 codex 订阅渠道：记录里 `provider == "codex"` 且 `auth_type == "oauth"`，其它渠道的记录直接忽略
- 上游 429（记录的失败状态码为 429）且错误体 `type == "usage_limit_reached"`（该字段在顶层或 `error` 嵌套里均可）
- 窗口为周窗：`limit_window_minutes == 10080`，字段缺失时以 `resets_in_seconds > 18000`（或由 `resets_at` 推算）兜底
- 自然恢复在一天之外：解析出的剩余秒数 > 86400（`resets_in_seconds` 优先，缺失时由 `resets_at` 推算）
- 插件 `enabled` 为真，且凭证在 `include_credentials` 中

命中后：取凭证的 access token 与 account ID → 列 reset credit → 选 `status == available`、id 非空、尚未过期且 `expires_at` 最早的 credit（`expires_at` 为空视为永不过期，排在有到期日的之后）→ 带流程级幂等键 consume → **只有** `reset` / `already_redeemed` 视为成功 → 成功后清 CPA 冷却（经 host 回调 `host.routing.reset_cooldown`，进程内 RPC，不重写 auth 文件）。

任何一步失败（无可用 credit、`nothing_to_reset`、`no_credit`、网络或 HTTP 错误、清冷却失败）都只记日志，回落 CPA 默认冷却行为，不补偿。唯一的重试是只读的 credit 列表：401/403 直接结束，其它失败流程内最多共试 3 次；consume 与清冷却从不重试。当前客户端请求照常返回错误，插件不做请求重放。

同凭证并发命中只跑一次流程；流程有定论后（credit 已消耗、无可用 credit、token 失效、列表重试耗尽、consume 已尝试）5 分钟内不再触发，只有凭证本身读不到时不抑制（内存态，随 CPA 重启清空）。

## 安装

### 从 CPA 面板的插件商店安装

仓库根目录的 `registry.json` 是一份 CPA 插件商店清单。把它作为自定义来源加进 CPA 配置并重启：

```yaml
plugins:
  store-sources:
    - "https://raw.githubusercontent.com/mrchi/cpa-plugin-codex-auto-reset/main/registry.json"
```

重启后在面板的插件商店里即可一键安装。商店从本仓库的 GitHub Release 取产物（`install.type` 缺省即 `github-release`），所以版本由 release tag 决定，装完的库落在 `<plugins.dir>/<goos>/<goarch>/`。

> 商店按 CPA 运行时的 `${GOOS}_${GOARCH}` 选产物，目前发版只产出 `linux_amd64` 与 `linux_arm64`。在其它平台（如本机 macOS）用面板安装会因找不到对应产物而失败，请改用下方的手动构建。

### 手动安装

```bash
make build          # 产出 dist/cpa-plugin-codex-auto-reset.dylib（linux 为 .so）
```

产物放到 CPA 的 `plugins/`（或 `plugins/<goos>/<goarch>/`）目录，文件名即插件 id。

## 构建与发版

`make release` 产出一个平台的商店产物：`dist/cpa-plugin-codex-auto-reset_<version>_<goos>_<goarch>.zip`（库在压缩包根目录，名为 `cpa-plugin-codex-auto-reset.<ext>`）加一行 `checksums.txt`，并自检压缩包布局符合商店的安装契约——布局不对会在本地直接失败，而不是在面板安装时才暴露。跨平台时逐个调用，产物累积到同一个 `dist/`：

```bash
make release GOOS=linux GOARCH=amd64
make release GOOS=linux GOARCH=arm64 CC=aarch64-linux-gnu-gcc
```

发版走 `.github/workflows/release.yml`：推 `v<version>` tag 触发，CI 校验 tag 与 `main.go` 的 `pluginVersion` 一致、跑测试、构建两个 linux 架构、创建 release 并附上 `dist/*.zip` 与 `checksums.txt`。改版本时同时改 `main.go` 的 `pluginVersion` 与 `registry.json` 的 `version`（`TestRegistryMatchesPlugin` 会盯住两者一致）。

## 配置

`plugins.configs.cpa-plugin-codex-auto-reset` 下的键：

```yaml
plugins:
  enabled: true
  dir: "plugins"
  configs:
    cpa-plugin-codex-auto-reset:
      enabled: true                          # 总开关；缺失视为 true
      include_credentials:                    # 白名单：只有列出的凭证会被插件消耗 credit
        - "codex-user@example.com.json"       # auth 文件名
        - "<auth_id>"                         # 或记录的 auth id
        - "<auth_index>"                      # 或运行时 auth index
```

插件在注册响应里声明了这个私有键，管理面板会据此把它渲染成表单，可不开 yaml 直接填写；`enabled` 由 host 注入、面板另有启用开关，不作为插件字段声明。

需要 **CPA v8.0.12 或更高**：清冷却经 host 回调 `host.routing.reset_cooldown` 完成，不再需要 management key 或本机 management HTTP 服务。

### 升级必读（破坏性变更）

**0.3.0**：清冷却从 management HTTP API `POST /v0/management/reset-quota` 改为 host 回调。`management_key`、`management_base_url` 两个键不再被读取，旧配置里可以直接删掉（留着也无害，插件不读未知键）。

**0.2.0**：作用范围从旧的排除名单改为纳入名单 `include_credentials`，且默认**为空**。旧配置里的排除键已不再被识别，插件也不会对其做任何兼容。

默认不对任何凭证动作：升级后必须显式在 `include_credentials` 中列出要纳入的凭证，否则插件启用却不会消耗任何 credit。为提示这一状态，插件在 `enabled` 为真但 `include_credentials` 为空（或只含空白项，空白项从不匹配任何凭证）时会在加载/改配置阶段记一条 warn。

记录不带 `auth_index` 时中止并记 warn：读不了凭证也清不了冷却，继续消耗只会白烧一张 credit。

## 已知上限

- 上游 `chatgpt.com/backend-api` 的 reset credit 接口是**非官方接口**，契约可能随时变更；接口用法由多个开源项目在生产环境验证过。
- `host.http.do` 无超时可设，上游挂死会占住该凭证的 reset 流程（不影响 CPA 自身的 usage 队列）；代码中以 `ponytail:` 注释标注。
- 不刷新 access token：token 过期即视为一次失败，回落默认行为。
- 5 小时窗用尽不触发。
- 最低支持 CPA v8.0.12：更低的 host 上没有 `host.routing.reset_cooldown` 这个回调，调用被 host 报为不支持的 callback（`host_call_failed`），插件不做运行时版本探测、也没有旧 HTTP 回退，表现为 credit 已消耗但清冷却失败（日志记 `credential cooldown clear failed`），凭证要等旧的 reset 时间才恢复调度。
- 清冷却只清 CPA 对该凭证的**本地冷却状态**，不改变上游实际限额。
- 卸载或重启时插件最多等 5 秒等在跑的 reset 流程结束。host 收回 host 回调并卸载本库时不会等插件自己起的 goroutine，所以在跑的流程必须先结束；卡在挂死的上游请求里的流程等不到（见上一条），这种情况下卸载仍会中断它。

## 日志

全部经 `host.log` 输出，`fields.plugin = "cpa-plugin-codex-auto-reset"`，含触发判定（负向决策为 debug 级；`window_source` / `resets_source` 标明窗口与重置时长取自哪个字段）、credit id、幂等键、结果码与清冷却结果，可事后审计 credit 消耗。
