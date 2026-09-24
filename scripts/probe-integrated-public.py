#!/usr/bin/env python3
"""Probe one authorized public P1 fixture using its exact test CA and SNI."""
import argparse
import http.client
import ipaddress
import json
import os
from pathlib import Path
import socket
import ssl


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--address", required=True)
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    assert ipaddress.ip_address(args.address).is_global, "a public IP is required"
    context = ssl.create_default_context(cadata=os.environ["P1_PUBLIC_CA_PEM"])

    def request(name, path, extra=""):
        with socket.create_connection((args.address, 443), timeout=10) as sock:
            with context.wrap_socket(sock, server_hostname=name) as tls:
                tls.sendall((f"GET {path} HTTP/1.1\r\nHost: {name}\r\n"
                             f"Connection: close\r\n{extra}\r\n").encode())
                response = http.client.HTTPResponse(tls)
                response.begin()
                return response.status, response.read().decode()

    status, observed = request("controller.p1.test", "/api/ip")
    assert status == 200, status
    client_ip = ipaddress.ip_address(observed.strip())
    assert client_ip.is_global, f"not a public client source: {client_ip}"
    status, spoofed = request("controller.p1.test", "/api/ip",
                              "X-Ocservia-Client-IP: 203.0.113.66\r\nX-Forwarded-For: 203.0.113.66\r\n")
    assert status == 200 and spoofed.strip() == str(client_ip), "forwarded source changed"
    status, _ = request("relay.p1.test", "/healthz")
    assert status == 200, status
    args.output.parent.mkdir(parents=True, exist_ok=True)
    args.output.write_text(json.dumps({
        "status": "PASS", "public_client_ip": str(client_ip), "server_ip": args.address,
        "runner_arch": os.environ.get("RUNNER_ARCH", "unknown"),
        "checks": ["Controller TLS and source IP", "spoofed HTTP headers ignored", "Relay TLS"],
        "dns": "explicit public IP with test-domain SNI and pinned test CA, not public DNS acceptance",
        "not_run": ["QUIC address discovery", "real Agent Relay-only command", "wrong Relay token"],
    }, indent=2) + "\n")
    print(args.output.read_text())


if __name__ == "__main__":
    main()
