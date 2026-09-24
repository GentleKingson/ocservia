#!/usr/bin/env python3
"""BuildServer-only network fixture; not external Agent or release acceptance."""
import argparse
import concurrent.futures
import copy
import json
import os
from pathlib import Path
import secrets
import shutil
import socket
import ssl
import subprocess
import tempfile
import time

ROOT = Path(__file__).resolve().parents[1]
CADDY = "caddy:2.11.4-alpine@sha256:de23def33b17fb5d1290b0f6c2add1d70780e52341896c00a4c8a2a2fe9d355e"
PROBE = "node:24.18.1-bookworm@sha256:19cd848a0e073d34bd8cd5545a1b6b4d28489b3e3b607366621ced442bd5f6b4"


def run(*args, check=True, **kwargs):
    result = subprocess.run(args, text=True, stdout=subprocess.PIPE,
                            stderr=subprocess.STDOUT, **kwargs)
    if check and result.returncode:
        raise RuntimeError(f"{args[:4]}: {result.stdout}")
    return result.stdout


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--edge-image", required=True)
    parser.add_argument("--relay-image", required=True)
    parser.add_argument("--artifacts", type=Path, required=True)
    args = parser.parse_args()
    args.artifacts.mkdir(parents=True, exist_ok=False)
    work = Path(tempfile.mkdtemp(prefix="ocservia-p1-"))
    os.chmod(work, 0o755)
    project = "ocservia-p1-" + secrets.token_hex(4)
    gateway_image = project + "-gateway"
    command = ["docker", "compose", "-p", project, "-f", str(work / "compose.json")]
    result = {"scope": "isolated fixture, not external clients or real Agent",
              "status": "FAIL", "checks": [], "not_run": [
                  "two external sources to one public endpoint", "transportd/public TCP+QUIC hairpin",
                  "real Agent command with direct path excluded", "bad Relay token",
                  "35-minute idle timeout", "real Web/OIDC workflow"]}

    def compose(*parts, **kwargs):
        return run(*command, *parts, **kwargs)

    def save_topology():
        (work / "compose.json").write_text(json.dumps(topology))

    def address(service, network):
        info = json.loads(run("docker", "inspect", compose("ps", "-q", service).strip()))[0]
        return info["NetworkSettings"]["Networks"][project + "_" + network]["IPAddress"]

    try:
        env = {key: value for key, value in os.environ.items() if not key.startswith('OCSERV_')}
        env.update({f"OCSERV_{role}_IMAGE": "fixture.invalid/image@sha256:" + "0" * 64
                    for role in ("EDGE", "RELAY", "GATEWAY", "CONTROL", "TRANSPORT", "BACKUP", "POSTGRES", "OTEL")})
        env.update(OCSERV_PUBLIC_HOST="controller.p1.test", OCSERV_RELAY_PUBLIC_HOST="relay.p1.test",
                   OCSERV_SECRET_DIR=str(work), OCSERV_RELAY_SECRET_DIR=str(work),
                   OCSERV_AUDIT_EVENT_KEY_ID="fixture", OCSERV_CONTROLLER_ENDPOINT_ID="fixture",
                   OCSERV_CERTIFICATE_SIGNER_URL="https://signer.p1.test/sign",
                   OCSERV_RELAY_URL_A="https://relay.p1.test")
        base = ["docker", "compose", "-f", str(ROOT / "deploy/production/compose.yaml")]
        raw = json.loads(run(*base, "-f", str(ROOT / "deploy/production/integrated/compose.yaml"),
                             "config", "--no-env-resolution", "--format", "json", env=env))
        ports = [(name, p["target"], str(p["published"]), p["protocol"])
                 for name, service in raw["services"].items() for p in service.get("ports", [])]
        assert ports == [("edge", 8443, "443", "tcp"), ("relay", 7842, "7842", "udp")], ports
        standalone = json.loads(run(*base, "config", "--no-env-resolution", "--format", "json", env=env))
        assert standalone["services"]["gateway"]["ports"][0]["target"] == 8443
        for name, expected in (("gateway", "integrated/Caddyfile"), ("relay", "relay/relay.toml")):
            assert any(v["source"] == str(ROOT / "deploy/production" / expected)
                       for v in raw["services"][name]["volumes"])
        assert not raw["services"]["edge"].get("secrets")
        result["checks"].append("merged ports, relative mounts, standalone mapping, no Edge secrets")
        (args.artifacts / "ports.json").write_text(json.dumps(ports))
        run("docker", "build", "--pull=false", "-t", gateway_image, "-", input=(
            f"FROM {CADDY}\nRUN setcap -r /usr/bin/caddy\nUSER 65532:65532\n"))
        for name in ("controller", "relay"):
            run("openssl", "req", "-x509", "-newkey", "rsa:2048", "-nodes", "-days", "1",
                "-subj", f"/CN={name}.p1.test", "-addext", f"subjectAltName=DNS:{name}.p1.test",
                "-keyout", str(work / f"{name}.key"), "-out", str(work / f"{name}.crt"))
            os.chmod(work / f"{name}.key", 0o444)
        (work / "ca.crt").write_bytes((work / "controller.crt").read_bytes() + (work / "relay.crt").read_bytes())
        (work / "token").write_text(secrets.token_hex(32))
        os.chmod(work / "token", 0o444)
        (work / "index.html").write_text("p1-static-fixture")
        backend = '''from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import time
class Handler(BaseHTTPRequestHandler):
 def log_message(self, *args): pass
 def do_GET(self):
  self.send_response(200)
  self.send_header('Content-Type', 'text/event-stream' if self.path == '/api/events' else 'text/plain')
  self.end_headers()
  if self.path == '/api/events':
   for i in range(3):
    self.wfile.write(('data: %s\\n\\n' % i).encode()); self.wfile.flush(); time.sleep(1)
  else: self.wfile.write(self.headers.get('X-Ocservia-Client-IP', 'health').encode())
ThreadingHTTPServer(('0.0.0.0', 8080), Handler).serve_forever()
'''
        services = {k: copy.deepcopy(raw["services"][k]) for k in ("edge", "gateway", "relay")}
        for service in services.values():
            service.pop("depends_on", None)
            service.pop("healthcheck", None)
            service.pop("secrets", None)
            service["restart"] = "no"
        services["edge"].update(image=args.edge_image,
            environment={"OCSERV_PUBLIC_HOST": "controller.p1.test", "OCSERV_RELAY_PUBLIC_HOST": "relay.p1.test"},
            ports=["127.0.0.1::8443"],
            networks={"frontend": {}, "edge-gateway": {"ipv4_address": "198.18.91.2"}, "edge-relay": {}})
        services["gateway"].update(image=gateway_image,
            environment={"OCSERV_PUBLIC_HOST": "controller.p1.test", "OCSERV_EDGE_GATEWAY_IP": "198.18.91.2"},
            networks={"edge-gateway": {}, "application": {}},
            volumes=[f"{ROOT}/deploy/production/integrated/Caddyfile:/etc/caddy/Caddyfile:ro",
                     f"{work}/controller.crt:/run/secrets/tls_certificate:ro",
                     f"{work}/controller.key:/run/secrets/tls_private_key:ro",
                     f"{work}/index.html:/srv/index.html:ro"])
        services["relay"].update(image=args.relay_image, ports=["127.0.0.1::7842/udp"],
            volumes=[f"{ROOT}/deploy/production/relay/relay.toml:/etc/iroh-relay/relay.toml:ro",
                     f"{work}/relay.crt:/run/secrets/relay_tls_certificate:ro",
                     f"{work}/relay.key:/run/secrets/relay_tls_private_key:ro",
                     f"{work}/token:/run/secrets/relay_access_token:ro"])
        services["control-plane"] = {"image": PROBE, "command": ["python3", "-u", "-c", backend], "networks": ["application"]}
        for name in ("client-a", "client-b"):
            services[name] = {"image": PROBE, "command": ["sleep", "600"], "networks": ["frontend"],
                              "volumes": [f"{work}/ca.crt:/ca.crt:ro"]}
        topology = {"services": services, "networks": {
            "frontend": {}, "edge-relay": {"ipam": {"config": [{"subnet": "198.18.92.0/24"}]}},
            "application": {"internal": True},
            "edge-gateway": {"internal": True, "ipam": {"config": [
                {"subnet": "198.18.91.0/24", "ip_range": "198.18.91.128/25"}]}}}}
        save_topology()
        compose("up", "-d", "--quiet-pull")
        port = int(compose("port", "edge", "8443").strip().rsplit(":", 1)[1])
        context = ssl.create_default_context(cafile=str(work / "ca.crt"))
        no_sni_context = ssl.create_default_context(cafile=str(work / "ca.crt"))
        no_sni_context.check_hostname = False

        def request(name="controller.p1.test", path="/api/ip", host=None, preamble=b"", stream=False):
            with socket.create_connection(("127.0.0.1", port), timeout=8) as sock:
                if preamble:
                    sock.sendall(preamble)
                with (context if name else no_sni_context).wrap_socket(sock, server_hostname=name) as tls:
                    tls.sendall((f"GET {path} HTTP/1.1\r\nHost: {host or name}\r\nConnection: close\r\n"
                                 "X-Ocservia-Client-IP: 203.0.113.66\r\nX-Forwarded-For: 203.0.113.66\r\n\r\n").encode())
                    data = b""
                    while part := tls.recv(65536):
                        data += part
                        if stream and b"data: 0" in data:
                            return data
                    return data

        def wait_ready(name="controller.p1.test", path="/api/ip"):
            deadline = time.monotonic() + 40
            last = None
            while time.monotonic() < deadline:
                try:
                    data = request(name, path)
                    last = data[:256]
                    if b"200 OK" in data or b"204 No Content" in data:
                        return data
                except OSError as error:
                    last = str(error)
                time.sleep(1)
            raise AssertionError(f"not ready: {name}: {last}")

        wait_ready()
        wait_ready("relay.p1.test", "/healthz")
        assert "iroh-relay 1.2.0" in compose("exec", "-T", "relay", "iroh-relay", "--version")
        result["checks"].append("both TLS certificates; real iroh-relay 1.2.0 accepts stripped TLS")
        assert b"421" in request(host="wrong.p1.test").split(b"\r\n", 1)[0]
        assert b"421" in request(host="controller.p1.test:8443").split(b"\r\n", 1)[0]
        assert b"200 OK" in request(host="controller.p1.test:443")
        result["relay_wrong_host_status"] = request("relay.p1.test", "/healthz", "wrong.p1.test").split(b"\r\n", 1)[0].decode()
        assert b"p1-static-fixture" in request(path="/")
        assert b"alt-svc:" not in request(path="/").lower()
        for name, preamble in (("unknown.p1.test", b""), (None, b""),
                               ("controller.p1.test", b"PROXY TCP4 203.0.113.66 127.0.0.1 1234 443\r\n")):
            try:
                request(name=name, preamble=preamble)
            except OSError:
                pass
            else:
                raise AssertionError("invalid SNI/PROXY input accepted")
        with socket.create_connection(("127.0.0.1", port), timeout=8) as sock:
            sock.sendall(b"GET / HTTP/1.1\r\nHost: controller.p1.test\r\n\r\n")
            try:
                assert sock.recv(1024) == b"", "plaintext request accepted"
            except ConnectionResetError:
                pass
        # Two bridge sources test metadata propagation, not the two external-source gate.
        client_code = '''import socket, ssl, sys
with socket.create_connection(('edge', 8443), timeout=5) as sock:
 with ssl.create_default_context(cafile='/ca.crt').wrap_socket(sock, server_hostname='controller.p1.test') as tls:
  tls.sendall(b'GET /api/ip HTTP/1.1\\r\\nHost: controller.p1.test\\r\\nX-Ocservia-Client-IP: 203.0.113.66\\r\\nX-Forwarded-For: 203.0.113.66\\r\\nConnection: close\\r\\n\\r\\n')
  data = b''
  while part := tls.recv(65536): data += part
  assert sys.argv[1].encode() in data.split(b'\\r\\n\\r\\n', 1)[1], data
'''
        for name in ("client-a", "client-b"):
            compose("exec", "-T", name, "python3", "-c", client_code, address(name, "frontend"))
        run("docker", "run", "--rm", "--network", project + "_edge-gateway",
            "-v", f"{work}/ca.crt:/ca.crt:ro", PROBE, "python3", "-c", '''
import socket, ssl, sys
with socket.create_connection((sys.argv[1], 8443), timeout=5) as sock:
 sock.sendall(b'PROXY TCP4 203.0.113.66 127.0.0.1 1234 443\\r\\n')
 try:
  with ssl.create_default_context(cafile='/ca.crt').wrap_socket(sock, server_hostname='controller.p1.test'):
   raise AssertionError('Gateway trusted PROXY from a non-Edge source')
 except OSError: pass
''', address("gateway", "edge-gateway"))
        result["checks"].append("two bridge source IPs, forged HTTP/PROXY headers, wrong Host/SNI, static page, no HTTP3 advertisement")
        start = time.monotonic()
        assert b"data: 0" in request(path="/api/events", stream=True)
        assert time.monotonic() - start < 2, "SSE first event buffered"
        with concurrent.futures.ThreadPoolExecutor(max_workers=32) as pool:
            assert all(b"data: 2" in data for data in pool.map(lambda _: request(path="/api/events"), range(32)))
        result["checks"].append("32 concurrent 3-second SSE streams; not a capacity or 35-minute idle guarantee")
        for name, network in (("gateway", "edge-gateway"), ("relay", "edge-relay")):
            before = address(name, network)
            compose("stop", name)
            compose("rm", "-f", name)
            holder = project + "-holder"
            try:
                run("docker", "run", "-d", "--name", holder, "--network", project + "_" + network,
                    "--ip", before, PROBE, "sleep", "120")
                compose("up", "-d", name)
                assert address(name, network) != before
                wait_ready("relay.p1.test" if name == "relay" else "controller.p1.test",
                           "/healthz" if name == "relay" else "/api/ip")
            finally:
                run("docker", "rm", "-f", holder, check=False)
        compose("restart", "edge")
        port = int(compose("port", "edge", "8443").strip().rsplit(":", 1)[1])
        wait_ready()
        wait_ready("relay.p1.test", "/healthz")
        result["checks"].append("Gateway/Relay changed-IP recreation; Edge restart; new TLS connections recover")
        result["images"] = {name: json.loads(run("docker", "image", "inspect", image))[0]["Id"]
                            for name, image in (("edge", args.edge_image), ("relay", args.relay_image), ("caddy", gateway_image))}
        result["status"] = "PASS"
    finally:
        for name in ("edge", "gateway", "relay", "control-plane"):
            (args.artifacts / f"{name}.log").write_text(compose("logs", "--no-color", name, check=False))
        (args.artifacts / "result.json").write_text(json.dumps(result, indent=2) + "\n")
        compose("down", "--volumes", "--remove-orphans", check=False)
        run("docker", "image", "rm", gateway_image, check=False)
        shutil.rmtree(work)
    print(json.dumps(result, indent=2))


if __name__ == "__main__":
    main()
