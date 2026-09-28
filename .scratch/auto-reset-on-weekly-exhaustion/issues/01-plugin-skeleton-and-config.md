# 01: 插件骨架与配置

**What to build:** 一个可编译为 c-shared 动态库并被 CPA 加载的插件骨架：导出插件 ABI 入口，完成 register/shutdown 生命周期，解析插件配置（enabled 开关、exclude_credentials 排除列表、management_key、management_base_url 含默认值），日志经 host.log 输出。同时建立测试接缝：所有 host 回调（auth.get、auth.list、http.do、log）的 fake 实现，http.do 按 URL 分发到脚本化响应。

**Blocked by:** None (can start immediately)

**Status:** ready-for-review

- [x] 编译产物为 c-shared 动态库，能被 CPA 从 plugins 目录加载并完成 register/shutdown
- [x] 配置项 enabled / exclude_credentials / management_key / management_base_url 全部可解析，缺失时有文档化的默认值或明确报错
- [x] 插件日志经 host.log 输出，带插件标识
- [x] fake host 回调接缝就位，后续 ticket 的测试无需真 CPA 进程
