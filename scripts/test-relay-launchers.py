#!/usr/bin/env python3
"""Execute the shipped launchers against argv recorders in a disposable container."""
import json
import os
from pathlib import Path
import subprocess
import tempfile
import tomllib

ROOT = Path(__file__).resolve().parents[1]
assert os.geteuid() == 0 and Path("/.dockerenv").exists(), "isolated root Docker container required"
for relative in ("deploy/production/relay/relay.toml", "deploy/g6-readiness/relay.toml"):
    with (ROOT / relative).open("rb") as source:
        config = tomllib.load(source)
    assert config["access"] == {"shared_token": ["replaced-by-entrypoint"]}, relative
    assert "access" not in config["limits"]["client"]["rx"], relative
    print(f"PASS {relative}: top-level Relay token authentication")
agent = Path("/usr/libexec/ocservia/ocservia-agent")
transport = Path("/usr/local/bin/ocservia-transportd")
assert not agent.exists() and not transport.exists(), "refusing to replace installed binaries"
recorder = "#!/usr/bin/python3\nimport json,sys\nprint(json.dumps(sys.argv[1:]))\n"
ca_paths = [Path("/etc/ocservia-agent/relay-ca.pem"), Path("/run/secrets/relay_ca")]
assert all(not path.exists() and not path.is_symlink() for path in ca_paths)
created_ca_dirs = []
env = os.environ | {
    "CONTROLLER_ENDPOINT_ID": "c" * 64, "NODE_ID": "018f1e11-2222-7333-8444-555555555555",
    "CONTROLLER_COMMAND_VERIFICATION_KEY_FILE": "/protected/a key's $literal.pem",
    "USER_PASSWORD_SEAL_KEY_ID": "user", "USER_PASSWORD_SEAL_PUBLIC_KEY_SHA256": "a" * 64,
    "P12_PASSWORD_SEAL_KEY_ID": "p12", "P12_PASSWORD_SEAL_PUBLIC_KEY_SHA256": "b" * 64,
}
cases = [None, "", "https://relay-b.example.test"]
try:
    for binary in (agent, transport):
        binary.parent.mkdir(parents=True, exist_ok=True)
        binary.write_text(recorder)
        binary.chmod(0o755)
    for wrapper, prefix in [
        ("deploy/production/transportd-relays.sh", "OCSERV_"),
        ("deploy/production/systemd/agent-relays.sh", ""),
    ]:
        for b in cases:
            current = env | {prefix + "RELAY_URL_A": "https://relay-a.example.test"}
            current.pop(prefix + "RELAY_URL_B", None)
            if b is not None:
                current[prefix + "RELAY_URL_B"] = b
            args = [str(ROOT / wrapper)]
            if prefix:
                args += ["--relay-token-file", "/protected/token file", "--socket", "/tmp/a socket"]
            result = subprocess.run(args, env=current, check=True, capture_output=True, text=True)
            argv = json.loads(result.stdout)
            urls = [argv[i + 1] for i, arg in enumerate(argv) if arg == "--relay-url"]
            assert urls == ["https://relay-a.example.test"] + ([b] if b else []), argv
            assert argv[argv.index("--relay-mode") + 1] == "custom"
            assert "--relay-token-file" in argv and "" not in argv
            if not prefix:
                assert argv[argv.index("--controller-command-key-file") + 1] == env["CONTROLLER_COMMAND_VERIFICATION_KEY_FILE"]
            print(f"PASS {wrapper}: B={b!r}")
        for value in ["https://bad host", 'https://bad"host', "https://$(touch /tmp/relay-injection)"]:
            current = env | {prefix + "RELAY_URL_A": value, prefix + "RELAY_URL_B": ""}
            argv = json.loads(subprocess.check_output([str(ROOT / wrapper)], env=current, text=True))
            assert argv[argv.index("--relay-url") + 1] == value
            assert not Path("/tmp/relay-injection").exists()
        current = env | {prefix + "RELAY_URL_B": "https://relay-b.example.test"}
        current.pop(prefix + "RELAY_URL_A", None)
        assert subprocess.run([str(ROOT / wrapper)], env=current, capture_output=True).returncode != 0
        ca = ca_paths[1 if prefix else 0]
        if not ca.parent.exists():
            ca.parent.mkdir(parents=True)
            created_ca_dirs.append(ca.parent)
        ca.write_text("public CA fixture; PEM parsing belongs to the real binary\n")
        ca.chmod(0o444)
        current = env | {prefix + "RELAY_URL_A": "https://relay-a.example.test"}
        argv = json.loads(subprocess.check_output([str(ROOT / wrapper)], env=current, text=True))
        assert argv[argv.index("--relay-ca-file") + 1] == str(ca)
        if not prefix:
            def rejected():
                assert subprocess.run([str(ROOT / wrapper)], env=current, capture_output=True).returncode != 0
            ca.chmod(0o644)
            rejected()
            ca.chmod(0o444)
            os.chown(ca, 1, 0)
            rejected()
            os.chown(ca, 0, 0)
            saved = ca.with_suffix(".saved")
            ca.rename(saved)
            ca.symlink_to(saved)
            rejected()
            ca.unlink()
            saved.rename(ca)
            os.link(ca, saved)
            rejected()
            saved.unlink()
            ca.parent.chmod(0o775)
            rejected()
            ca.parent.chmod(0o755)
        ca.unlink()
        print(f"PASS {wrapper}: optional protected Relay CA")
    with tempfile.TemporaryDirectory() as temporary:
        config = Path(temporary) / "install.env"
        for file_b, override in [(None, None), ("", None), ("https://relay-b.example.test", ""), ("https://relay-b.example.test", None)]:
            config.write_text("OCSERV_RELAY_URL_A=https://relay-a.example.test\n" +
                              (f"OCSERV_RELAY_URL_B={file_b}\n" if file_b is not None else ""))
            current = env.copy()
            current.pop("OCSERV_RELAY_URL_A", None)
            current.pop("OCSERV_RELAY_URL_B", None)
            if override is not None:
                current["OCSERV_RELAY_URL_B"] = override
            result = subprocess.check_output([
                "bash", "-c",
                'source "$1/deploy/lib/install-env.sh"; install_env_load "$2" OCSERV_RELAY_URL_A OCSERV_RELAY_URL_B >&2; exec "$1/deploy/production/transportd-relays.sh"',
                "relay-test", str(ROOT), str(config)
            ], env=current, text=True)
            argv = json.loads(result)
            assert argv.count("--relay-url") == (2 if file_b and override is None else 1)
    print("PASS literal argv and install.env unset/empty/override contracts")
finally:
    for ca in ca_paths:
        ca.unlink(missing_ok=True)
    for directory in reversed(created_ca_dirs):
        directory.rmdir()
    agent.unlink(missing_ok=True)
    transport.unlink(missing_ok=True)
