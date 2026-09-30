# cpa-plugin-codex-auto-reset

[CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) 插件：当 codex 订阅渠道的**周用量窗口**用尽、且账号持有 reset credit 时，自动消耗一张 credit 重置限额并清除 CPA 对该凭证的本地冷却，无需人工介入。

判定与流程的取舍见 `docs/adr/`，术语见 `CONTEXT.md`，实现决策见 `.scratch/auto-reset-on-weekly-exhaustion/decisions.md`。

## 行为

触发条件（全部满足）：

- 上游 429 且错误体 `type == "usage_limit_reached"`
- 窗口为周窗：`limit_window_minutes == 10080`，字段缺失时以 `resets_in_seconds > 18000`（或由 `resets_at` 推算）兜底
- 自然恢复在一天之外：`resets_in_seconds > 86400`
- 插件 `enabled` 为真，且凭证在 `include_credentials` 中

命中后：取凭证的 access token 与 account ID → 列 reset credit → 选 `expires_at` 最早且未过期的可用 credit → 带流程级幂等键 consume → **只有** `reset` / `already_redeemed` 视为成功 → 成功后清 CPA 冷却。

任何一步失败（无可用 credit、`nothing_to_reset`、`no_credit`、网络或 HTTP 错误、清冷却失败）都只记日志，回落 CPA 默认冷却行为，不补偿。唯一的重试是只读的 credit 列表：401/403 直接结束，其它失败流程内最多共试 3 次；consume 与清冷却从不重试。当前客户端请求照常返回错误，插件不做请求重放。

同凭证并发命中只跑一次流程；流程有定论后（credit 已消耗、无可用 credit、token 失效、consume 已发出）5 分钟内不再触发，临时故障不抑制（内存态，随 CPA 重启清空）。

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
      management_key: "<CPA management key>"  # 必需：缺失时插件不消耗任何 credit（见下）
      management_base_url: "http://127.0.0.1:8317"  # 默认值
      include_credentials:                    # 白名单：只有列出的凭证会被插件消耗 credit
        - "codex-user@example.com.json"       # auth 文件名
        - "<auth_index>"                      # 或运行时 auth index
```

### 升级必读（破坏性变更）

作用范围从旧的排除名单改为纳入名单 `include_credentials`，且默认**为空**。旧配置里的排除键已不再被识别，插件也不会对其做任何兼容。

默认不对任何凭证动作：升级后必须显式在 `include_credentials` 中列出要纳入的凭证，否则插件启用却不会消耗任何 credit。为提示这一状态，插件在 `enabled` 为真但 `include_credentials` 为空时会在加载/改配置阶段记一条 warn。

`management_key` 取自 CPA 的 `management.secret-key`；插件经本机 management API `POST /v0/management/reset-quota` 清冷却（本机访问不受 `management.allow-remote` 限制，但该 API 仍需 key 非空）。

**`management_key` 缺失时插件不做任何事**：清不掉冷却就消费 credit，等于白烧一张卡还让凭证被锁到旧的 reset 时间——比不装插件更糟。所以命中后若 key 为空，插件立即中止并在日志里记 warn，不发任何上游请求。

## 已知上限

- 上游 `chatgpt.com/backend-api` 的 reset credit 接口是**非官方接口**，契约可能随时变更；接口用法由多个开源项目在生产环境验证过。
- `host.http.do` 无超时可设，上游挂死会占住该凭证的 reset 流程（不影响 CPA 自身的 usage 队列）；代码中以 `ponytail:` 注释标注。
- 不刷新 access token：token 过期即视为一次失败，回落默认行为。
- 5 小时窗用尽不触发。
- 需要配置 `management_key`（CPA 的 `management.secret-key`），这是相对"只在 credit 快过期时兑换"类插件的额外运维负担——换来的是凭证冷却能被清掉、渠道立刻恢复可用。

## 日志

全部经 `host.log` 输出，`fields.plugin = "cpa-plugin-codex-auto-reset"`，含触发判定（负向决策为 debug 级；`window_source` / `resets_source` 标明窗口与重置时长取自哪个字段）、credit id、幂等键、结果码与清冷却结果，可事后审计 credit 消耗。
