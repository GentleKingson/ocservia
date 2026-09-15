#!/usr/bin/env python3
"""Manual integration probe of the rendered transportd network, not G6 acceptance."""
import argparse
import copy
import json
import os
from pathlib import Path
import secrets
import shutil
import socket
import subprocess
import tempfile
import time


def run(*args, check=True):
    result = subprocess.run(args, text=True, stdout=subprocess.PIPE, stderr=subprocess.STDOUT)
    if check and result.returncode:
        raise RuntimeError(f"{args[:3]}: {result.stdout}")
    return result


parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument("--compose-json", type=Path, required=True)
parser.add_argument("--transport-image", required=True)
parser.add_argument("--relay-image", required=True)
parser.add_argument("--probe-image", default="node:24.18.0-bookworm")
parser.add_argument("--artifacts", type=Path, required=True)
parser.add_argument("--expect-isolated", action="store_true")
parser.add_argument("--cold-start", action="store_true")
args = parser.parse_args()
assert os.geteuid() == 0, "run on a disposable Linux integration host as root"
args.artifacts.mkdir(parents=True, exist_ok=True)
work = Path(tempfile.mkdtemp(prefix="single-relay-network-"))
os.chmod(work, 0o755)
project = "ocservia-single-relay-network-" + secrets.token_hex(4)
source = json.loads(args.compose_json.read_text())
service = copy.deepcopy(source["services"]["transportd"])
gateway = json.loads(run("docker", "network", "inspect", "bridge").stdout)[0]["IPAM"]["Config"][0]["Gateway"]
start = time.monotonic()
command = ["docker", "compose", "-p", project, "-f", str(work / "compose.json")]
try:
    run("openssl", "req", "-x509", "-newkey", "rsa:2048", "-nodes", "-days", "1",
        "-subj", "/CN=single-relay-test-ca", "-addext", "basicConstraints=critical,CA:TRUE",
        "-keyout", str(work / "ca.key"), "-out", str(work / "ca.crt"))
    run("openssl", "req", "-new", "-newkey", "rsa:2048", "-nodes",
        "-subj", "/CN=relay.single.test", "-keyout", str(work / "relay.key"),
        "-out", str(work / "relay.csr"))
    (work / "leaf.ext").write_text("basicConstraints=critical,CA:FALSE\nsubjectAltName=DNS:relay.single.test\nextendedKeyUsage=serverAuth\n")
    run("openssl", "x509", "-req", "-days", "1", "-in", str(work / "relay.csr"),
        "-CA", str(work / "ca.crt"), "-CAkey", str(work / "ca.key"), "-CAcreateserial",
        "-extfile", str(work / "leaf.ext"), "-out", str(work / "relay.crt"))
    os.chmod(work / "ca.key", 0o600)
    (work / "relay-token").write_text(secrets.token_hex(32) + "\n")
    (work / "controller.key").write_bytes(secrets.token_bytes(32))
    for name in ("relay.key", "relay-token", "controller.key"):
        os.chown(work / name, 65532, 65532)
        os.chmod(work / name, 0o400)
    for name, uid in (("runtime", 65532), ("trust", 65534)):
        (work / name).mkdir(mode=0o750)
        os.chown(work / name, uid, 65532)
    relay_config = Path(__file__).resolve().parents[1] / "deploy/g6-readiness/relay.toml"
    with socket.socket() as reservation:
        reservation.bind((gateway, 0))
        relay_port = reservation.getsockname()[1]
    # The Relay has its own bridge and is reachable only through a host TCP
    # publication. It never joins either production internal network.
    relay = {
        "image": args.relay_image,
        "command": ["--config-path", "/etc/iroh-relay/relay.toml"],
        "environment": {"IROH_RELAY_ACCESS_TOKEN_FILE": "/run/relay-secrets/relay-token",
                        "RUST_LOG": "info,iroh_relay::server::clients=debug"},
        "volumes": [f"{work}:/run/relay-secrets:ro", f"{relay_config}:/etc/iroh-relay/relay.toml:ro"],
        "ports": [f"{gateway}:{relay_port}:3443"],
        "networks": ["relay-boundary"],
    }
    networks = {name: {"internal": bool(source["networks"][name].get("internal", False))}
                for name in service["networks"]}
    networks["relay-boundary"] = {}
    topology = {"services": {"relay": relay}, "networks": networks}
    (work / "compose.json").write_text(json.dumps(topology))
    run(*command, "up", "-d", "--no-build", "relay")
    port = run(*command, "port", "relay", "3443").stdout.strip().rsplit(":", 1)[1]
    for key in ("depends_on", "secrets", "volumes", "healthcheck", "pull_policy"):
        service.pop(key, None)
    service["image"] = args.transport_image
    service["restart"] = "no"
    service["environment"] = {"OCSERV_RELAY_URL_A": f"https://relay.single.test:{port}",
                              "OCSERV_RELAY_URL_B": "", "RUST_LOG": "info,iroh=debug"}
    service["extra_hosts"] = [f"relay.single.test:{gateway}"]
    # No Controller backend is started by this network-only probe. Keep the
    # default empty approved-identity policy, not an unauthenticated Agent.
    trust_index = service["command"].index("--trust-socket")
    del service["command"][trust_index:trust_index + 2]
    service["command"] += ["--relay-ca-file", "/run/test-ca.crt"]
    service["volumes"] = [f"{work}/controller.key:/run/secrets/controller_iroh_key:ro",
                          f"{work}/relay-token:/run/secrets/relay_access_token:ro",
                          f"{work}/ca.crt:/run/test-ca.crt:ro",
                          f"{work}/runtime:/run/ocserv-platform",
                          f"{work}/trust:/run/ocserv-trust"]
    topology["services"]["transportd"] = service
    (work / "compose.json").write_text(json.dumps(topology))
    if args.cold_start:
        assert not args.expect_isolated
        run(*command, "stop", "relay")
    run(*command, "up", "-d", "--no-build", "transportd")
    container = run(*command, "ps", "-q", "transportd").stdout.strip()
    probe = run("docker", "run", "--rm", "--network", f"container:{container}",
                "--volume", f"{work}/ca.crt:/ca.crt:ro", args.probe_image,
                "python3", "-c", """
import json, socket, ssl, sys
result = {}
for name in ('example.com',):
    try:
        result['external_dns'] = bool(socket.getaddrinfo(name, 443))
    except OSError as e:
        result['external_dns'] = False
        result['dns_error'] = str(e)
try:
    with socket.create_connection((sys.argv[1], int(sys.argv[2])), timeout=5) as sock:
        with ssl.create_default_context(cafile='/ca.crt').wrap_socket(sock, server_hostname='relay.single.test') as tls:
            result['tls'] = tls.version()
except OSError as e:
    result['tls'] = False
    result['tls_error'] = str(e)
print(json.dumps(result))
""", gateway, port)
    result = json.loads(probe.stdout)
    if args.cold_start:
        assert not result["tls"], "cold-start Relay was unexpectedly reachable"
        run(*command, "start", "relay")
    # The vendor dial timeout is 10s; normal-relay backoff caps at 16s before
    # full jitter. A fixed 90s bound covers a full retry cycle and scheduling.
    def wait_connected(previous=0):
        deadline = time.monotonic() + 90
        while time.monotonic() < deadline:
            logs = run(*command, "logs", "--no-color", "transportd").stdout
            if logs.count('"target":"iroh::_events::relay::connected"') > previous and "Pong received from relay server" in logs:
                return logs.count('"target":"iroh::_events::relay::connected"')
            time.sleep(1)
        raise AssertionError("authenticated Relay connection did not recover within 90 seconds")

    if not args.expect_isolated:
        connected_count = wait_connected()
        original_identity = (work / "controller.key").read_bytes()
        run(*command, "stop", "relay")
        time.sleep(5)
        restore_start = time.monotonic()
        run(*command, "start", "relay")
        wait_connected(connected_count)
        result["relay_recovery_seconds"] = round(time.monotonic() - restore_start, 2)
        assert (work / "controller.key").read_bytes() == original_identity
    # A bounded observation window, fixed before either before/after run.
    time.sleep(20)
    for name in ("transportd", "relay"):
        (args.artifacts / f"{name}.log").write_text(run(*command, "logs", "--no-color", name).stdout)
    inspect = json.loads(run("docker", "inspect", container).stdout)[0]
    (args.artifacts / "transport-inspect.json").write_text(json.dumps(inspect, indent=2))
    result.update(networks=networks, elapsed_seconds=round(time.monotonic() - start, 2),
                  expected_isolated=args.expect_isolated, cold_start=args.cold_start,
                  restart_count=inspect["RestartCount"], running=inspect["State"]["Running"])
    (args.artifacts / "result.json").write_text(json.dumps(result, indent=2) + "\n")
    print(json.dumps(result, indent=2))
    assert result["running"], "transportd exited before the network probe"
    assert result["restart_count"] == 0, "network recovery restarted transportd"
    assert bool(result["tls"]) != (args.expect_isolated or args.cold_start), "unexpected egress result"
finally:
    for name in ("transportd", "relay"):
        (args.artifacts / f"{name}.log").write_text(run(*command, "logs", "--no-color", name, check=False).stdout)
    run(*command, "down", "--volumes", "--remove-orphans", check=False)
    shutil.rmtree(work)
