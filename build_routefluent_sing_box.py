#!/usr/bin/env python3
"""Build RouteFluent's controlled sing-box binary.

The patch is intentionally narrow: it exposes sing-anytls' client broadcast
value through sing-box AnyTLS outbound config as `client`. RouteFluent compiler
and deploy gates remain responsible for deciding when that field may be used.
"""

from __future__ import annotations

import argparse
import hashlib
import json
import os
from pathlib import Path
import shutil
import stat
import subprocess
import sys
import time


SING_BOX_REPO = "https://github.com/SagerNet/sing-box.git"
SING_ANYTLS_REPO = "https://github.com/anytls/sing-anytls.git"
SING_BOX_VERSION = "v1.13.12"
SING_ANYTLS_VERSION = "v0.0.11"
SING_BOX_COMMIT = "1086ab2563320e0da0c23b3a491d8dfa0939dff4"
SING_ANYTLS_COMMIT = "130d2e61b8895727bfed4942c535e91b246a9603"
PATCH_ID = "routefluent-anytls-client-config-v1"
FEATURE_ANYTLS_CLIENT_FIELD = "anytls_outbound_client_field"
PATCHED_SING_BOX_VERSION = "1.13.12-routefluent-anytls-client.2"
DEFAULT_TAGS = "with_utls with_clash_api"


def sha256_file(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as file:
        for chunk in iter(lambda: file.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def run(args: list[str], cwd: Path | None = None, env: dict[str, str] | None = None) -> str:
    cwd_display = str(cwd) if cwd else os.getcwd()
    print(f"+ {cwd_display}> {' '.join(args)}")
    completed = subprocess.run(
        args,
        cwd=str(cwd) if cwd else None,
        env=env,
        text=True,
        stdout=subprocess.PIPE,
        stderr=subprocess.STDOUT,
    )
    if completed.stdout:
        print(completed.stdout, end="")
    if completed.returncode != 0:
        raise RuntimeError(f"command failed with exit {completed.returncode}: {' '.join(args)}")
    return completed.stdout


def require_tool(name: str) -> str:
    found = shutil.which(name)
    if not found:
        raise RuntimeError(f"{name} executable not found in PATH")
    return found


def remove_tree(path: Path) -> None:
    def handle_remove_error(func, target, exc_info):
        try:
            os.chmod(target, stat.S_IWRITE)
            func(target)
        except Exception:
            raise exc_info[1]

    shutil.rmtree(path, onerror=handle_remove_error)


def clone_exact(repo: str, tag: str, expected_commit: str, dest: Path) -> Path:
    git = require_tool("git")
    last_err: Exception | None = None
    for attempt in range(1, 4):
        if dest.exists():
            remove_tree(dest)
        try:
            dest.parent.mkdir(parents=True, exist_ok=True)
            run([git, "clone", "--depth", "1", "--branch", tag, repo, str(dest)])
            last_err = None
            break
        except RuntimeError as exc:
            last_err = exc
            if attempt == 3:
                break
            print(f"[WARN] clone attempt {attempt}/3 failed for {repo}; retrying")
            time.sleep(3 * attempt)
    if last_err is not None:
        raise last_err
    dest.parent.mkdir(parents=True, exist_ok=True)
    actual = run([git, "rev-parse", "HEAD"], cwd=dest).strip()
    if actual != expected_commit:
        raise RuntimeError(
            f"{repo} {tag} resolved to {actual}, expected {expected_commit}; refusing to build unknown source"
        )
    return dest


def replace_once(path: Path, old: str, new: str) -> None:
    text = path.read_text(encoding="utf-8")
    if old not in text:
        raise RuntimeError(f"patch anchor not found in {path}: {old!r}")
    path.write_text(text.replace(old, new, 1), encoding="utf-8")


def patch_sing_anytls(source_dir: Path) -> None:
    replace_once(
        source_dir / "client.go",
        "type ClientConfig struct {\n\tPassword                 string\n",
        "type ClientConfig struct {\n\tPassword                 string\n\tClient                   string\n",
    )
    replace_once(
        source_dir / "client.go",
        "c.sessionClient = session.NewClient(ctx, config.Logger, c.createOutboundConnection, &c.padding, config.IdleSessionCheckInterval, config.IdleSessionTimeout, config.MinIdleSession)",
        "c.sessionClient = session.NewClient(ctx, config.Logger, c.createOutboundConnection, config.Client, &c.padding, config.IdleSessionCheckInterval, config.IdleSessionTimeout, config.MinIdleSession)",
    )
    replace_once(
        source_dir / "session" / "client.go",
        "\tdialOut util.DialOutFunc\n\n\tsessionCounter atomic.Uint64\n",
        "\tdialOut util.DialOutFunc\n\tclient  string\n\n\tsessionCounter atomic.Uint64\n",
    )
    replace_once(
        source_dir / "session" / "client.go",
        "func NewClient(ctx context.Context, logger logger.Logger, dialOut util.DialOutFunc,\n\t_padding *atomic.TypedValue[*padding.PaddingFactory], idleSessionCheckInterval, idleSessionTimeout time.Duration, minIdleSession int,\n) *Client {",
        "func NewClient(ctx context.Context, logger logger.Logger, dialOut util.DialOutFunc,\n\tclient string, _padding *atomic.TypedValue[*padding.PaddingFactory], idleSessionCheckInterval, idleSessionTimeout time.Duration, minIdleSession int,\n) *Client {",
    )
    replace_once(
        source_dir / "session" / "client.go",
        "\t\tdialOut:            dialOut,\n\t\tpadding:            _padding,\n",
        "\t\tdialOut:            dialOut,\n\t\tclient:             client,\n\t\tpadding:            _padding,\n",
    )
    replace_once(
        source_dir / "session" / "client.go",
        "\tsession := NewClientSession(underlying, c.padding, c.logger)\n",
        "\tsession := NewClientSession(underlying, c.client, c.padding, c.logger)\n",
    )
    replace_once(
        source_dir / "session" / "session.go",
        "\t// client\n\tisClient    bool\n",
        "\t// client\n\tclient      string\n\tisClient    bool\n",
    )
    replace_once(
        source_dir / "session" / "session.go",
        "func NewClientSession(conn net.Conn, _padding *atomic.TypedValue[*padding.PaddingFactory], logger logger.Logger) *Session {\n",
        "func NewClientSession(conn net.Conn, client string, _padding *atomic.TypedValue[*padding.PaddingFactory], logger logger.Logger) *Session {\n",
    )
    replace_once(
        source_dir / "session" / "session.go",
        "\t\tconn:        conn,\n\t\tisClient:    true,\n",
        "\t\tconn:        conn,\n\t\tclient:      client,\n\t\tisClient:    true,\n",
    )
    replace_once(
        source_dir / "session" / "session.go",
        "\tsettings := util.StringMap{\n\t\t\"v\":           \"2\",\n\t\t\"client\":      util.Verison,\n\t\t\"padding-md5\": s.padding.Load().Md5,\n\t}\n",
        "\tclient := s.client\n\tif client == \"\" {\n\t\tclient = util.Verison\n\t}\n\tsettings := util.StringMap{\n\t\t\"v\":           \"2\",\n\t\t\"client\":      client,\n\t\t\"padding-md5\": s.padding.Load().Md5,\n\t}\n",
    )


def patch_sing_box(source_dir: Path) -> None:
    replace_once(
        source_dir / "option" / "anytls.go",
        "\tPassword                 string             `json:\"password,omitempty\"`\n",
        "\tPassword                 string             `json:\"password,omitempty\"`\n\tClient                   string             `json:\"client,omitempty\"`\n",
    )
    replace_once(
        source_dir / "protocol" / "anytls" / "outbound.go",
        "\t\tPassword:                 options.Password,\n",
        "\t\tPassword:                 options.Password,\n\t\tClient:                   options.Client,\n",
    )


def build(args: argparse.Namespace) -> None:
    workdir = Path(args.workdir).resolve()
    src_root = workdir / "src"
    sing_box_dir = src_root / f"sing-box-{SING_BOX_VERSION.removeprefix('v')}"
    sing_anytls_dir = src_root / f"sing-anytls-{SING_ANYTLS_VERSION.removeprefix('v')}-routefluent-client"

    clone_exact(SING_BOX_REPO, SING_BOX_VERSION, SING_BOX_COMMIT, sing_box_dir)
    clone_exact(SING_ANYTLS_REPO, SING_ANYTLS_VERSION, SING_ANYTLS_COMMIT, sing_anytls_dir)

    patch_sing_anytls(sing_anytls_dir)
    patch_sing_box(sing_box_dir)

    go = require_tool("go")
    run([go, "fmt", "./..."], cwd=sing_anytls_dir)
    run([go, "fmt", "./option", "./protocol/anytls"], cwd=sing_box_dir)
    replace_path = os.path.relpath(sing_anytls_dir, sing_box_dir).replace(os.sep, "/")
    run([go, "mod", "edit", f"-replace=github.com/anytls/sing-anytls={replace_path}"], cwd=sing_box_dir)

    output = Path(args.output).resolve()
    output.parent.mkdir(parents=True, exist_ok=True)
    env = os.environ.copy()
    env.update({"GOOS": args.goos, "GOARCH": args.goarch, "CGO_ENABLED": "0"})

    tags = " ".join(args.tags.split())
    build_cmd = [
        go,
        "build",
        "-trimpath",
        "-ldflags",
        f"-s -w -X github.com/sagernet/sing-box/constant.Version={PATCHED_SING_BOX_VERSION}",
    ]
    if tags:
        build_cmd.extend(["-tags", tags])
    build_cmd.extend(["-o", str(output), "./cmd/sing-box"])
    run(build_cmd, cwd=sing_box_dir, env=env)

    metadata = {
        "schema": "routefluent.sing_box_build.v1",
        "name": "sing-box",
        "patch_id": PATCH_ID,
        "features": [FEATURE_ANYTLS_CLIENT_FIELD],
        "sing_box_version": SING_BOX_VERSION,
        "sing_box_commit": SING_BOX_COMMIT,
        "sing_anytls_version": SING_ANYTLS_VERSION,
        "sing_anytls_commit": SING_ANYTLS_COMMIT,
        "version_name": PATCHED_SING_BOX_VERSION,
        "goos": args.goos,
        "goarch": args.goarch,
        "cgo_enabled": "0",
        "tags": tags.split() if tags else [],
        "source_repositories": {
            "sing_box": SING_BOX_REPO,
            "sing_anytls": SING_ANYTLS_REPO,
        },
        "binary": str(output),
        "binary_size": output.stat().st_size,
        "binary_sha256": sha256_file(output),
        "built_at": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
    }
    manifest = Path(args.manifest).resolve()
    manifest.parent.mkdir(parents=True, exist_ok=True)
    manifest.write_text(json.dumps(metadata, ensure_ascii=False, indent=2, sort_keys=True) + "\n", encoding="utf-8")
    print(json.dumps(metadata, ensure_ascii=False, indent=2, sort_keys=True))


def parse_args(argv: list[str]) -> argparse.Namespace:
    parser = argparse.ArgumentParser(description="Build RouteFluent patched sing-box with AnyTLS client field support.")
    default_workdir = Path(__file__).resolve().parent / "work"
    default_output = Path(__file__).resolve().parent / "dist" / "sing-box-linux-amd64"
    default_manifest = Path(__file__).resolve().parent / "dist" / "sing-box-linux-amd64.routefluent-anytls-client.json"
    parser.add_argument("--workdir", default=str(default_workdir))
    parser.add_argument("--output", default=str(default_output))
    parser.add_argument("--manifest", default=str(default_manifest))
    parser.add_argument("--goos", default="linux")
    parser.add_argument("--goarch", default="amd64")
    parser.add_argument("--tags", default=os.environ.get("ROUTEFLUENT_SING_BOX_TAGS", DEFAULT_TAGS))
    return parser.parse_args(argv)


if __name__ == "__main__":
    try:
        build(parse_args(sys.argv[1:]))
    except Exception as exc:
        print(f"[ERROR] {exc}", file=sys.stderr)
        raise SystemExit(1) from exc
