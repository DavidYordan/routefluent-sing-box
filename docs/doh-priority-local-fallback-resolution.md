# DoH 优先与本机 DNS 兜底解析设计

日期：2026-07-11
状态：已在开发分支实现，目标发布版本 `v1.13.12-routefluent-anytls-client.7`

## 背景

RouteFluent 生成的 sing-box 配置中，代理 outbound 的 `server` 可能是域名。此前的严格做法倾向于由 RouteFluent 在服务端预解析为 IP 后下发，但这会带来两个问题：

1. 一些订阅或线路商家会提供更适合该线路的 DoH 解析入口，终端应优先使用它来解析该商家的代理域名。
2. 终端环境可能临时无法访问 DoH。如果完全禁止本机 DNS，可能导致本来可用的线路无法建立连接。

因此 sing-box 侧需要补一层受控解析能力：**优先使用线路或商家携带的 DoH；当该解析组内所有 DoH 均不可用时，允许临时 fallback 到终端本机 DNS；DoH 恢复后必须自动优先切回 DoH。**

一份配置可能同时包含多个商家的线路：

- 商家 A 携带 DoH A。
- 商家 B 携带 DoH B。
- 商家 C 不携带 DoH。

这些线路不能共享一个粗暴的全局 DNS fallback 状态。商家 A 的 DoH 故障不能让商家 B 的线路改用本机 DNS；商家 C 没有 DoH 时也不能被误记为“DoH 故障 fallback”。

## 目标

1. outbound `server` 域名解析优先使用该线路所属解析组的 DoH。
2. 当解析组内所有 DoH 均不可用时，允许临时使用本机 DNS。
3. 本机 DNS 只作为 DoH 不可用期间的临时兜底；DoH 恢复后，新解析请求应自动优先回到 DoH。
4. 解析状态按线路商家、订阅源或明确分组隔离，不能全局串扰。
5. 同一 sing-box 配置允许混合存在多个 DoH 解析组、多个本机 DNS 兜底组，以及无 DoH 的 local-only 线路。
6. 行为必须可审计：能看到本次解析使用了哪个解析器、为什么 fallback、何时恢复。
7. 不能照搬 Clash 式宽松逻辑，不能把 DNS fallback 扩大成 outbound 自动换线、route fallback 或数据面放行。

## 非目标

1. 不实现代理线路自动故障转移。
2. 不改变 route、outbound、selector 或 urltest 的选择逻辑。
3. 不处理 LAN 终端业务 DNS 的准入策略。
4. 不实现 fake-ip、透明代理 DNS 劫持或订阅解析兼容层。
5. 不把 RouteFluent 服务端预解析能力删除。服务端预解析仍作为配置生成策略之一，用于不适合终端解析或需要强审计的场景。

## 现有 sing-box 能力与缺口

sing-box `v1.13.12` 已有基础构件：

- `dns.servers` 支持 `https` 类型 DoH server。
- `dns.servers` 支持 `local` 类型本机解析。
- outbound `dialer.domain_resolver.server` 可以指定某个 DNS server 来解析 outbound 的 `server` 域名。

这些能力不足以直接表达本需求：

- 只能把 outbound 绑定到一个 DNS server，不能表达“多个 DoH 优先，全部不可用后才 local fallback”。
- 没有按商家或线路组隔离的 DoH 健康状态。
- 没有 DoH 恢复后自动优先切回 DoH 的明确机制。
- local fallback 产生的缓存结果可能压住已恢复的 DoH。

因此需要在自构建 sing-box 中增加一个窄范围 DNS transport 包装器，而不是改动 outbound 路由主逻辑。

## 建议配置模型

新增 DNS server 类型：`routefluent_resolver_group`。

示例：

```json
{
  "dns": {
    "servers": [
      {
        "tag": "local-system",
        "type": "local"
      },
      {
        "tag": "vendor-a-doh-1",
        "type": "https",
        "server": "dns.vendor-a.example",
        "server_port": 443,
        "path": "/dns-query"
      },
      {
        "tag": "vendor-b-doh-1",
        "type": "https",
        "server": "dns.vendor-b.example",
        "server_port": 443,
        "path": "/dns-query"
      },
      {
        "tag": "rf-resolver-vendor-a",
        "type": "routefluent_resolver_group",
        "primary": ["vendor-a-doh-1"],
        "fallback": "local-system",
        "fallback_enabled": true,
        "probe_domains": ["www.gstatic.com", "cloudflare.com"],
        "failure_threshold": 2,
        "recovery_success_threshold": 2,
        "probe_interval": "30s",
        "unhealthy_cooldown": "20s",
        "fallback_ttl_cap": "60s"
      },
      {
        "tag": "rf-resolver-vendor-b",
        "type": "routefluent_resolver_group",
        "primary": ["vendor-b-doh-1"],
        "fallback": "local-system",
        "fallback_enabled": true,
        "probe_domains": ["www.gstatic.com", "cloudflare.com"],
        "failure_threshold": 2,
        "recovery_success_threshold": 2,
        "probe_interval": "30s",
        "unhealthy_cooldown": "20s",
        "fallback_ttl_cap": "60s"
      },
      {
        "tag": "rf-resolver-no-doh-local",
        "type": "routefluent_resolver_group",
        "primary": [],
        "fallback": "local-system",
        "fallback_enabled": true,
        "mode": "local_only",
        "fallback_ttl_cap": "60s"
      }
    ]
  },
  "outbounds": [
    {
      "type": "anytls",
      "tag": "vendor-a-line-1",
      "server": "proxy-a.example",
      "server_port": 443,
      "domain_resolver": {
        "server": "rf-resolver-vendor-a",
        "strategy": "ipv4_only"
      }
    },
    {
      "type": "anytls",
      "tag": "vendor-b-line-1",
      "server": "proxy-b.example",
      "server_port": 443,
      "domain_resolver": {
        "server": "rf-resolver-vendor-b",
        "strategy": "ipv4_only"
      }
    },
    {
      "type": "anytls",
      "tag": "vendor-c-line-1",
      "server": "proxy-c.example",
      "server_port": 443,
      "domain_resolver": {
        "server": "rf-resolver-no-doh-local",
        "strategy": "ipv4_only"
      }
    }
  ]
}
```

`local_only` 与 DoH 故障 fallback 语义不同：

- `local_only` 表示该线路或商家没有提供 DoH，生成配置时已明确选择本机 DNS。
- DoH 解析组的 fallback 表示该组存在 DoH，但当前全部不可用，本机 DNS 只是临时兜底。

两者必须在日志和统计中分开呈现。

## 严格校验规则

1. `routefluent_resolver_group` 必须引用已存在的 DNS server tag。
2. `mode` 只允许省略或为 `local_only`。
3. 常规模式下 `primary` 至少包含一个 DoH server。
4. `local_only` 模式下 `primary` 必须为空，且必须配置 `fallback`。
5. 常规模式下 `fallback_enabled=true` 时，`fallback` 必须引用本机 local resolver，且 `probe_domains` 必须非空；否则 `sing-box check -c` 必须失败。
6. `fallback_enabled=false` 时，如果所有 DoH 不可用，必须返回解析错误，不能静默使用本机 DNS。
7. DoH endpoint 的 `server` 如果是域名，必须显式配置 bootstrap `domain_resolver`，不能隐式借用业务解析组。
8. outbound `server` 是域名时，生成器必须显式设置 `domain_resolver.server` 到对应 `routefluent_resolver_group`；需要 route 层域名解析时也必须设置 `route.default_domain_resolver`。
9. 未知字段、错误类型、重复 tag、循环引用和空 fallback tag 都必须在 `sing-box check -c` 阶段失败。
10. wrapper 只允许包装现有 DNS transport，不允许自己解析订阅字段或改写 outbound。

## 解析选择逻辑

一次 outbound server 域名解析按以下顺序执行：

1. 读取 outbound 显式指定的 `domain_resolver.server`。
2. 如果解析组为 `local_only`，直接使用本机 DNS，并记录 `mode=local_only`。
3. 如果解析组有健康 DoH，按配置顺序尝试健康 DoH。
4. 如果某个 DoH 返回有效 DNS 响应，直接使用该结果。
5. 如果 DoH 出现 transport timeout、dial error、TLS error、HTTP 5xx、协议解析失败或 resolver SERVFAIL，则累计该 DoH 失败证据。
6. 当前 DoH 达到失败阈值后进入 unhealthy 或 cooldown，继续尝试组内下一个 DoH。
7. 只有当组内所有 DoH 都不可用，且 `fallback_enabled=true` 时，才使用本机 DNS。
8. 如果组内所有 DoH 都不可用且 fallback 未开启，返回解析错误。

不能触发 fallback 的情况：

- NXDOMAIN。
- NODATA。
- NOERROR 但结果为空。
- 明确的 DNS 策略拒绝。
- outbound、route 或配置校验错误。

这些结果说明 resolver 正常给出了 DNS 语义响应，不代表 DoH transport 不可用。

## 健康状态与恢复

每个 primary DoH transport 维护独立状态：

```text
healthy -> suspect -> unhealthy -> probing -> healthy
```

建议默认参数：

| 参数 | 默认值 | 说明 |
| --- | --- | --- |
| `failure_threshold` | `2` | 连续失败达到阈值后标记不可用 |
| `unhealthy_cooldown` | `20s` | 不可用后等待一段时间再探测 |
| `probe_interval` | `30s` | fallback 期间周期性探测 DoH |
| `recovery_success_threshold` | `2` | 连续探测成功后恢复 healthy |
| `fallback_ttl_cap` | `60s` | 本机 DNS fallback 结果的最大缓存 TTL |

恢复要求：

1. DoH 不可用期间，解析组继续按 `probe_interval` 探测 primary DoH。
2. DoH 达到恢复成功阈值后，新解析请求必须优先使用 DoH。
3. local fallback 产生的缓存结果不能长期压住已恢复的 DoH。
4. DoH 恢复时，应清理或绕过该解析组内由 local fallback 产生的缓存项。
5. 恢复只影响该解析组，不能改变其它解析组的状态。

## 缓存策略

缓存 key 至少包含：

- resolver group tag。
- 实际 resolver tag。
- domain。
- query type。
- strategy。

本机 DNS fallback 结果需要带来源标记：

```text
source=local_fallback
group=rf-resolver-vendor-a
resolver=local-system
```

当 `rf-resolver-vendor-a` 的 DoH 恢复 healthy 后，该组内 `source=local_fallback` 的缓存项必须失效或不再优先命中。当前实现对非 `local_only` 的 `routefluent_resolver_group` 禁用 sing-box 外层 DNS cache，让每次 outbound server 解析都进入解析组状态机，从而避免 local fallback 缓存压住已恢复的 DoH；`local_only` 模式仍允许正常缓存，因为它不是临时 fallback。

## DoH 自身域名的 bootstrap

DoH server 自身可能也是域名，例如 `dns.vendor-a.example`。这类 bootstrap 解析与 outbound server 解析必须区分：

1. 如果 DoH endpoint 配置为 IP，应直接连接。
2. 如果 DoH endpoint 配置为域名，可显式指定 bootstrap resolver。
3. bootstrap 可以使用本机 DNS，但日志必须标记为 `purpose=doh_bootstrap`，不能伪装成 outbound server fallback。
4. bootstrap 失败只影响该 DoH transport 的健康状态，不应污染其它解析组。

## 审计与日志

每次解析和状态变化至少记录：

- resolver group tag。
- outbound tag。
- domain。
- query type 与 strategy。
- 实际选择的 resolver tag。
- 是否 local fallback。
- fallback 原因。
- DoH 健康状态变化。
- probe 成功或失败。
- 缓存命中来源。
- 返回地址族与结果数量。

当前实现的 release smoke 会验证以下关键证据：

- primary DoH 能解析 outbound server 域名并完成 `tools fetch`。
- primary DoH 不可用且 `fallback_enabled=false` 时按 `primary DoH resolvers unavailable` 失败关闭。
- Linux release 环境中，primary DoH 不可用且 `fallback_enabled=true` 时可以临时使用本机 DNS。
- `fallback_enabled=true` 但缺少 `probe_domains` 的配置必须在 `check -c` 阶段失败。
- DoH endpoint 为域名且带 bootstrap `domain_resolver` 的配置必须在 `check -c` 阶段通过。

敏感信息处理：

- 不记录 DoH header、认证参数或完整 token。
- DoH URL path 可按配置安全级别决定是否脱敏。
- 失败日志应可定位到 resolver tag，而不是只记录“DNS failed”。

## 与 RouteFluent 服务端预解析的关系

RouteFluent 仍保留服务端预解析 IP 下发能力。sing-box 侧新增能力用于以下场景：

- 线路商家提供 DoH，且希望终端按商家解析结果连接代理 server。
- 终端网络环境变化较频繁，服务端预解析结果可能不如终端侧 DoH 合适。
- 需要在 DoH 短时不可用时保持线路可建连，同时保留恢复后回到 DoH 的机制。

服务端预解析仍适合以下场景：

- 需要强制固定 IP 连接。
- DoH 配置不可审计或质量不可信。
- 目标设备不具备自构建 sing-box 能力。
- 配置策略要求失败关闭，不允许终端使用本机 DNS。

二者应由上游配置生成器显式选择，不能在 sing-box 内自动把 IP、DoH 和本机 DNS 混成不可审计的猜测链路。

## 实现位置建议

建议只做窄范围 patch：

1. 在 `option/dns.go` 增加 `routefluent_resolver_group` 配置结构。
2. 在 DNS transport registry 中注册新类型。
3. 新增 `dns/transport/routefluentgroup` 包，包装已有 `adapter.DNSTransport`。
4. wrapper 内维护 primary DoH 的健康状态、probe 定时器和 fallback 证据。
5. outbound 仍使用原生 `domain_resolver.server` 绑定该 wrapper tag。
6. 新增 `adapter.DNSCacheControlTransport`，让非 `local_only` 解析组绕开 sing-box 外层 DNS cache，避免恢复后继续命中 fallback 结果。
7. 不修改 route selector、outbound selector、subscription parser 或代理协议实现。

## 测试计划

单元测试：

1. 单 DoH healthy 时使用 DoH。
2. 多 DoH 时第一个失败、第二个成功，不使用 local。
3. 全部 DoH transport 失败后使用 local fallback。
4. `fallback_enabled=false` 时全部 DoH 失败应返回错误。
5. NXDOMAIN、NODATA 和空 NOERROR 不触发 local fallback。
6. DoH 连续失败进入 unhealthy。
7. fallback 期间 probe 成功后恢复 healthy。
8. DoH 恢复后新请求优先回到 DoH。
9. local fallback 缓存不会压住已恢复的 DoH。
10. 商家 A 的 DoH 故障不影响商家 B。
11. `local_only` 模式使用本机 DNS，但不记为 DoH fallback。
12. malformed config 在 `sing-box check -c` 阶段失败。

集成测试：

1. 启动两个 mock DoH server 和一个 mock local resolver。
2. vendor A DoH 正常，vendor B DoH 正常，分别验证 outbound 解析隔离。
3. 关闭 vendor A DoH，仅 vendor A 使用 local fallback，vendor B 仍用自己的 DoH。
4. 恢复 vendor A DoH，等待 probe 成功后验证 vendor A 新解析回到 DoH。
5. 配置 vendor C 无 DoH，验证 local-only 日志与 fallback 日志不同。

验收标准：

1. `sing-box check -c` 能拒绝错误配置。
2. `go test ./...` 通过。
3. 可通过日志证明 DoH 优先、local 临时 fallback 和 DoH 自动恢复。
4. 不引入 outbound 自动换线或 route fallback。
5. 不影响现有 AnyTLS `client` 字段 patch。
