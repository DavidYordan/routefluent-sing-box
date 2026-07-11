# RouteFluent sing-box

Controlled sing-box build used by RouteFluent when a route explicitly requires the AnyTLS outbound `client` field.

This repository does not maintain a broad sing-box fork. It is a deterministic build wrapper that:

1. clones exact upstream sources,
2. verifies exact commits,
3. applies a narrow AnyTLS client-field patch,
4. builds a Linux amd64 binary,
5. writes a machine-readable build manifest,
6. publishes release assets through GitHub Actions.

## Build Inputs

| Input | Value |
| --- | --- |
| sing-box | `v1.13.12` / `1086ab2563320e0da0c23b3a491d8dfa0939dff4` |
| sing-anytls | `v0.0.11` / `130d2e61b8895727bfed4942c535e91b246a9603` |
| RouteFluent patch id | `routefluent-anytls-client-config-v1` |
| Version name | `1.13.12-routefluent-anytls-client.2` |
| Default tags | `with_utls with_clash_api` |
| Target | `linux/amd64`, `CGO_ENABLED=0` |

## Patch Scope

The patch only exposes sing-anytls' client broadcast value through sing-box AnyTLS outbound config:

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
```

Expected version string includes:

```text
1.13.12-routefluent-anytls-client.2
```

## GitHub Release

`docs/release-workflow.yml` is the intended GitHub Actions workflow. Copy it to `.github/workflows/release.yml` in this repository to enable GitHub-built releases.

Tags matching `v*` then trigger the release workflow.

Recommended tag:

```bash
git tag v1.13.12-routefluent-anytls-client.2
git push origin v1.13.12-routefluent-anytls-client.2
```

Release assets:

- `sing-box-linux-amd64`
- `sing-box-linux-amd64.routefluent-anytls-client.json`
- `SHA256SUMS`

The first repository publication used an OAuth token without the `workflow` scope, so the workflow is committed as a template rather than an active workflow file. Enabling the active workflow requires a token with `workflow` scope.

## RouteFluent Submodule Use

RouteFluent consumes this repository as:

```bash
git submodule update --init --recursive third_party/sing-box
```

The RouteFluent build scripts pass explicit output and manifest paths, so the same build script works both standalone and as a submodule.

## Compatibility Boundary

This binary exists for one controlled case: providers whose AnyTLS servers reject stock sing-anytls' native client broadcast but accept `mihomo/1.19.28`.

It is not a subscription parser compatibility layer, not an automatic provider fallback, and not a data-plane failover mechanism. RouteFluent's compiler and deploy path remain fail-closed unless the route config explicitly chooses a non-native AnyTLS client mode and the target device reports the patched runtime capability.

## Upstream Licenses

The binary is built from upstream sing-box and sing-anytls sources at the commits listed above. Their upstream licenses apply to the generated binary and patched source. This repository provides the deterministic build automation and patch procedure needed to reproduce the distributed artifacts.
