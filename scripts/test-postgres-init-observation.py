"""Run on BuildServer only. Fake credentials, task-owned Docker resources only."""
import argparse
import base64
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import time


def run(*args, check=True, **kwargs):
    return subprocess.run(args, check=check, capture_output=True, **kwargs)


parser = argparse.ArgumentParser()
parser.add_argument("--image", required=True)
parser.add_argument("--expect-argv", choices=["yes", "no"], required=True)
args = parser.parse_args()
root = Path(__file__).resolve().parents[1]
work = Path(tempfile.mkdtemp(prefix="postgres-observation-", dir=root))
name = work.name
secrets = work / "secrets"
secrets.mkdir(mode=0o700)
init = work / "init"
init.mkdir()
app = "test-only-app-'\"\\:$ spaces!\r\u03a9"
backup = "test-only-backup-'\"\\:$ spaces!"
values = {"owner": "test-only-owner", "app": app, "backup": backup}
for key, value in values.items():
    path = secrets / key
    path.write_text(value + "\n")
    path.chmod(0o444)
shutil.copy2(root / "deploy/production/postgres-init/001-runtime-role.sh", init)
# Hold a real catalog lock to make a short init window observable. This changes
# timing only: the entrypoint, psql, namespaces, users and proc mounts are real.
(init / "000-observation.sh").write_text("""#!/usr/bin/env bash
set -euo pipefail
psql -U "$POSTGRES_USER" -d "$POSTGRES_DB" -v ON_ERROR_STOP=1 -c 'BEGIN; LOCK TABLE pg_authid IN ACCESS EXCLUSIVE MODE; SELECT pg_sleep(12); COMMIT' &
sleep 1
""")
(init / "000-observation.sh").chmod(0o755)
data = work / "data"
data.mkdir(mode=0o700)
os.chown(data, 999, 999)
archive = work / "backup"
archive.mkdir(mode=0o700)
os.chown(archive, 999, 999)
result = {"image": args.image, "timing_instrumentation": "12s pg_authid lock", "checks": {}}
logs = b""


def sql(statement):
    return run("docker", "exec", name, "psql", "-U", "ocservia_owner", "-d", "ocservia", "-v", "ON_ERROR_STOP=1", "-Atc", statement).stdout


def check(label, value):
    result["checks"][label] = bool(value)
    assert value, label


try:
    run("docker", "network", "create", "--internal", name)
    command = ["docker", "run", "-d", "--name", name, "--network", name,
               "--read-only", "--cap-drop=ALL", "--security-opt=no-new-privileges:true",
               "--init", "--user", "999:999", "--cpus", "2", "--memory", "2g", "--pids-limit", "256",
               "--tmpfs", "/run/postgresql:size=16m,uid=999,gid=999,mode=0770",
               "--tmpfs", "/tmp:size=16m,uid=999,gid=999,mode=0700",
               "-e", "POSTGRES_USER=ocservia_owner", "-e", "POSTGRES_DB=ocservia",
               "-e", "POSTGRES_PASSWORD_FILE=/run/secrets/postgres_owner_password",
               "-e", "POSTGRES_INITDB_ARGS=--auth-host=scram-sha-256 --data-checksums",
               "-e", "PGDATA=/var/lib/postgresql/data",
               "-v", f"{data}:/var/lib/postgresql/data",
               "-v", f"{archive}:/var/lib/ocservia-backup",
               "-v", f"{init}:/docker-entrypoint-initdb.d:ro",
               "-v", f"{root}/deploy/production/postgresql.conf:/etc/postgresql/postgresql.conf:ro"]
    for key in values:
        command += ["-v", f"{secrets / key}:/run/secrets/postgres_{key}_password:ro"]
    command += [args.image, "postgres", "-c", "config_file=/etc/postgresql/postgresql.conf"]
    run(*command)
    container_pid = run("docker", "inspect", "--format", "{{.State.Pid}}", name).stdout.decode().strip()
    pid_namespace = (Path("/proc") / container_pid / "ns/pid").stat().st_ino
    seen = None
    limit = time.monotonic() + 50
    while time.monotonic() < limit and seen is None:
        for proc in Path("/proc").glob("[0-9]*"):
            try:
                if (proc / "ns/pid").stat().st_ino != pid_namespace:
                    continue
                argv = (proc / "cmdline").read_bytes()
                if b"psql\x00" in argv and b"--set=ON_ERROR_STOP=1\x00" in argv and b"--username\x00ocservia_owner" in argv:
                    seen = proc
                    break
            except (FileNotFoundError, PermissionError, ProcessLookupError):
                pass
        time.sleep(0.01)
    check("real_init_psql_observed", seen is not None)
    status = (seen / "status").read_text()
    inner_pid = next(line for line in status.splitlines() if line.startswith("NSpid:")).split()[-1]
    result["psql_uid"] = next(line for line in status.splitlines() if line.startswith("Uid:")).split()[1]
    result["image_id"] = run("docker", "inspect", "--format", "{{.Image}}", name).stdout.decode().strip()
    for label, prefix, secret_path, pid in [
        ("host_ordinary", ["setpriv", "--reuid=65534", "--regid=65534", "--clear-groups"], str(secrets / "app"), seen.name),
        ("host_root", [], str(secrets / "app"), seen.name),
        ("container_postgres", ["docker", "exec", "--user", "999:999", name], "/run/secrets/postgres_app_password", inner_pid),
        ("container_other_uid", ["docker", "exec", "--user", "65534:65534", name], "/run/secrets/postgres_app_password", inner_pid),
        ("container_root_no_caps", ["docker", "exec", "--user", "0:0", name], "/run/secrets/postgres_app_password", inner_pid),
    ]:
        proc_read = run(*prefix, "cat", f"/proc/{pid}/cmdline", check=False)
        secret_read = run(*prefix, "cat", secret_path, check=False)
        env_read = run(*prefix, "cat", f"/proc/{pid}/environ", check=False)
        forms = [form for value in [app, backup] for form in [value.encode(), base64.b64encode(value.encode())]]
        exposed = any(form in proc_read.stdout for form in forms)
        result[label] = {"argv_readable": proc_read.returncode == 0, "password_in_argv": exposed,
                         "source_secret_readable": secret_read.returncode == 0,
                         "environment_readable": env_read.returncode == 0,
                         "password_in_environment": any(form in env_read.stdout for form in forms)}
        check(f"{label}_argv_readable", proc_read.returncode == 0)
        check(f"{label}_argv_expectation", exposed == (args.expect_argv == "yes"))
    check("host_ordinary_cannot_read_secret", not result["host_ordinary"]["source_secret_readable"])
    check("host_root_environment_readable", result["host_root"]["environment_readable"])
    check("no_password_environment", not any(result[k]["password_in_environment"] for k in result if isinstance(result[k], dict) and "password_in_environment" in result[k]))
    limit = time.monotonic() + 60
    while time.monotonic() < limit:
        ready = run("docker", "exec", name, "pg_isready", "-h", "127.0.0.1", "-U", "ocservia_owner", "-d", "ocservia", check=False)
        if ready.returncode == 0:
            break
        time.sleep(0.3)
    check("database_ready", ready.returncode == 0)
    result["server_version"] = sql("SHOW server_version").decode().strip()
    check("runtime_roles", sql("SELECT rolname || ':' || rolreplication FROM pg_roles WHERE rolname IN ('ocservia_app','ocservia_backup') ORDER BY rolname").decode().strip() == "ocservia_app:false\nocservia_backup:true")
    before = sql("SELECT rolname, rolpassword FROM pg_authid WHERE rolname IN ('ocservia_app','ocservia_backup') ORDER BY rolname")
    (secrets / "app").write_text("different-test-only-password\n")
    repeated = run("docker", "exec", name, "bash", "/docker-entrypoint-initdb.d/001-runtime-role.sh")
    logs += repeated.stdout + repeated.stderr
    check("existing_roles_unchanged", before == sql("SELECT rolname, rolpassword FROM pg_authid WHERE rolname IN ('ocservia_app','ocservia_backup') ORDER BY rolname"))
    (secrets / "app").write_text(app + "\n")
    # Authenticate over TCP with a private transient pgpass; no password argv/env.
    for role, password in [("ocservia_app", app), ("ocservia_backup", backup)]:
        escaped = password.replace("\\", "\\\\").replace(":", "\\:")
        passline = f"127.0.0.1:5432:*:{role}:{escaped}\n".encode()
        login = run("docker", "exec", "-i", name, "bash", "-ceu",
                    'f=$(mktemp); trap \'rm -f "$f"\' EXIT; chmod 600 "$f"; cat >"$f"; PGPASSFILE="$f" psql -w -h 127.0.0.1 -U "$1" -d ocservia -Atc "SELECT 1"', "--", role, input=passline)
        check(f"{role}_login", login.stdout.strip() == b"1")
    escaped = backup.replace("\\", "\\\\").replace(":", "\\:")
    backup_run = run("docker", "exec", "-i", name, "bash", "-ceu",
                     'f=$(mktemp); trap \'rm -f "$f"\' EXIT; chmod 600 "$f"; cat >"$f"; export PGPASSFILE="$f"; pg_basebackup -w -h 127.0.0.1 -U ocservia_backup -D /var/lib/ocservia-backup/base/test -X stream -c fast; pg_verifybackup /var/lib/ocservia-backup/base/test',
                     input=f"127.0.0.1:5432:replication:ocservia_backup:{escaped}\n".encode())
    logs += backup_run.stdout + backup_run.stderr
    check("physical_backup_verified", backup_run.returncode == 0)
    for invalid in ["", "line1\nline2"]:
        (secrets / "app").write_text(invalid)
        rejected = run("docker", "exec", name, "bash", "/docker-entrypoint-initdb.d/001-runtime-role.sh", check=False)
        check(f"invalid_password_{len(invalid)}", rejected.returncode != 0)
        logs += rejected.stdout + rejected.stderr
    (secrets / "app").write_text(app + "\n")
    error = run("docker", "exec", "-e", "POSTGRES_USER=missing_role", name, "bash", "/docker-entrypoint-initdb.d/001-runtime-role.sh", check=False)
    check("psql_failure_propagated", error.returncode != 0)
    logs += error.stdout + error.stderr
    if args.expect_argv == "no":
        sql("DROP ROLE ocservia_app; ALTER ROLE ocservia_owner SET default_transaction_read_only = on")
        hba_before = run("docker", "exec", name, "cat", "/var/lib/postgresql/data/pg_hba.conf").stdout
        error = run("docker", "exec", name, "bash", "/docker-entrypoint-initdb.d/001-runtime-role.sh", check=False)
        check("sql_failure_propagated", error.returncode != 0)
        check("no_hba_append_after_sql_error", hba_before == run("docker", "exec", name, "cat", "/var/lib/postgresql/data/pg_hba.conf").stdout)
        logs += error.stdout + error.stderr
    temporary = run("docker", "exec", name, "tar", "-cf", "-", "/tmp", check=False).stdout
    check("no_password_in_temp_files", not any(value.encode() in temporary or base64.b64encode(value.encode()) in temporary for value in values.values()))
finally:
    container_logs = run("docker", "logs", name, check=False)
    logs += container_logs.stdout + container_logs.stderr
    result["logs_contain_password"] = any(v.encode() in logs or base64.b64encode(v.encode()) in logs for v in values.values())
    run("docker", "rm", "-f", name, check=False)
    run("docker", "network", "rm", name, check=False)
    shutil.rmtree(work)
    print(json.dumps(result, indent=2))
assert not result["logs_contain_password"], "credentials in stdout/stderr"
