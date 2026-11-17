#!/usr/bin/env python3
"""Exercise native TCP attribution, two simultaneous entry IPs, process isolation,
close, and same-PID reload through the root-only observation socket. All DNS,
SOCKS peers and payloads are local synthetic fixtures; no external request.
"""
from __future__ import annotations

import hashlib
import http.client
import json
import os
from pathlib import Path
import signal
import socket
import socketserver
import struct
import subprocess
import sys
import tempfile
import threading
import time


def read_exact(conn, count):
    value = bytearray()
    while len(value) < count:
        part = conn.recv(count - len(value))
        if not part:
            raise EOFError("fixture socket closed")
        value.extend(part)
    return bytes(value)


class SocksPeer(socketserver.BaseRequestHandler):
    def handle(self):
        try:
            version, count = read_exact(self.request, 2)
            assert version == 5
            read_exact(self.request, count)
            self.request.sendall(b"\x05\x00")
            version, command, _, kind = read_exact(self.request, 4)
            assert version == 5 and command == 1
            size = {1: 4, 4: 16}.get(kind)
            if kind == 3:
                size = read_exact(self.request, 1)[0]
            read_exact(self.request, size + 2)
            self.request.sendall(b"\x05\x00\x00\x01\x7f\x00\x00\x01\x00\x00")
            while value := self.request.recv(4096):
                self.request.sendall(value)
        except (EOFError, OSError):
            pass


class TCPServer(socketserver.ThreadingTCPServer):
    allow_reuse_address = True
    daemon_threads = True


class DNSPeer(socketserver.BaseRequestHandler):
    def handle(self):
        raw, conn = self.request
        end = 12
        while raw[end]:
            end += raw[end] + 1
        end += 5
        question = raw[12:end]
        kind = struct.unpack("!H", raw[end - 4:end - 2])[0]
        answers = 1 if kind == 1 else 0
        response = raw[:2] + struct.pack("!HHHHH", 0x8180, 1, answers, 0, 0) + question
        if answers:
            response += b"\xc0\x0c" + struct.pack("!HHIH", 1, 1, 1, 4) + socket.inet_aton(self.server.answer)
        conn.sendto(response, self.client_address)


def free_port():
    with socket.socket() as conn:
        conn.bind(("127.0.0.1", 0))
        return conn.getsockname()[1]


def observe(pid, path):
    with socket.socket(socket.AF_UNIX) as conn:
        conn.settimeout(2)
        conn.connect("\0rf-business-current-" + str(pid))
        conn.sendall(("GET " + path + " HTTP/1.1\r\nHost: native\r\nConnection: close\r\n\r\n").encode())
        response = http.client.HTTPResponse(conn)
        response.begin()
        assert response.status == 200, response.status
        assert response.getheader("X-Rf-Native-Pid") == str(pid)
        body = response.read()
        return response.getheader("X-Rf-Config-Sha256"), body


def wait_for(predicate, process, log):
    end = time.monotonic() + 8
    last = None
    while time.monotonic() < end:
        if process.poll() is not None:
            raise RuntimeError("native fixture exited: " + log.read_text()[-4096:])
        try:
            result = predicate()
            if result:
                return result
        except (OSError, http.client.HTTPException) as error:
            last = str(error)
        time.sleep(0.02)
    raise AssertionError("native observation did not reach expected fixture result: " + str(last))


def open_stream(port):
    conn = socket.create_connection(("127.0.0.1", port), timeout=3)
    try:
        conn.sendall(b"\x05\x01\x00")
        assert read_exact(conn, 2) == b"\x05\x00"
        # Documentation-only target: the local SOCKS peer echoes, never dials it.
        conn.sendall(b"\x05\x01\x00\x01" + socket.inet_aton("198.51.100.55") + b"\x01\xbb")
        assert read_exact(conn, 10)[:2] == b"\x05\x00"
        payload = b"rf-native-owned-stream"
        conn.sendall(payload)
        assert read_exact(conn, len(payload)) == payload
        return conn
    except BaseException:
        conn.close()
        raise


def run(binary, root):
    servers, threads, processes, files, clients = [], [], [], [], []
    try:
        first_peer = TCPServer(("127.0.0.1", 0), SocksPeer)
        servers.append(first_peer)
        second_peer = TCPServer(("127.0.0.2", first_peer.server_address[1]), SocksPeer)
        servers.append(second_peer)
        dns = socketserver.UDPServer(("127.0.0.1", 0), DNSPeer)
        dns.answer = "127.0.0.1"
        servers.append(dns)
        for server in servers:
            thread = threading.Thread(target=server.serve_forever)
            thread.start()
            threads.append(thread)
        binding = root / "line-7.json"
        binding.write_text(json.dumps({"schema": "rf.device_line_ingress_binding.v1", "tenant_id": 3, "device_id": 8, "line_asset_id": 7, "endpoint_family_id": 11, "logical_domain": "entry.test", "mode": "resolver", "connect_ipv4": None, "binding_revision": 1, "last_server_choice_revision": 0, "source": "fixture", "updated_at": "2026-11-17T00:00:00Z"}))
        port = free_port()
        config = {"log": {"level": "warn"}, "dns": {"disable_cache": True, "servers": [{"type": "udp", "tag": "fixture-dns", "server": "127.0.0.1", "server_port": dns.server_address[1]}, {"type": "rf_ingress_binding", "tag": "entry-dns", "binding_file": str(binding), "logical_domain": "entry.test", "resolver": "fixture-dns"}]}, "inbounds": [{"type": "socks", "tag": "fixture-in", "listen": "127.0.0.1", "listen_port": port}], "outbounds": [{"type": "socks", "tag": "entry-a", "server": "entry.test", "server_port": first_peer.server_address[1], "version": "5", "domain_resolver": {"server": "entry-dns", "strategy": "ipv4_only"}}], "route": {"final": "entry-a"}}
        path = root / "business.json"
        path.write_text(json.dumps(config))
        log = root / "business.log"
        log_file = log.open("wb")
        files.append(log_file)
        business = subprocess.Popen([str(binary), "run", "--rf-observe-current", "-c", str(path)], stdout=log_file, stderr=log_file)
        processes.append(business)
        digest, current = wait_for(lambda: observe(business.pid, "/v1/current"), business, log)
        assert hashlib.sha256(current).hexdigest() == digest

        def connections():
            sha, body = observe(business.pid, "/v1/connections")
            assert sha == digest
            result = json.loads(body)
            assert result["schema"] == "rf.native_connections.v1"
            return result["connections"]

        assert connections() == []
        a = open_stream(port)
        clients.append(a)
        first = connections()
        assert first == [{"outbound_tag": "entry-a", "peer_ipv4": "127.0.0.1", "peer_port": first_peer.server_address[1], "count": 1}], first
        dns.answer = "127.0.0.2"
        b = open_stream(port)
        clients.append(b)
        both = connections()
        assert len(both) == 2 and {v["peer_ipv4"] for v in both} == {"127.0.0.1", "127.0.0.2"} and all(v["count"] == 1 for v in both), both

        # A second native process reaches the same upstream. It has no observer
        # and cannot add connections to the business instance's snapshot.
        worker_port = free_port()
        config["inbounds"][0]["listen_port"] = worker_port
        worker_path = root / "worker.json"
        worker_path.write_text(json.dumps(config))
        worker_log = root / "worker.log"
        worker_file = worker_log.open("wb")
        files.append(worker_file)
        worker = subprocess.Popen([str(binary), "run", "-c", str(worker_path)], stdout=worker_file, stderr=worker_file)
        processes.append(worker)
        c = wait_for(lambda: open_stream(worker_port), worker, worker_log)
        clients.append(c)
        outsider = socket.create_connection(first_peer.server_address)
        clients.append(outsider)
        assert connections() == both
        a.close()
        wait_for(lambda: connections() == [both[1]], business, log)
        b.close()
        wait_for(lambda: connections() == [], business, log)

        config["inbounds"][0]["listen_port"] = port
        config["log"]["level"] = "error"
        path.write_text(json.dumps(config))
        business.send_signal(signal.SIGHUP)
        new_digest, new_current = wait_for(lambda: (value if (value := observe(business.pid, "/v1/current"))[0] != digest else None), business, log)
        assert hashlib.sha256(new_current).hexdigest() == new_digest
        digest = new_digest
        assert connections() == []
        print(json.dumps({"scope": "actual Linux native DNS/binding/SOCKS/TCP and root-only HTTP; synthetic local peers", "pid": business.pid, "first": first, "simultaneous": both, "other_native_and_process_socket_excluded": True, "last_close_retired": True, "same_pid_reload_cleared": True, "config_sha_after_reload": digest}), flush=True)
    finally:
        for conn in clients:
            conn.close()
        for process in reversed(processes):
            if process.poll() is None:
                process.terminate()
            try:
                process.wait(timeout=8)
            except subprocess.TimeoutExpired:
                process.kill()
                process.wait()
        for index, server in enumerate(servers):
            if index < len(threads):
                server.shutdown()
            server.server_close()
        for thread in threads:
            thread.join()
        for file in files:
            file.close()


if __name__ == "__main__":
    with tempfile.TemporaryDirectory(prefix="rf-native-connections-", dir=sys.argv[2]) as temporary:
        run(Path(sys.argv[1]).resolve(), Path(temporary).resolve())
