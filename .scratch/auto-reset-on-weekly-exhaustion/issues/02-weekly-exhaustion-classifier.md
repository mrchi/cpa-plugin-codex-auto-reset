# 02: 周限额用尽判定

**What to build:** 插件注册 usage.handle 能力，收到失败记录时完成全部判定逻辑并只记录日志、不执行任何重置动作：识别 codex 凭证的 429 usage_limit_reached；按 limit_window_minutes 区分周窗与 5 小时窗（字段缺失时以 resets_in_seconds>18000 兜底为周窗）；应用一天阈值（resets_in_seconds>86400 才算命中）、enabled 开关与 exclude_credentials 排除列表；非 codex 凭证与非 429 失败完全忽略。

**Blocked by:** 01（插件骨架与配置）

**Status:** ready-for-review

- [x] 429 + type=usage_limit_reached + 周窗 + resets_in_seconds>86400 的未排除凭证判定为命中并写日志
- [x] limit_window_minutes 缺失时 resets_in_seconds>18000 兜底判为周窗
- [x] 5 小时窗用尽、一天内自然恢复、enabled=false、凭证在排除列表中四种情况均不命中，且各有日志
- [x] 非 codex 凭证、非 429、非 usage_limit_reached 的记录被静默忽略
- [x] 以上行为均有经 fake host 接缝的外部行为测试

**Status note:** 本 ticket 不触发任何重置动作，判定结果只体现在日志。
