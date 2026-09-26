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
import secrets
import urllib.request


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--address", required=True)
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--mode", choices=('fixture', 'controller'), default='fixture')
    parser.add_argument("--candidate-sha", default='')
    args = parser.parse_args()
    assert ipaddress.ip_address(args.address).is_global, "a public IP is required"
    context = ssl.create_default_context(cadata=os.environ["P1_PUBLIC_CA_PEM"])

    def request(name, path, extra="", method="GET", body=""):
        with socket.create_connection((args.address, 443), timeout=10) as sock:
            with context.wrap_socket(sock, server_hostname=name) as tls:
                tls.sendall((f"{method} {path} HTTP/1.1\r\nHost: {name}\r\n"
                             f"Connection: close\r\nContent-Length: {len(body)}\r\n{extra}\r\n{body}").encode())
                response = http.client.HTTPResponse(tls)
                response.begin()
                return response.status, response.read().decode()

    if args.mode == 'controller':
        assert len(args.candidate_sha) == 40 and all(c in '0123456789abcdef' for c in args.candidate_sha)
        name = f'controller.{args.address}.sslip.io'
        status, body = request(name, '/api/v1/version')
        assert status == 200 and json.loads(body)['commit'] == args.candidate_sha
        status, _ = request(f'relay.{args.address}.sslip.io', '/healthz')
        assert status == 200
        with urllib.request.urlopen('https://api.ipify.org', timeout=10) as response:
            client_ip = str(ipaddress.ip_address(response.read().decode().strip()))
        assert ipaddress.ip_address(client_ip).is_global
        request_id = 'integrated-public-' + secrets.token_hex(12)
        status, _ = request(name, '/api/v1/auth/login',
                            f'Origin: https://{name}\r\nContent-Type: application/json\r\n'
                            f'X-Request-ID: {request_id}\r\nX-Ocservia-Client-IP: 203.0.113.66\r\n'
                            'X-Forwarded-For: 203.0.113.66\r\n', 'POST',
                            '{"username":"public-source-probe","password":"intentionally-invalid"}')
        assert status == 401, status
        args.output.parent.mkdir(parents=True, exist_ok=True)
        args.output.write_text(json.dumps(dict(status='PASS', candidate_sha=args.candidate_sha,
            public_client_ip=client_ip, server_ip=args.address, request_id=request_id,
            runner_arch=os.environ.get('RUNNER_ARCH', 'unknown'),
            checks=['Controller exact source and TLS', 'Relay TLS', 'invalid login rejected'],
            source_ip_status='PENDING server auth.result correlation',
            dns='explicit public address; real domain SNI and pinned test CA'), indent=2) + '\n')
        print(args.output.read_text())
        return

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
