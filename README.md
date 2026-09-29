# cpa-plugin-codex-auto-reset

[CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) 插件：当 codex 订阅渠道的**周用量窗口**用尽、且账号持有 reset credit 时，自动消耗一张 credit 重置限额并清除 CPA 对该凭证的本地冷却，无需人工介入。

判定与流程的取舍见 `docs/adr/`，术语见 `CONTEXT.md`，实现决策见 `.scratch/auto-reset-on-weekly-exhaustion/decisions.md`。

## 行为

触发条件（全部满足）：

- 上游 429 且错误体 `type == "usage_limit_reached"`
- 窗口为周窗：`limit_window_minutes == 10080`，字段缺失时以 `resets_in_seconds > 18000`（或由 `resets_at` 推算）兜底
- 自然恢复在一天之外：`resets_in_seconds > 86400`
- 插件 `enabled` 为真，且凭证不在 `exclude_credentials` 中

命中后：取凭证的 access token 与 account ID → 列 reset credit → 选 `expires_at` 最早且未过期的可用 credit → 带流程级幂等键 consume → **只有** `reset` / `already_redeemed` 视为成功 → 成功后清 CPA 冷却。

任何一步失败（无可用 credit、`nothing_to_reset`、`no_credit`、网络或 HTTP 错误、清冷却失败）都只记日志，回落 CPA 默认冷却行为，不重试、不补偿。当前客户端请求照常返回错误，插件不做请求重放。

同凭证并发命中只跑一次流程；成功后 5 分钟内不再触发（内存态，随 CPA 重启清空）。

## 构建

```bash
make build          # 产出 dist/cpa-plugin-codex-auto-reset.dylib（linux 为 .so）
```

产物放到 CPA 的 `plugins/`（或 `plugins/<goos>/<goarch>/`）目录，文件名即插件 id。

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
      exclude_credentials:                    # 手工管理的账号，插件不碰
        - "codex-user@example.com.json"       # auth 文件名
        - "<auth_index>"                      # 或运行时 auth index
```

`management_key` 取自 CPA 的 `management.secret-key`；插件经本机 management API `POST /v0/management/reset-quota` 清冷却（本机访问不受 `management.allow-remote` 限制，但该 API 仍需 key 非空）。

**`management_key` 缺失时插件不做任何事**：清不掉冷却就消费 credit，等于白烧一张卡还让凭证被锁到旧的 reset 时间——比不装插件更糟。所以命中后若 key 为空，插件立即中止并在日志里记 warn，不发任何上游请求。

## 已知上限

- 上游 `chatgpt.com/backend-api` 的 reset credit 接口是**非官方接口**，契约可能随时变更；接口用法由多个开源项目在生产环境验证过。
- `host.http.do` 无超时可设，上游挂死会占住该凭证的 reset 流程（不影响 CPA 自身的 usage 队列）；代码中以 `ponytail:` 注释标注。
- 不刷新 access token：token 过期即视为一次失败，回落默认行为。
- 5 小时窗用尽不触发。
- 需要配置 `management_key`（CPA 的 `management.secret-key`），这是相对"只在 credit 快过期时兑换"类插件的额外运维负担——换来的是凭证冷却能被清掉、渠道立刻恢复可用。

## 日志

全部经 `host.log` 输出，`fields.plugin = "cpa-plugin-codex-auto-reset"`，含触发判定（负向决策为 debug 级）、credit id、幂等键、结果码与清冷却结果，可事后审计 credit 消耗。
