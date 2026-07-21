# RouteFluent sing-box

> NekoRay product integration notice (2026-07-20): the current product contract permits only strict `primary` DoH resolver groups. `fallback`, `fallback_enabled`, `local_only`, probe-based recovery, and local DNS bootstrap are quarantined legacy experiments; the root ConfigBuilder rejects them, including in custom configs. They must be removed from the controlled fork before release and must not be generated or advertised as supported product behavior.

Controlled sing-box build used by RouteFluent when a route explicitly requires the AnyTLS outbound `client` field or RouteFluent's provider-scoped DNS resolver group.

This repository does not maintain a broad sing-box fork. It is a deterministic build wrapper that:

1. clones exact upstream sources,
2. verifies exact commits,
3. applies narrow RouteFluent patches,
4. builds a Linux amd64 binary,
5. writes a machine-readable build manifest,
6. publishes release assets through GitHub Actions.

## Build Inputs

| Input | Value |
| --- | --- |
| sing-box | `v1.13.12` / `1086ab2563320e0da0c23b3a491d8dfa0939dff4` |
| sing-anytls | `v0.0.11` / `130d2e61b8895727bfed4942c535e91b246a9603` |
| RouteFluent patch id | `routefluent-anytls-client-dns-resolver-group-check-v1` |
| Version name | `1.13.12-routefluent-anytls-client.7` |
| Default tags | `with_utls with_clash_api` |
| Target | `linux/amd64`, `CGO_ENABLED=0` |

## Patch Scope

The first patch exposes sing-anytls' client broadcast value through sing-box AnyTLS outbound config:

```json
{
  "type": "anytls",
  "tag": "outbound-anytls",
  "server": "example.com",
  "server_port": 443,
  "password": "secret",
  "client": "mihomo/1.19.28"
}
```

If `client` is omitted, sing-anytls keeps its native default behavior.

RouteFluent still treats this as an explicit runtime capability. A device may only report:

```text
sing_box_anytls_client=ok:client_field
```

after its installed binary passes `sing-box check -c` with an AnyTLS outbound containing the `client` field. Stock sing-box must not report this capability.

The second patch adds a RouteFluent DNS server type:

```json
{
  "tag": "rf-resolver-vendor-a",
  "type": "routefluent_resolver_group",
  "primary": ["vendor-a-doh"],
  "fallback": "local-system",
  "fallback_enabled": true,
  "probe_domains": ["www.gstatic.com", "cloudflare.com"]
}
```

It is documented in `docs/doh-priority-local-fallback-resolution.md`. The behavior is provider-scoped: configured DoH resolvers are preferred, local host DNS is allowed only as a temporary fallback when every DoH resolver in that group is unavailable, and recovered DoH resolvers automatically become preferred again. A `local_only` mode exists for providers that do not carry DoH.

Generator contract:

- regular mode must configure at least one primary DoH server;
- `fallback_enabled=true` must also configure a local fallback resolver and non-empty `probe_domains`;
- strict provider-DoH mode must leave `fallback_enabled=false`; when all primary DoH resolvers are unavailable, resolution fails closed;
- provider lines without DoH must use `mode=local_only`, empty `primary`, and an explicit local fallback;
- outbounds whose `server` is a domain must bind `domain_resolver.server` to the provider-scoped RouteFluent resolver group;
- configs that rely on route-level domain resolution should also set `route.default_domain_resolver`;
- DoH endpoints whose own `server` is a domain must set an explicit bootstrap `domain_resolver`.

Debug logging records resolver-group selection, strict failures, fallback use, and recovery probes. Local fallback is an explicit availability policy for the same provider resolver group; it is not route fallback, outbound failover, subscription compatibility, or a data-plane direct bypass.

The third patch makes `sing-box check` run the service pre-start lifecycle. This catches RouteFluent DNS transport dependency errors before deployment, including a local resolver used as a DoH primary or a non-local resolver used as fallback.

## Local Build

Prerequisites:

- Git
- Go
- Python 3.9+

Run:

```bash
python build_routefluent_sing_box.py
```

Outputs:

```text
dist/sing-box-linux-amd64
dist/sing-box-linux-amd64.routefluent-anytls-client.json
```

Smoke check:

```bash
./dist/sing-box-linux-amd64 version
./dist/sing-box-linux-amd64 check -c testdata/anytls-client-check.json
./dist/sing-box-linux-amd64 check -c testdata/routefluent-dns-resolver-group-check.json
./dist/sing-box-linux-amd64 check -c testdata/routefluent-dns-doh-bootstrap-check.json
! ./dist/sing-box-linux-amd64 check -c testdata/routefluent-dns-invalid-primary-local.json
! ./dist/sing-box-linux-amd64 check -c testdata/routefluent-dns-invalid-fallback-https.json
! ./dist/sing-box-linux-amd64 check -c testdata/routefluent-dns-invalid-fallback-missing-probes.json
python3 scripts/routefluent_dns_runtime_smoke.py --sing-box ./dist/sing-box-linux-amd64
```

Expected version string includes:

```text
1.13.12-routefluent-anytls-client.7
```

## GitHub Release

`.github/workflows/release.yml` is the active GitHub Actions workflow. `docs/release-workflow.yml` is kept as a readable copy for downstream audits.

Tags matching `v*` trigger the release workflow. The workflow builds the Linux amd64 binary, runs the static RouteFluent check fixtures, runs the local runtime DNS smoke, writes checksums, and publishes the immutable release assets.

Recommended tag:

```bash
git tag v1.13.12-routefluent-anytls-client.7
git push origin v1.13.12-routefluent-anytls-client.7
```

Release assets:

- `sing-box-linux-amd64`
- `sing-box-linux-amd64.routefluent-anytls-client.json`
- `SHA256SUMS`

Pushing workflow changes from a local GitHub CLI session requires the `workflow` OAuth scope. Browser edits use the browser GitHub session instead.

## Use From Another Project

Other projects should consume an immutable release or pin this repository as a submodule. Do not replace a stock sing-box binary with this build unless the target runtime explicitly requires and verifies at least one of these features:

- `anytls_outbound_client_field`
- `routefluent_dns_resolver_group`
- `routefluent_dns_check_start_validation`

Release-binary mode:

```bash
VERSION=v1.13.12-routefluent-anytls-client.7
BASE=https://github.com/DavidYordan/routefluent-sing-box/releases/download/$VERSION

curl -L -o sing-box-linux-amd64 "$BASE/sing-box-linux-amd64"
curl -L -o sing-box-linux-amd64.routefluent-anytls-client.json "$BASE/sing-box-linux-amd64.routefluent-anytls-client.json"
curl -L -o SHA256SUMS "$BASE/SHA256SUMS"
sha256sum -c SHA256SUMS
chmod +x sing-box-linux-amd64
./sing-box-linux-amd64 version
```

Pinned submodule mode:

```bash
git submodule add https://github.com/DavidYordan/routefluent-sing-box.git third_party/routefluent-sing-box
git -C third_party/routefluent-sing-box checkout v1.13.12-routefluent-anytls-client.7
git add .gitmodules third_party/routefluent-sing-box
```

Build from the submodule when the consuming project needs to produce its own artifact:

```bash
python third_party/routefluent-sing-box/build_routefluent_sing_box.py \
  --output dist/sing-box-linux-amd64 \
  --manifest dist/sing-box-linux-amd64.routefluent-anytls-client.json
```

## RouteFluent Submodule Use

RouteFluent consumes this repository as:

```bash
git submodule update --init --recursive third_party/sing-box
```

The RouteFluent build scripts pass explicit output and manifest paths, so the same build script works both standalone and as a submodule.

## Compatibility Boundary

This binary exists for controlled RouteFluent runtime gaps that stock sing-box does not expose:

- providers whose AnyTLS servers reject stock sing-anytls' native client broadcast but accept `mihomo/1.19.28`;
- outbound server-domain resolution that must prefer provider DoH, temporarily fall back to local DNS only when all DoH resolvers in that group are unavailable, and recover back to DoH.

It is not a subscription parser compatibility layer, not an automatic provider fallback, and not a data-plane failover mechanism. RouteFluent's compiler and deploy path remain fail-closed unless the route config explicitly chooses a patched capability and the target device reports that capability.

## Upstream Licenses

The binary is built from upstream sing-box and sing-anytls sources at the commits listed above. Their upstream licenses apply to the generated binary and patched source. This repository provides the deterministic build automation and patch procedure needed to reproduce the distributed artifacts.

## Source Cache

`build_routefluent_sing_box.py` keeps upstream source checkouts under `work/src`.
If the checkout already exists at the pinned commit, the script reuses it, resets
it locally, reapplies the RouteFluent patches, and builds without cloning again.
The script only clones from the upstream repositories when the local checkout is
missing or pinned to a different commit.

This keeps all source cache state inside this project directory. It does not use
or mutate global Go, Git, or system package locations beyond normal Go module
cache behavior.
