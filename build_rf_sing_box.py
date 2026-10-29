#!/usr/bin/env python3
"""Build RF's controlled sing-box binary.

Upstream sing-box 1.13.16 and later owns AnyTLS ``client_metadata``.  The
RF patch is therefore limited to the provider-scoped DNS resolver
group, the Device/LineAsset ingress-binding transport, and strict
``sing-box check`` pre-start validation.
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
SING_BOX_VERSION = "v1.14.2"
SING_ANYTLS_VERSION = "v0.0.11"
SING_BOX_COMMIT = "af6e64c3b69e6132ebaee0e1a3d24e93903f6709"
SING_ANYTLS_COMMIT = "130d2e61b8895727bfed4942c535e91b246a9603"
REPO_ROOT = Path(__file__).resolve().parent
PATCH_ID = "rf-dns-query-recipe-check-v1"
FEATURE_UPSTREAM_ANYTLS_CLIENT_METADATA = "upstream_anytls_client_metadata"
FEATURE_RF_DNS_RESOLVER_GROUP = "rf_dns_resolver_group"
FEATURE_RF_INGRESS_BINDING = "rf_ingress_binding"
FEATURE_RF_DNS_CHECK_START_VALIDATION = "rf_dns_check_start_validation"
PATCHED_SING_BOX_VERSION = "1.14.2-rf-dns.3"
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
        encoding="utf-8",
        errors="replace",
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


def read_git_config(git: str, repository: Path, key: str) -> str:
    completed = subprocess.run(
        [git, "config", "--local", "--get", key],
        cwd=str(repository),
        text=True,
        encoding="utf-8",
        errors="replace",
        stdout=subprocess.PIPE,
        stderr=subprocess.STDOUT,
    )
    if completed.returncode == 1:
        return ""
    if completed.returncode != 0:
        raise RuntimeError(f"unable to read Git config {key} in {repository}: {completed.stdout.strip()}")
    return completed.stdout.strip()


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
    if dest.exists():
        git_marker = dest / ".git"
        if git_marker.exists():
            autocrlf = read_git_config(git, dest, "core.autocrlf").lower()
            if autocrlf != "false":
                print(f"[WARN] local source cache {dest} does not guarantee LF checkout; refreshing")
            else:
                actual = run([git, "rev-parse", "HEAD"], cwd=dest).strip()
                if actual == expected_commit:
                    print(f"[INFO] reuse local source cache: {dest}")
                    run([git, "reset", "--hard", expected_commit], cwd=dest)
                    run([git, "clean", "-fdx"], cwd=dest)
                    return dest
                print(f"[WARN] local source cache {dest} is {actual}, expected {expected_commit}; refreshing")
        else:
            print(f"[WARN] local source cache path is not a git checkout: {dest}; refreshing")

    last_err: Exception | None = None
    for attempt in range(1, 4):
        if dest.exists():
            remove_tree(dest)
        try:
            dest.parent.mkdir(parents=True, exist_ok=True)
            run([git, "-c", "core.autocrlf=false", "clone", "--depth", "1", "--branch", tag, repo, str(dest)])
            run([git, "config", "--local", "core.autocrlf", "false"], cwd=dest)
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


def copy_patch_file(source_dir: Path, relative_path: str) -> None:
    source = REPO_ROOT / "patches" / "sing-box" / relative_path
    if not source.is_file():
        raise RuntimeError(f"patch source file not found: {source}")
    target = source_dir / relative_path
    target.parent.mkdir(parents=True, exist_ok=True)
    shutil.copyfile(source, target)


def gofmt_files(paths: list[Path]) -> None:
    gofmt = require_tool("gofmt")
    run([gofmt, "-w", *[str(path) for path in paths]])


def patch_sing_box(source_dir: Path) -> None:
    replace_once(
        source_dir / "cmd" / "sing-box" / "cmd_check.go",
        "\tif err == nil {\n\t\tinstance.Close()\n\t}\n\tcancel()\n\treturn err\n",
        "\tif err != nil {\n\t\tcancel()\n\t\treturn err\n\t}\n\terr = instance.PreStart()\n\tif err != nil {\n\t\tcancel()\n\t\treturn err\n\t}\n\terr = instance.Close()\n\tcancel()\n\treturn err\n",
    )
    replace_once(
        source_dir / "constant" / "dns.go",
        '\tDNSTypeTailscale   = "tailscale"\n',
        '\tDNSTypeTailscale                 = "tailscale"\n'
        '\tDNSTypeRFResolverGroup  = "rf_resolver_group"\n'
        '\tDNSTypeRFIngressBinding = "rf_ingress_binding"\n',
    )
    replace_once(
        source_dir / "option" / "dns.go",
        "type DHCPDNSServerOptions struct {\n\tLocalDNSServerOptions\n\tInterface string `json:\"interface,omitempty\"`\n}\n",
        "type DHCPDNSServerOptions struct {\n\tLocalDNSServerOptions\n\tInterface string `json:\"interface,omitempty\"`\n}\n\n"
        "type RFResolverGroupDNSServerOptions struct {\n"
        "\tPrimary                  []string           `json:\"primary,omitempty\"`\n"
        "\tFallback                 string             `json:\"fallback,omitempty\"`\n"
        "\tFallbackEnabled          bool               `json:\"fallback_enabled,omitempty\"`\n"
        "\tMode                     string             `json:\"mode,omitempty\"`\n"
        "\tProbeDomains             []string           `json:\"probe_domains,omitempty\"`\n"
        "\tFailureThreshold         int                `json:\"failure_threshold,omitempty\"`\n"
        "\tRecoverySuccessThreshold int                `json:\"recovery_success_threshold,omitempty\"`\n"
        "\tProbeInterval            badoption.Duration `json:\"probe_interval,omitempty\"`\n"
        "\tUnhealthyCooldown        badoption.Duration `json:\"unhealthy_cooldown,omitempty\"`\n"
        "\tFallbackTTLCap           badoption.Duration `json:\"fallback_ttl_cap,omitempty\"`\n"
        "}\n\n"
        "type RFIngressBindingDNSServerOptions struct {\n"
        "\tBindingFile   string `json:\"binding_file\"`\n"
        "\tLogicalDomain string `json:\"logical_domain\"`\n"
        "\tQueryName     string `json:\"query_name,omitempty\"`\n"
        "\tResolver      string `json:\"resolver\"`\n"
        "}\n",
    )
    replace_once(
        source_dir / "include" / "registry.go",
        '\t"github.com/sagernet/sing-box/dns/transport/local"\n',
        '\t"github.com/sagernet/sing-box/dns/transport/local"\n'
        '\t"github.com/sagernet/sing-box/dns/transport/rfbinding"\n'
        '\t"github.com/sagernet/sing-box/dns/transport/rfgroup"\n',
    )
    replace_once(
        source_dir / "include" / "registry.go",
        "\tlocal.RegisterTransport(registry)\n",
        "\tlocal.RegisterTransport(registry)\n"
        "\trfbinding.RegisterTransport(registry)\n"
        "\trfgroup.RegisterTransport(registry)\n",
    )
    copy_patch_file(source_dir, "dns/transport/rfgroup/rfgroup.go")
    copy_patch_file(source_dir, "dns/transport/rfgroup/rfgroup_test.go")
    copy_patch_file(source_dir, "dns/transport/rfbinding/rfbinding.go")
    copy_patch_file(source_dir, "dns/transport/rfbinding/rfbinding_test.go")


def verify_sing_anytls_dependency(sing_box_dir: Path, sing_anytls_dir: Path) -> None:
    go_mod = (sing_box_dir / "go.mod").read_text(encoding="utf-8")
    expected_requirement = f"github.com/anytls/sing-anytls {SING_ANYTLS_VERSION}"
    if expected_requirement not in go_mod:
        raise RuntimeError(
            f"sing-box {SING_BOX_VERSION} does not require the controlled dependency {expected_requirement}"
        )
    actual = run([require_tool("git"), "rev-parse", "HEAD"], cwd=sing_anytls_dir).strip()
    if actual != SING_ANYTLS_COMMIT:
        raise RuntimeError(
            f"sing-anytls {SING_ANYTLS_VERSION} resolved to {actual}, expected {SING_ANYTLS_COMMIT}"
        )


def build(args: argparse.Namespace) -> None:
    workdir = Path(args.workdir).resolve()
    src_root = workdir / "src"
    sing_box_dir = src_root / f"sing-box-{SING_BOX_VERSION.removeprefix('v')}"
    sing_anytls_dir = src_root / f"sing-anytls-{SING_ANYTLS_VERSION.removeprefix('v')}"

    clone_exact(SING_BOX_REPO, SING_BOX_VERSION, SING_BOX_COMMIT, sing_box_dir)
    clone_exact(SING_ANYTLS_REPO, SING_ANYTLS_VERSION, SING_ANYTLS_COMMIT, sing_anytls_dir)
    verify_sing_anytls_dependency(sing_box_dir, sing_anytls_dir)
    patch_sing_box(sing_box_dir)

    go = require_tool("go")
    gofmt_files([
        sing_box_dir / "cmd" / "sing-box" / "cmd_check.go",
        sing_box_dir / "constant" / "dns.go",
        sing_box_dir / "dns" / "transport" / "rfbinding" / "rfbinding.go",
        sing_box_dir / "dns" / "transport" / "rfbinding" / "rfbinding_test.go",
        sing_box_dir / "dns" / "transport" / "rfgroup" / "rfgroup.go",
        sing_box_dir / "dns" / "transport" / "rfgroup" / "rfgroup_test.go",
        sing_box_dir / "include" / "registry.go",
        sing_box_dir / "option" / "dns.go",
    ])

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
        "schema": "rf.sing_box_build.v1",
        "name": "sing-box",
        "patch_id": PATCH_ID,
        "features": [
            FEATURE_UPSTREAM_ANYTLS_CLIENT_METADATA,
            FEATURE_RF_DNS_RESOLVER_GROUP,
            FEATURE_RF_INGRESS_BINDING,
            FEATURE_RF_DNS_CHECK_START_VALIDATION,
        ],
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
    parser = argparse.ArgumentParser(description="Build RF sing-box with the controlled DNS resolver patch.")
    default_workdir = Path(__file__).resolve().parent / "work"
    default_output = Path(__file__).resolve().parent / "dist" / "sing-box-linux-amd64"
    default_manifest = Path(__file__).resolve().parent / "dist" / "sing-box-linux-amd64.rf-dns.json"
    parser.add_argument("--workdir", default=str(default_workdir))
    parser.add_argument("--output", default=str(default_output))
    parser.add_argument("--manifest", default=str(default_manifest))
    parser.add_argument("--goos", default="linux")
    parser.add_argument("--goarch", default="amd64")
    parser.add_argument("--tags", default=os.environ.get("RF_SING_BOX_TAGS", DEFAULT_TAGS))
    return parser.parse_args(argv)


if __name__ == "__main__":
    try:
        build(parse_args(sys.argv[1:]))
    except Exception as exc:
        print(f"[ERROR] {exc}", file=sys.stderr)
        raise SystemExit(1) from exc
