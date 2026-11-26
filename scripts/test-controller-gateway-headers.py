#!/usr/bin/env python3
"""Gateway security headers on normal and error responses, in both Caddy configs.

Runs the pinned Caddy image with the repository's Standalone Caddyfile and the
Integrated Caddyfile/site.caddy. No control plane runs, so proxied paths fail
inside Caddy (502) and must carry the same headers as a static response.
"""
import os
from pathlib import Path
import secrets
import socket
import ssl
import subprocess
import tempfile
import time

ROOT = Path(__file__).resolve().parents[1]
CADDY = "caddy:2.11.9-alpine@sha256:003a97808e781aa9f7e75cc2f43d34d461099031faf5e20faefb9f61126836f0"
HOST = "controller.headers.test"
SUBNET, CLIENT_GATEWAY = "198.18.94.0/24", "198.18.94.1"
EXPECTED = {
    "strict-transport-security": "max-age=31536000; includeSubDomains",
    "x-content-type-options": "nosniff",
    "x-frame-options": "DENY",
    "referrer-policy": "no-referrer",
    "content-security-policy": "default-src 'self'; object-src 'none'; base-uri 'none'; "
                               "frame-ancestors 'none'; form-action 'self'",
}


def run(*args, check=True):
    result = subprocess.run(args, text=True, stdout=subprocess.PIPE, stderr=subprocess.STDOUT)
    if check and result.returncode:
        raise RuntimeError(f"{args[:4]}: {result.stdout}")
    return result.stdout


def fetch(address, path, context, proxy, etag=None):
    with socket.create_connection((address, 8443), timeout=5) as sock:
        if proxy:
            # Integrated trusts PROXY only from Edge; the client here is the
            # network gateway address configured as OCSERV_EDGE_GATEWAY_IP.
            sock.sendall(f"PROXY TCP4 203.0.113.7 {address} 40000 443\r\n".encode())
        with context.wrap_socket(sock, server_hostname=HOST) as tls:
            conditional = f"If-None-Match: {etag}\r\n" if etag else ""
            tls.sendall(f"GET {path} HTTP/1.1\r\nHost: {HOST}\r\nAccept-Encoding: gzip\r\n{conditional}Connection: close\r\n\r\n".encode())
            data = b""
            while part := tls.recv(65536):
                data += part
    head = data.split(b"\r\n\r\n", 1)[0].decode("latin-1").split("\r\n")
    headers = {}
    for line in head[1:]:
        name, value = line.split(":", 1)
        headers.setdefault(name.strip().lower(), []).append(value.strip())
    return int(head[0].split()[1]), headers


def main():
    work = Path(tempfile.mkdtemp(prefix="ocservia-gateway-headers-"))
    os.chmod(work, 0o755)
    name = "ocservia-gateway-headers-" + secrets.token_hex(4)
    containers = []
    try:
        run("openssl", "req", "-x509", "-newkey", "rsa:2048", "-nodes", "-days", "1",
            "-subj", f"/CN={HOST}", "-addext", f"subjectAltName=DNS:{HOST}",
            "-keyout", str(work / "tls.key"), "-out", str(work / "tls.crt"))
        os.chmod(work / "tls.key", 0o444)
        (work / "srv").mkdir()
        (work / "srv" / "index.html").write_text("<!doctype html><title>headers</title>")
        asset = "/assets/index-Abcd1234.js"
        (work / "srv" / "assets").mkdir()
        (work / "srv" / asset[1:]).write_text("/* gateway asset */" * 100)
        context = ssl.create_default_context(cafile=str(work / "tls.crt"))
        context.minimum_version = ssl.TLSVersion.TLSv1_2
        run("docker", "network", "create", "--subnet", SUBNET, "--gateway", CLIENT_GATEWAY, name)
        common = ["-v", f"{work}/tls.crt:/run/secrets/tls_certificate:ro",
                  "-v", f"{work}/tls.key:/run/secrets/tls_private_key:ro",
                  "-v", f"{work}/srv:/srv:ro", "-e", f"OCSERV_PUBLIC_HOST={HOST}"]
        modes = {
            "standalone": (["-v", f"{ROOT}/deploy/production/Caddyfile:/etc/caddy/Caddyfile:ro"], False),
            "integrated": (["-e", f"OCSERV_EDGE_GATEWAY_IP={CLIENT_GATEWAY}",
                            "-v", f"{ROOT}/deploy/production/integrated/Caddyfile:/etc/caddy/Caddyfile:ro",
                            "-v", f"{ROOT}/deploy/production/integrated/site.caddy:/etc/caddy/site.caddy:ro"], True),
        }
        for mode, (mounts, proxy) in modes.items():
            container = f"{name}-{mode}"
            containers.append(container)
            run("docker", "run", "-d", "--name", container, "--network", name, *common, *mounts, CADDY)
            address = run("docker", "inspect", "-f", "{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}",
                          container).strip()
            deadline = time.monotonic() + 30
            while True:
                try:
                    fetch(address, "/", context, proxy)
                    break
                except OSError:
                    if time.monotonic() > deadline:
                        raise
                    time.sleep(0.5)
            for path, status in (("/", 200), ("/nodes/deep-link", 200), ("/api/v1/auth/session", 502),
                                 ("/readyz", 502)):
                got, headers = fetch(address, path, context, proxy)
                assert got == status, (mode, path, got)
                for header, value in EXPECTED.items():
                    assert headers.get(header) == [value], (mode, path, header, headers.get(header))
                assert "content-security-policy-report-only" not in headers, (mode, path)
                assert "server" not in headers, (mode, path, headers.get("server"))
            for path, policy in ((asset, "public, max-age=31536000, immutable"),
                                 ("/assets/missing-Ab3dE6gH.js", "no-cache"), ("/", "no-cache")):
                got, headers = fetch(address, path, context, proxy)
                assert got == 200 and headers.get("cache-control") == [policy], (mode, path, got, headers)
                if path == asset:
                    assert headers.get("content-encoding") == ["gzip"], (mode, path, headers)
                else:
                    assert "content-encoding" not in headers, (mode, path, headers)
                got, _ = fetch(address, path, context, proxy, headers["etag"][0])
                assert got == 304, (mode, path, got)
            print(f"PASS {mode}: security headers on static, fallback and 502 responses")
    finally:
        for container in containers:
            run("docker", "rm", "-f", container, check=False)
        run("docker", "network", "rm", name, check=False)
        for path in sorted(work.rglob("*"), reverse=True):
            path.rmdir() if path.is_dir() else path.unlink()
        work.rmdir()


if __name__ == "__main__":
    main()
