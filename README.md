# RF sing-box

RF 的受控制品构建仓库。它以固定的上游 sing-box 版本为基线，只维护 RF 尚未被上游覆盖的窄范围 DNS 能力；AnyTLS 客户端元数据使用上游正式字段 `client_metadata`，不再维护私有 `client` 字段。

该仓库会：

1. 克隆并核对精确的上游 sing-box tag 与 commit；
2. 应用 RF DNS resolver group、LineAsset 入口 binding transport 与严格 `check` 生命周期补丁；
3. 构建静态 Linux amd64 二进制；
4. 输出可机读、带哈希的构建 manifest；
5. 通过 GitHub Actions 校验并发布不可变制品。

## 构建输入

| 输入 | 值 |
| --- | --- |
| sing-box | `v1.14.2` / `af6e64c3b69e6132ebaee0e1a3d24e93903f6709` |
| sing-anytls（上游依赖） | `v0.0.11` / `130d2e61b8895727bfed4942c535e91b246a9603` |
| RF patch id | `rf-dns-query-recipe-check-v1` |
| 版本名 | `1.14.2-rf-dns.3` |
| 默认 build tags | `with_utls with_clash_api` |
| 目标 | `linux/amd64`, `CGO_ENABLED=0` |

## 能力边界

### 上游 AnyTLS `client_metadata`

sing-box 1.13.16 起原生支持 AnyTLS `client_metadata`。RF 编译器只生成这个上游字段：

```json
{
  "type": "anytls",
  "tag": "outbound-anytls",
  "server": "example.com",
  "server_port": 443,
  "password": "secret",
  "client_metadata": "mihomo/1.19.28"
}
```

旧私有字段 `client` 必须被 `sing-box check -c` 拒绝。设备是否具备该能力以受控二进制的版本证据判定，不再上报 RF 私有 AnyTLS capability。

### RF resolver group

补丁增加 `rf_resolver_group` DNS server：

```json
{
  "tag": "rf-resolver-vendor-a",
  "type": "rf_resolver_group",
  "primary": ["vendor-a-doh"],
  "fallback": "local-system",
  "fallback_enabled": true,
  "probe_domains": ["www.gstatic.com", "cloudflare.com"]
}
```

详细契约见 [docs/doh-priority-local-fallback-resolution.md](docs/doh-priority-local-fallback-resolution.md)。关键边界如下：

- 常规模式必须至少声明一个 primary DoH；
- 开启 local fallback 时必须显式声明 local resolver 与非空探测域名；
- 未开启 fallback 时，全部 primary 失效即失败关闭；
- 无 DoH 的线路必须显式选择 `local_only`；
- outbound 的域名解析必须显式绑定对应 resolver group；
- resolver group 的健康状态通过上游 `DNSTransportWithEnvironment` 进入 DNS cache key，健康状态变化会自然淘汰旧 fallback 缓存；
- `sing-box check` 会执行 pre-start，部署前即验证 resolver 类型和依赖关系。

本机 DNS fallback 只是同一 provider resolver group 内的显式可用性策略，不是线路切换、订阅兼容或业务流量 direct 放行。

### RF LineAsset 入口 binding

`rf_ingress_binding` 是每个已部署 LineAsset 独立的 DNS-shaped transport。它读取 Agent 原子提交的单一 binding current：`resolver` 按可选 `query_name`（缺省为 logical domain）委托给资产既定 resolver，并把答案归还给原 logical domain；`explicit_ipv4` 只为该资产的 logical domain 返回当前一个 IPv4。没有 Provider 关联但存在别名配方时，transport 只执行静态 query-name 改写，不要求虚构 binding current。`binding_revision` 与 query name 进入 DNS cache environment；地址簿和历史候选不会进入 sing-box 配置，也不会形成 try-next。

## 本地构建与验证

依赖 Git、Go 1.25.5 和 Python 3.9+：

```bash
python build_rf_sing_box.py
```

输出：

```text
dist/sing-box-linux-amd64
dist/sing-box-linux-amd64.rf-dns.json
```

验证：

```bash
./dist/sing-box-linux-amd64 version
./dist/sing-box-linux-amd64 check -c testdata/anytls-client-metadata-check.json
! ./dist/sing-box-linux-amd64 check -c testdata/anytls-legacy-client-rejected.json
./dist/sing-box-linux-amd64 check -c testdata/rf-dns-resolver-group-check.json
./dist/sing-box-linux-amd64 check -c testdata/rf-dns-doh-bootstrap-check.json
./dist/sing-box-linux-amd64 check -c testdata/rf-ingress-binding-check.json
! ./dist/sing-box-linux-amd64 check -c testdata/rf-dns-invalid-primary-local.json
! ./dist/sing-box-linux-amd64 check -c testdata/rf-dns-invalid-fallback-https.json
! ./dist/sing-box-linux-amd64 check -c testdata/rf-dns-invalid-fallback-missing-probes.json
python3 scripts/rf_dns_runtime_smoke.py --sing-box ./dist/sing-box-linux-amd64
```

版本输出必须包含：

```text
1.14.2-rf-dns.3
```

## GitHub Release

`.github/workflows/release.yml` 是唯一发布流程。`v*` tag 会触发构建、静态配置校验、DNS runtime smoke、checksum 生成和 GitHub Release 发布。

当前 tag：

```bash
git tag v1.14.2-rf-dns.3
git push origin v1.14.2-rf-dns.3
```

发布资产：

- `sing-box-linux-amd64`
- `sing-box-linux-amd64.rf-dns.json`
- `SHA256SUMS`

## 下游使用

发布制品模式：

```bash
VERSION=v1.14.2-rf-dns.3
BASE=https://github.com/DavidYordan/routefluent-sing-box/releases/download/$VERSION

curl -L -o sing-box-linux-amd64 "$BASE/sing-box-linux-amd64"
curl -L -o sing-box-linux-amd64.rf-dns.json "$BASE/sing-box-linux-amd64.rf-dns.json"
curl -L -o SHA256SUMS "$BASE/SHA256SUMS"
sha256sum -c SHA256SUMS
chmod +x sing-box-linux-amd64
./sing-box-linux-amd64 version
```

RF 主仓库通过 `third_party/sing-box` 子模块固定此仓库 commit，并校验 release URL、manifest、二进制大小和 SHA256。主路径只接受以下明确能力：

- `upstream_anytls_client_metadata`
- `rf_dns_resolver_group`
- `rf_ingress_binding`
- `rf_dns_check_start_validation`

从子模块直接构建：

```bash
python third_party/sing-box/build_rf_sing_box.py \
  --output dist/sing-box-linux-amd64 \
  --manifest dist/sing-box-linux-amd64.rf-dns.json
```

## 兼容边界

该制品不是订阅解析兼容层，不提供自动换线路、跨 provider fallback 或数据面 direct。RF 编译器和 Deploy 路径仍会在字段、版本、manifest 或运行证据不一致时失败关闭。

生成二进制所包含的上游 sing-box 与 sing-anytls 继续适用各自许可证；本仓库负责可复现构建和 RF 窄补丁。

## 源码缓存

构建脚本把上游 checkout 保存在指定 workdir 的 `src` 目录。缓存恰好位于固定 commit 时，脚本会先将这个专用派生目录清理回该 commit，再重新应用 RF 补丁；commit 不一致或目录不是 Git checkout 时才删除并重新克隆。缓存 checkout 强制使用 LF，避免 Windows 全局 Git 配置改变制品哈希。缓存不会改变版本核验、补丁锚点校验或制品哈希要求。
