# 仅周限额用尽且自然恢复超过一天时才自动消耗 reset credit

自动触发条件限定为：429 `usage_limit_reached` 且窗口为周窗（`limit_window_minutes==10080`，缺失时以 `resets_in_seconds>18000` 兜底）且 `resets_in_seconds>86400`。5 小时窗用尽不触发（几小时自然恢复，烧卡不值）；周窗一天内自然恢复也不触发。在此之上不设烧卡张数上限：reset credit 30 天过期，不用即废。

## Consequences

- CPA 不解析 `limit_window_minutes` 字段，插件需自行解析 429 错误体。
- 消耗一张 credit 会同时重置 5 小时窗和周窗（上游行为），即使触发条件只看周窗。
- 任何一步失败（无卡、网络错误、接口报错）不做任何重试或补偿，回落 CPA 默认冷却/换号行为，只记日志。
