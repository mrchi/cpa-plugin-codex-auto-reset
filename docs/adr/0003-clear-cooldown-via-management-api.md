# 通过 management HTTP API 清除凭证冷却

## Status

Superseded by ADR-0006.

消耗 reset credit 成功后，必须清除 CPA 对该凭证的本地冷却，否则 CPA 仍按旧 429 的 reset 时间锁定凭证（上游限额已恢复也调度不到）。CPA 的 host 回调全集不含清冷却能力，唯一正规途径是本机 management HTTP API `POST /v0/management/reset-quota`，故插件配置需携带 management key，通过 `host.http.do` 调用。

## Considered Options

- **`host.auth.save` 副作用**：重写凭证可顺带清掉 auth 级冷却，但模型级冷却会被继承，行为脆弱且未文档化，排除。
