# 用 usage.handle 观测上游 429

插件需要感知 codex 上游的 429 限额错误，但 CPA 的 response 链 hook 只在成功响应上触发，错误路径不经过任何拦截器；且凭证级重试发生在 handler 之下，CPA 换凭证重试成功时中间 429 对插件完全不可见。决定用 `usage.handle` 作为唯一观测点：它按尝试粒度上报失败记录，携带 AuthID/AuthIndex/状态码/上游错误体，中间 429 也能看到。

## Considered Options

- **response.intercept_after 等 response 链 hook**：只在成功响应触发，错误完全不可见，排除。
- **executor.execute 接管上游调用**：能第一手拿到 429 和 token，但需自行重实现请求翻译、流式、token 刷新、用量上报，成本与维护面不成比例，排除。

## Consequences

`usage.handle` 按尝试而非按请求触发，同一客户端请求可能产生多条记录，触发逻辑必须自带 per-credential 防抖。
