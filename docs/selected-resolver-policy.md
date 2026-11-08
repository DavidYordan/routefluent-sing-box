# 选定解析策略的 native transport

本仓库只定义原生配置与执行行为。LineAsset 的 logical server、SNI/Host、query name 与策略选择由 RouteFluent 主仓库的 runtime owner 定义。

## DoH 组

`rf_resolver_group` 仅接收原生 DNS server 公共字段及 `primary` tag 数组。数组必须非空，tag 不为空、不重复、不指向自身；native Start 要求每个依赖存在且类型为 HTTPS 或 H3。

一次 Exchange 按声明顺序请求 primary。transport error 保留 resolver tag 与原始错误，并在同一次请求内尝试下一个 primary；所有 transport 失败时返回聚合错误。context 取消直接结束该请求。SERVFAIL、NXDOMAIN 等有效 DNS 回答原样返回，不据此重新解释其它域名或请求。

没有跨请求 healthy/unhealthy 状态、阈值、后台 probe、恢复计数、cooldown 或系统 DNS fallback。cache environment 只包含固定 mode 和 primary tags，不随请求结果改变。正常 DNS 缓存继续由上游客户端负责。

## 明确的系统解析

系统解析使用原生 `{"tag":"local-system","type":"local"}`，outbound 的 domain resolver 直接引用该 tag；不套私有 group，也不改写 TTL。未指定策略的默认值由调用方 owner 决定，native transport 不猜默认策略。

## 原生消费者

`sing-box check -c` 执行 pre-start，使缺失依赖和错误类型在 native 消费时返回精确错误。`fallback`、`fallback_enabled`、`mode`、probe 和健康阈值等退役字段均由严格 decoder 拒绝。

`rf_ingress_binding` 独立承担 LineAsset identity、显式 IPv4 和 query name。请求结果不改写 binding current、outbound、SNI 或 Host。地址切换的 cache identity 仍由 binding revision 承担。

直接回归位于 `patches/sing-box/dns/transport/rfgroup/`；配置样例位于 `testdata/`；`scripts/rf_dns_runtime_smoke.py` 使用本地 TLS DoH 与 HTTP consumer 验证解析、query-name 配方、DoH 错误和明确系统解析，不请求外部 DNS。
