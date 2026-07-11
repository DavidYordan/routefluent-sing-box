#!/usr/bin/env python3
"""Runtime smoke for RouteFluent DNS resolver groups.

The test is intentionally local-only and deterministic. It starts:

* a loopback HTTP target used by `sing-box tools fetch`;
* a loopback HTTPS DoH mock that resolves any A query to 127.0.0.1.

Then it validates three runtime dial paths:

1. primary DoH resolves a custom domain and fetch succeeds;
2. strict mode with dead DoH and no fallback fails closed;
3. local fallback mode with dead DoH resolves localhost and fetch succeeds.
"""

from __future__ import annotations

import argparse
import base64
import contextlib
import http.server
import json
import os
from pathlib import Path
import socket
import ssl
import struct
import subprocess
import sys
import tempfile
import threading
import time
from typing import Iterator
from urllib.parse import parse_qs, urlparse


CERT_PEM = """-----BEGIN CERTIFICATE-----
MIIC1DCCAbygAwIBAgIUKw0hvAteIDuKH4EEae9A5ITGlDUwDQYJKoZIhvcNAQEL
BQAwFDESMBAGA1UEAwwJbG9jYWxob3N0MB4XDTI2MDcxMDExNTYzOFoXDTM2MDcw
ODExNTYzOFowFDESMBAGA1UEAwwJbG9jYWxob3N0MIIBIjANBgkqhkiG9w0BAQEF
AAOCAQ8AMIIBCgKCAQEA1YybYf9ITUTEOvM9+iwMtl4FTZPalMRUhaQ8eIcpAh46
WD2k5dJfs4XIRGAGquGNB2wSSz0TQox9HlHAhKKNdIP+zv6JGcr9f5ZG5W7wD4H/
+QZRip+/rCRwzIAs0LgyaR/Az1x9o7uJzryC6wSiO1EvUvXUinlZx7rPs0NaiQM1
kpNfnPlllxvsQPI/YmDMVz/vSpPhrxfiCuZOmn+Ds8rlLLa2CTzACGWqpwawykSq
2Wo+7iTZa10UbUegKqlVtk4xgETUPXFgAkqYRScLk72tgTXi6nHi77C0QY+KEbMW
QATApvdvmtWcn+Gm5/cHI2z1xR/b5AQeyOT3p2YG4QIDAQABox4wHDAaBgNVHREE
EzARgglsb2NhbGhvc3SHBH8AAAEwDQYJKoZIhvcNAQELBQADggEBACSmOxwglhCw
+vgF4ey48oj9da0aK74GauteDn7b4NPHVQ1oDozL0AnF/Tne5mBfrsHI9qXIYn46
E5d6vChJKjQ6OfkufdZ/gvfCKlrajIORYJAnCXfSl9HjcUE2nsUHeIqUc4K5NyaO
tskP2hD3VX4f4m+ibXzAgJPrA9UPymgije/UCC43vSnE5XY0c25K1WIpP/LSCZg3
mkr+OwDg9CfltrLfbHizPEPAnrAVJuSCqSAha4GkWBNpPg14Z7AllBE/OGDNOW4E
QfUvnruVg7giR1sZKMi6ANeDEa0MACD9kLmaI/aB8hdiRtbxIauTWixk8jYGGV8k
gS07/sOo7ic=
-----END CERTIFICATE-----
"""

KEY_PEM = """-----BEGIN RSA PRIVATE KEY-----
MIIEowIBAAKCAQEA1YybYf9ITUTEOvM9+iwMtl4FTZPalMRUhaQ8eIcpAh46WD2k
5dJfs4XIRGAGquGNB2wSSz0TQox9HlHAhKKNdIP+zv6JGcr9f5ZG5W7wD4H/+QZR
ip+/rCRwzIAs0LgyaR/Az1x9o7uJzryC6wSiO1EvUvXUinlZx7rPs0NaiQM1kpNf
nPlllxvsQPI/YmDMVz/vSpPhrxfiCuZOmn+Ds8rlLLa2CTzACGWqpwawykSq2Wo+
7iTZa10UbUegKqlVtk4xgETUPXFgAkqYRScLk72tgTXi6nHi77C0QY+KEbMWQATA
pvdvmtWcn+Gm5/cHI2z1xR/b5AQeyOT3p2YG4QIDAQABAoIBAE4ZPjp4wlh/7cQh
cWks8vk/KXFVwXrm0oKNrg/mXnkH1Q6wfL4QUi+1nahj9gxIsOsl+wrJK2ILPzb3
bxES4eja3TWWoU1tj4g5zXPbPrBtOtA4H1ozUkYCjb48oiczNjx8AGfVy5012RBi
oP6Sk0JeTpBol3KNLuh8ybklyNaSSoPBft1bmWsjhg9J4cmJ7nM70rAcjfRs4mFp
1kZ4xwnp174uiOTsJaIDnnXDIrbCBhmsMxeyKy3L6VkolDcRN8YGD8MBGnhXpuFw
kwoqgEL0EPxdLxJMtvBosYhcbXJDQv56WTEsRBG8eHauf6UPiOPxhusN1c3e8l//
D/fJAIECgYEA+XwOxgKIGau4BTUhOwrf+O5ZD53f5pJkPWzpHCu8E2BY4uT9+/0Q
BYQx/eFh6TrZe66lHkjvGntCdWMcbcfEivRwrZjh2NvfPq0ew6j+1YUq09JU2ll5
QMZBnpOaqk0Jh0WxmyJY6ovRktAMmWwylNE+3H5mtm7Nh6ahoE9d6CMCgYEA2yBN
M8MssgreFE2cinUdG+Y6jH4Ok15hXr1NmzAOJXIGq2FJ6Vkxvh/1dtcO0IMQTUpc
Z1qky+0fgDl8ZleOgK6/jAKMtQrnlfTgwo0lBhAQweyXbTqLKYqBhUMrU9HQS6B2
cJas/WPGEMjsDkzHtyeEHRKMtF8d+psR8/sj4ysCgYBIQaLxvGf4r2BTuciPFh46
NaX7vOjNGdcIZ1O4gf4tynjT9iiNZATQ5DGqay11ZEL67GEMPWlqzQo5f2QLc5E8
AYHk9WNr8Hpe57sCRh7Qboox4TgMKV/R39m/eNaRvlAlKo0+9hq0i5w+Hh2YDds0
eMnxCHbtHXhPcnZbPMFg6wKBgEp7HEKv4I1T7FByYPce/5nwE46VelbVCbuuFKbf
Gq/XpjSyiPDsBGBfdIvEZaCyK+RZljb7NHCsVLy8zW/r6uAKhckNRM50umraJVt7
pk39P/Tqej4CXDLuhT/KqPuAoZBTJm72iC5Ir5Yc9T+XEXtLzzgiifj1K7d25g9M
GdKpAoGBAI9imFToy0KU2CA3ncF5tR1hMmCwb6eAvJwB2HQP9z4XhIHh56YU0pK/
sXyXbP7VPeh8hVtY4pqmL5dKzeO9lm3cm8kYwqqtIMTKOPHB79nnpnjRv/i/cHSl
xXxo/M0BnEz1NQva4FsXCy4AbVIla1ua5nEnoOscjPv2D2VjMyr9
-----END RSA PRIVATE KEY-----
"""


class ThreadingHTTPServer(http.server.ThreadingHTTPServer):
    daemon_threads = True
    allow_reuse_address = True


class TargetHandler(http.server.BaseHTTPRequestHandler):
    def do_GET(self) -> None:
        self.send_response(200)
        self.send_header("content-type", "text/plain")
        self.end_headers()
        self.wfile.write(b"routefluent-smoke-ok\n")

    def log_message(self, _format: str, *_args: object) -> None:
        return


class DoHHandler(http.server.BaseHTTPRequestHandler):
    def do_GET(self) -> None:
        parsed = urlparse(self.path)
        query = parse_qs(parsed.query)
        encoded = query.get("dns", [""])[0]
        if not encoded:
            self.send_error(400, "missing dns query")
            return
        padding = "=" * ((4 - len(encoded) % 4) % 4)
        request = base64.urlsafe_b64decode((encoded + padding).encode("ascii"))
        self.write_dns_response(request)

    def do_POST(self) -> None:
        length = int(self.headers.get("content-length", "0"))
        request = self.rfile.read(length)
        self.write_dns_response(request)

    def write_dns_response(self, request: bytes) -> None:
        response = dns_a_response(request)
        self.send_response(200)
        self.send_header("content-type", "application/dns-message")
        self.send_header("content-length", str(len(response)))
        self.end_headers()
        self.wfile.write(response)

    def log_message(self, _format: str, *_args: object) -> None:
        return


def dns_a_response(request: bytes) -> bytes:
    if len(request) < 12:
        raise ValueError("DNS request is too short")
    transaction_id = request[:2]
    qdcount = struct.unpack("!H", request[4:6])[0]
    if qdcount != 1:
        raise ValueError("expected one DNS question")
    offset = 12
    while offset < len(request) and request[offset] != 0:
        offset += 1 + request[offset]
    if offset >= len(request):
        raise ValueError("DNS question is malformed")
    question_end = offset + 5
    question = request[12:question_end]
    header = transaction_id + b"\x81\x80" + b"\x00\x01\x00\x01\x00\x00\x00\x00"
    answer = b"\xc0\x0c" + struct.pack("!HHIH", 1, 1, 30, 4) + socket.inet_aton("127.0.0.1")
    return header + question + answer


def free_port() -> int:
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as sock:
        sock.bind(("127.0.0.1", 0))
        return int(sock.getsockname()[1])


@contextlib.contextmanager
def serve_http(
    handler: type[http.server.BaseHTTPRequestHandler],
    *,
    bind_host: str = "127.0.0.1",
    use_tls: bool = False,
) -> Iterator[int]:
    port = free_port()
    server = ThreadingHTTPServer((bind_host, port), handler)
    temp_dir: tempfile.TemporaryDirectory[str] | None = None
    if use_tls:
        temp_dir = tempfile.TemporaryDirectory()
        cert_path = Path(temp_dir.name) / "cert.pem"
        key_path = Path(temp_dir.name) / "key.pem"
        cert_path.write_text(CERT_PEM, encoding="ascii")
        key_path.write_text(KEY_PEM, encoding="ascii")
        context = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
        context.load_cert_chain(str(cert_path), str(key_path))
        server.socket = context.wrap_socket(server.socket, server_side=True)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    try:
        yield port
    finally:
        server.shutdown()
        server.server_close()
        thread.join(timeout=5)
        if temp_dir is not None:
            temp_dir.cleanup()


def routefluent_config(
    *,
    doh_port: int,
    fallback_enabled: bool,
    fallback: bool,
    http_port: int,
) -> dict[str, object]:
    primary: dict[str, object] = {
        "tag": "mock-doh",
        "type": "https",
        "server": "127.0.0.1",
        "server_port": doh_port,
        "path": "/dns-query",
        "tls": {
            "enabled": True,
            "insecure": True,
            "server_name": "localhost",
        },
    }
    servers: list[dict[str, object]] = [
        {"tag": "local-system", "type": "local"},
        primary,
    ]
    group: dict[str, object] = {
        "tag": "rf-runtime",
        "type": "routefluent_resolver_group",
        "primary": ["mock-doh"],
    }
    if fallback:
        group["fallback"] = "local-system"
    if fallback_enabled:
        group["fallback_enabled"] = True
        group["probe_domains"] = ["localhost"]
        group["failure_threshold"] = 1
        group["recovery_success_threshold"] = 1
        group["probe_interval"] = "1s"
        group["unhealthy_cooldown"] = "1s"
        group["fallback_ttl_cap"] = "1s"
    servers.append(group)
    return {
        "log": {"level": "debug", "timestamp": False},
        "dns": {"servers": servers, "final": "rf-runtime"},
        "outbounds": [
            {
                "type": "direct",
                "tag": "direct",
                "domain_resolver": {
                    "server": "rf-runtime",
                    "strategy": "ipv4_only",
                },
            }
        ],
        "route": {
            "default_domain_resolver": {
                "server": "rf-runtime",
                "strategy": "ipv4_only",
            },
            "final": "direct",
        },
        "experimental": {"cache_file": {"enabled": False}},
    }


def write_config(path: Path, config: dict[str, object]) -> None:
    path.write_text(json.dumps(config, indent=2) + "\n", encoding="utf-8")


def run_fetch(sing_box: Path, config: Path, url: str, timeout: int = 12) -> subprocess.CompletedProcess[str]:
    return subprocess.run(
        [str(sing_box), "-c", str(config), "tools", "fetch", "-o", "direct", url],
        text=True,
        encoding="utf-8",
        errors="replace",
        stdout=subprocess.PIPE,
        stderr=subprocess.STDOUT,
        timeout=timeout,
    )


def choose_local_resolvable_host(port: int) -> str:
    # Prefer localhost: systemd-resolved on GitHub runners can reject the runner
    # hostname even when Python getaddrinfo accepts it through NSS.
    candidates = ["localhost", socket.gethostname(), socket.getfqdn()]
    seen: set[str] = set()
    for candidate in candidates:
        candidate = candidate.strip()
        if not candidate or candidate in seen:
            continue
        seen.add(candidate)
        try:
            infos = socket.getaddrinfo(candidate, port, socket.AF_UNSPEC, socket.SOCK_STREAM)
        except OSError:
            continue
        for family, socktype, proto, _, address in infos:
            try:
                with socket.socket(family, socktype, proto) as sock:
                    sock.settimeout(1)
                    sock.connect(address)
                return candidate
            except OSError:
                continue
    raise RuntimeError("no locally resolvable hostname can reach the smoke HTTP server")


def require_success(label: str, result: subprocess.CompletedProcess[str]) -> None:
    if result.returncode != 0 or "routefluent-smoke-ok" not in result.stdout:
        raise RuntimeError(f"{label} failed unexpectedly with exit {result.returncode}:\n{result.stdout}")
    print(f"[OK] {label}")


def require_failure(label: str, result: subprocess.CompletedProcess[str], expected: str) -> None:
    if result.returncode == 0:
        raise RuntimeError(f"{label} succeeded unexpectedly:\n{result.stdout}")
    if expected not in result.stdout:
        raise RuntimeError(f"{label} failed without expected evidence {expected!r}:\n{result.stdout}")
    print(f"[OK] {label}")


def main() -> int:
    parser = argparse.ArgumentParser(description="Run local runtime smoke checks for RouteFluent DNS resolver group.")
    parser.add_argument("--sing-box", default="dist/sing-box-linux-amd64", help="Path to the built sing-box binary.")
    args = parser.parse_args()

    sing_box = Path(args.sing_box).resolve()
    if not sing_box.is_file():
        print(f"[ERROR] sing-box binary not found: {sing_box}", file=sys.stderr)
        return 2

    with tempfile.TemporaryDirectory() as temp:
        temp_dir = Path(temp)
        with serve_http(TargetHandler, bind_host="0.0.0.0") as http_port, serve_http(DoHHandler, use_tls=True) as doh_port:
            local_host = choose_local_resolvable_host(http_port)
            primary_config = temp_dir / "primary.json"
            write_config(
                primary_config,
                routefluent_config(
                    doh_port=doh_port,
                    fallback_enabled=False,
                    fallback=False,
                    http_port=http_port,
                ),
            )
            require_success(
                "primary DoH resolves outbound server domain",
                run_fetch(sing_box, primary_config, f"http://rf-primary.test:{http_port}/"),
            )

            strict_config = temp_dir / "strict.json"
            write_config(
                strict_config,
                routefluent_config(
                    doh_port=9,
                    fallback_enabled=False,
                    fallback=False,
                    http_port=http_port,
                ),
            )
            require_failure(
                "strict mode fails closed when primary DoH is unavailable",
                run_fetch(sing_box, strict_config, f"http://{local_host}:{http_port}/"),
                "primary DoH resolvers unavailable",
            )

            if os.name == "nt":
                print("[SKIP] local fallback runtime success: sing-box local resolver on Windows does not resolve local hostnames consistently")
            else:
                fallback_config = temp_dir / "fallback.json"
                write_config(
                    fallback_config,
                    routefluent_config(
                        doh_port=9,
                        fallback_enabled=True,
                        fallback=True,
                        http_port=http_port,
                    ),
                )
                require_success(
                    "local fallback resolves when primary DoH is unavailable",
                    run_fetch(sing_box, fallback_config, f"http://{local_host}:{http_port}/"),
                )

    return 0


if __name__ == "__main__":
    raise SystemExit(main())
