#!/usr/bin/env bash
# Exercise the released single-file entrypoint and runtime, with no Git on PATH.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
fixture="$(mktemp -d "${TMPDIR:-${HOME}}/.ocservia-controller-bootstrap-test.XXXXXX")"
trap 'rm -rf -- "${fixture}"' EXIT
mkdir -m 700 "${fixture}/assets" "${fixture}/bin" "${fixture}/config" "${fixture}/secrets"
"${ROOT}/scripts/prepare-bootstrap-release-assets.sh" "${fixture}/assets" v4.5.6
bootstrap="${fixture}/assets/controller-bootstrap.sh"
bin="${fixture}/bin"
# Only release construction above uses Git. Installation cannot find it.
for tool in bash env dirname uname tar gzip sha256sum find diff sort uniq awk sed mktemp rm cp mv mkdir stat realpath id chmod grep cut tr cat sleep; do
  ln -s "$(command -v "${tool}")" "${bin}/${tool}"
done
cat >"${bin}/curl" <<'MOCK'
#!/usr/bin/env bash
set -euo pipefail
output="" url=""
while (($#)); do
  case "$1" in --output) output="$2"; shift ;; https://*) url="$1" ;; esac
  shift
done
[[ "${url}" == https://github.com/GentleKingson/ocservia/releases/download/v4.5.6/* ]] || exit 22
printf '%s\n' "${url}" >>"${TEST_ROOT}/downloads"
cp "${TEST_ROOT}/assets/${url##*/}" "${output}"
MOCK
cat >"${bin}/docker" <<'MOCK'
#!/usr/bin/env bash
exit "${DOCKER_EXIT:-0}"
MOCK
cat >"${bin}/sudo" <<'MOCK'
#!/usr/bin/env bash
exit 99
MOCK
cat >"${bin}/getent" <<'MOCK'
#!/usr/bin/env bash
[[ "$1" == ahostsv4 ]] || exit 0
[[ "${DNS_FAIL:-false}" != true ]] || exit 2
echo '192.0.2.10 STREAM test'
MOCK
cat >"${bin}/ip" <<'MOCK'
#!/usr/bin/env bash
[[ "$*" == '-o -4 addr show' ]] || exit 0
echo '1: eth0 inet 192.0.2.10/24'
MOCK
cat >"${bin}/ss" <<'MOCK'
#!/usr/bin/env bash
[[ "${PORT_BUSY:-false}" != true ]] || echo occupied
MOCK
chmod 755 "${bin}"/{curl,docker,sudo,getent,ip,ss}
export TEST_ROOT="${fixture}"
export OCSERV_CONTROLLER_SOURCE_ROOT="${fixture}/releases"
export OCSERV_PUBLIC_HOST=vpn.example.test OCSERV_CONTROLLER_ENDPOINT_ID
OCSERV_CONTROLLER_ENDPOINT_ID="$(printf 'a%.0s' {1..64})"
export OCSERV_AUDIT_EVENT_KEY_ID=audit-event-v1 OCSERV_LOCAL_AUTH_ENABLED=true
export OCSERV_RELAY_URL_A=https://relay.example.test OCSERV_CERTIFICATE_SIGNER_URL=https://signer.example.test/sign
export OCSERV_SECRET_DIR="${fixture}/secrets" OCSERV_BACKUP_DIR="${fixture}/backups"
run() { (cd "${fixture}/config" && PATH="${bin}" bash "${bootstrap}" "$@"); }
reject() {
  local message="$1"; shift
  if run "$@" >"${fixture}/output" 2>&1; then echo "unexpected success: $*" >&2; exit 1; fi
  grep -Fq "${message}" "${fixture}/output"
}
reject 'requires a TTY'
reject 'exact vX.Y.Z' --version latest
printf '# protected configuration\n' >"${fixture}/settings.env"
ln -s "${fixture}/settings.env" "${fixture}/config-link"
reject 'not a symlink' --non-interactive --config "${fixture}/config-link"
# Missing configuration must fail before any downloads, not prompt or mutate.
if OCSERV_PUBLIC_HOST="" run --non-interactive >"${fixture}/output" 2>&1; then exit 1; fi
grep -Fq 'OCSERV_PUBLIC_HOST is required' "${fixture}/output"
[[ ! -e "${fixture}/downloads" ]]
run --non-interactive --check
[[ ! -e "${fixture}/releases" ]]
# Replace only the final installers with recording stubs; archive validation and
# handoff still run through the real released bootstrap and runtime helper.
mkdir "${fixture}/payload"
tar -xzf "${fixture}/assets/controller-runtime-v4.5.6.tar.gz" -C "${fixture}/payload"
# All checked-in relative Compose bind mounts and extends files must exist in
# the extracted runtime. No container daemon or YAML dependency is needed.
python3 - "${fixture}/payload" <<'DEPENDENCIES'
from pathlib import Path
import re, sys
root = Path(sys.argv[1])
production = root / 'deploy/production'
for compose in production.rglob('compose*.yaml'):
    base = compose.parent if compose.parent.name == 'relay' else production
    for line in compose.read_text().splitlines():
        match = re.match(r'\s*(?:- (\.\.?/[^:]+):|file: (\./\S+))', line)
        if match:
            target = base / (match[1] or match[2])
            assert target.exists(), f'{compose}: missing {target}'
assert not (root / '.git').exists()
assert not (root / 'control-plane').exists()
DEPENDENCIES
for entry in install.sh quick-install.sh; do
  cat >"${fixture}/payload/deploy/production/${entry}" <<'MOCK'
#!/usr/bin/env bash
printf '%s\n' "$*" >"${TEST_ROOT}/handoff"
printf '%s\n' "${OCSERV_PUBLIC_HOST:-}" "${OCSERV_DATABASE_BACKEND:-}" >"${TEST_ROOT}/environment"
for name in OCSERV_LOCAL_AUTH_ENABLED OCSERV_PUBLIC_ORIGIN OCSERV_SESSION_TTL; do
  printf '%s=%s\n' "${name}" "${!name-<unset>}"
done >"${TEST_ROOT}/auth-environment"
MOCK
  chmod 755 "${fixture}/payload/deploy/production/${entry}"
done
pack() {
  (cd "${fixture}/payload" && find deploy install.env.example RELEASE -type f | sort | xargs sha256sum >SHA256SUMS)
  (cd "${fixture}/payload" && find deploy install.env.example RELEASE SHA256SUMS -type f | sort | tar -czf "${fixture}/assets/controller-runtime-v4.5.6.tar.gz" -T -)
  (cd "${fixture}/assets" && sha256sum controller-runtime-v4.5.6.tar.gz >controller-runtime-v4.5.6.tar.gz.sha256)
}
pack
run --non-interactive
[[ -f "${fixture}/releases/v4.5.6/RELEASE" && ! -e "${fixture}/releases/v4.5.6/.git" ]]
grep -Fq -- '--non-interactive --config' "${fixture}/handoff"
# The released bootstrap must load the documented Local auth configuration before handoff.
awk -v dir="${fixture}" '
  /^```dotenv$/ { block++; active=1; next }
  /^```$/ { active=0 }
  active { print > (dir "/auth-" block ".env") }
' "${ROOT}/docs/operations/authentication.md"
(
  unset OCSERV_LOCAL_AUTH_ENABLED OCSERV_PUBLIC_ORIGIN OCSERV_SESSION_TTL
  run --non-interactive --config "${fixture}/auth-1.env"
)
while IFS= read -r setting; do
  grep -Fxq "${setting}" "${fixture}/auth-environment"
done <"${fixture}/auth-1.env"
# Reuse the validated runtime offline; changed contents never get overwritten.
: >"${fixture}/downloads"
run --version v4.5.6
[[ ! -s "${fixture}/downloads" ]]
printf '\n# drift\n' >>"${fixture}/releases/v4.5.6/deploy/production/compose.sh"
reject 'runtime content changed' --non-interactive
[[ -f "${fixture}/releases/v4.5.6/deploy/production/compose.sh" ]]
export OCSERV_CONTROLLER_SOURCE_ROOT="${fixture}/fresh-releases"
printf 'corrupt' >>"${fixture}/assets/controller-runtime-v4.5.6.tar.gz"
reject 'runtime archive checksum mismatch' --non-interactive
[[ ! -e "${fixture}/fresh-releases/v4.5.6" ]]
pack
# Existing target and symlink ancestry fail closed.
mkdir -p "${fixture}/fresh-releases/v4.5.6"
reject 'metadata is missing' --non-interactive
ln -s "${fixture}/config" "${fixture}/linked"
if OCSERV_CONTROLLER_SOURCE_ROOT="${fixture}/linked/releases" run --non-interactive >"${fixture}/output" 2>&1; then exit 1; fi
grep -Fq 'symlinks' "${fixture}/output"
export OCSERV_CONTROLLER_SOURCE_ROOT="${fixture}/quick-releases"
quick=(--quick --controller-domain vpn.example.test --relay-domain relay.example.test --acme-email ops@example.test)
reject 'requires --root-ca-passphrase-file' --non-interactive "${quick[@]}"
if DNS_FAIL=true run "${quick[@]}" >"${fixture}/output" 2>&1; then exit 1; fi
grep -Fq 'has no IPv4' "${fixture}/output"
if PORT_BUSY=true run "${quick[@]}" >"${fixture}/output" 2>&1; then exit 1; fi
grep -Fq 'already in use' "${fixture}/output"
[[ ! -e "${fixture}/quick-releases" ]]
if OCSERV_DATABASE_BACKEND=mysql OCSERV_DATABASE_DEPLOYMENT=external OCSERV_DATABASE_BACKUP_HOST=db.example.test \
run --non-interactive >"${fixture}/output" 2>&1; then exit 1; fi
grep -Fq 'unsupported database topology' "${fixture}/output"
[[ ! -e "${fixture}/quick-releases" ]]
# Actual pseudo-terminal: defaults, cancellation and equivalent Quick handoff.
python3 - "${bootstrap}" "${bin}" "${fixture}" <<'PTY'
import os, pty, select, subprocess, sys, time
script, bindir, root = sys.argv[1:]
def wizard(confirmation, preset="quick"):
  master, slave = pty.openpty()
  env = dict(os.environ, PATH=bindir, OCSERV_CONTROLLER_SOURCE_ROOT=root+'/wizard-releases')
  child = subprocess.Popen(['bash', script], stdin=slave, stdout=slave, stderr=slave, cwd=root+'/config', env=env)
  os.close(slave)
  answers = ('\nvpn.example.test\nrelay.example.test\nops@example.test\n\n\n' if preset == 'quick' else 'manual\n'+'\n'*12)
  os.write(master, (answers+confirmation+'\n').encode())
  output = b''
  deadline = time.monotonic()+20
  try:
      while time.monotonic() < deadline:
          if select.select([master], [], [], .1)[0]:
              try: output += os.read(master, 65536)
              except OSError: break
          if child.poll() is not None: break
      assert child.wait(timeout=1) == (0 if confirmation == 'y' else 1), output.decode()
  finally:
      if child.poll() is None: child.kill(); child.wait()
      os.close(master)
  return output.decode()
assert 'installation cancelled' in wizard('n')
assert not os.path.exists(root+'/wizard-releases')
assert 'Proceed with installation?' in wizard('y')
with open(root+'/quick-handoff', 'w') as file:
  file.write(open(root+'/handoff').read())
assert 'saved configuration:' in wizard('y', 'manual')
assert os.stat(root+'/config/install.env').st_mode & 0o777 == 0o600
with open(root+'/manual-environment', 'w') as file:
  file.write(open(root+'/environment').read())
PTY
run --non-interactive --config "${fixture}/config/install.env"
cmp "${fixture}/environment" "${fixture}/manual-environment"
cp "${fixture}/quick-handoff" "${fixture}/interactive-handoff"
run "${quick[@]}" --root-ca-export-dir /root/ocservia-root-ca
cmp "${fixture}/handoff" "${fixture}/interactive-handoff"
# Shared Controller and Relay domain: explicit, defaulted, interactive wizard,
# launcher asset, and manual Integrated mode must all succeed.
run --quick --controller-domain vpn.example.test --relay-domain vpn.example.test --acme-email ops@example.test --check
run --quick --controller-domain vpn.example.test --acme-email ops@example.test
grep -Fq -- '--controller-domain vpn.example.test --acme-email ops@example.test --relay-domain vpn.example.test' "${fixture}/handoff"
(
  cd "${fixture}/config"
  PATH="${bin}" bash "${fixture}/assets/install-controller" --quick \
    --controller-domain vpn.example.test --relay-domain vpn.example.test --acme-email ops@example.test --check
  PATH="${bin}" bash "${fixture}/assets/install-controller" --quick \
    --controller-domain vpn.example.test --acme-email ops@example.test --check
)
(
  export OCSERV_DEPLOYMENT_MODE=integrated OCSERV_RELAY_PUBLIC_HOST=vpn.example.test
  export OCSERV_RELAY_SECRET_DIR="${fixture}/secrets" OCSERV_SIGNER_SECRET_DIR="${fixture}/secrets" OCSERV_SIGNER_STATE_DIR="${fixture}/secrets"
  run --non-interactive --root-lifecycle --check
)
python3 - "${bootstrap}" "${bin}" "${fixture}" <<'PTY_SHARED'
import os, pty, select, subprocess, sys, time
script, bindir, root = sys.argv[1:]
master, slave = pty.openpty()
env = dict(os.environ, PATH=bindir, OCSERV_CONTROLLER_SOURCE_ROOT=root+'/wizard-releases')
child = subprocess.Popen(['bash', script], stdin=slave, stdout=slave, stderr=slave, cwd=root+'/config', env=env)
os.close(slave)
# Choose quick explicitly (install.env exists now), enter Controller domain, press Enter to share it for Relay.
os.write(master, b'quick\nvpn.example.test\n\nops@example.test\n\n\ny\n')
output = b''
deadline = time.monotonic()+20
try:
    while time.monotonic() < deadline:
        if select.select([master], [], [], .1)[0]:
            try: output += os.read(master, 65536)
            except OSError: break
        if child.poll() is not None: break
    assert child.wait(timeout=1) == 0, output.decode()
finally:
    if child.poll() is None: child.kill(); child.wait()
    os.close(master)
handoff = open(root+'/handoff').read()
assert '--controller-domain vpn.example.test --relay-domain vpn.example.test' in handoff, handoff
PTY_SHARED
# Edge and Relay entrypoint rendering for shared and distinct domains.
python3 - "${ROOT}" "${fixture}" <<'ENTRYPOINTS'
from pathlib import Path
import os, subprocess, sys
root, fixture = Path(sys.argv[1]), Path(sys.argv[2])
edge_dir = fixture / 'edge-test'
edge_bin = edge_dir / 'bin'
edge_bin.mkdir(parents=True)
(edge_bin / 'nginx').write_text('#!/usr/bin/env bash\nexit 0\n')
(edge_bin / 'envsubst').write_text('''#!/usr/bin/env python3
import os, re, sys
allowed = set(re.findall(r'\\$\\{([A-Z0-9_]+)\\}', sys.argv[1]))
text = sys.stdin.read()
for name in allowed:
    text = text.replace('${' + name + '}', os.environ.get(name, ''))
sys.stdout.write(text)
''')
for tool in ('nginx', 'envsubst'):
    (edge_bin / tool).chmod(0o755)
edge_script = (root / 'deploy/production/integrated/edge-entrypoint.sh').read_text()
edge_script = edge_script.replace('/etc/nginx/ocservia.conf.template', str(root / 'deploy/production/integrated/nginx.conf.template'))
edge_script = edge_script.replace('/tmp/nginx.conf', str(edge_dir / 'nginx.conf'))
edge_copy = edge_dir / 'edge-entrypoint.sh'
edge_copy.write_text(edge_script)
edge_copy.chmod(0o755)
env = dict(os.environ, PATH=f"{edge_bin}:{os.environ['PATH']}", OCSERV_PUBLIC_HOST='vpn.example.test', OCSERV_RELAY_PUBLIC_HOST='vpn.example.test')
subprocess.run(['sh', str(edge_copy)], env=env, check=True)
shared_conf = (edge_dir / 'nginx.conf').read_text()
assert shared_conf.count('vpn.example.test') == 1, shared_conf
assert '127.0.0.1:9443;' not in shared_conf, shared_conf
env['OCSERV_RELAY_PUBLIC_HOST'] = 'relay.example.test'
subprocess.run(['sh', str(edge_copy)], env=env, check=True)
distinct_conf = (edge_dir / 'nginx.conf').read_text()
assert 'vpn.example.test gateway:8443;' in distinct_conf, distinct_conf
assert 'relay.example.test 127.0.0.1:9443;' in distinct_conf, distinct_conf

relay_dir = fixture / 'relay-test'
acme_cert_dir = relay_dir / 'caddy-acme/certificates/acme-v02.api.letsencrypt.org-directory/vpn.example.test'
acme_cert_dir.mkdir(parents=True)
(acme_cert_dir / 'vpn.example.test.crt').write_text('-----BEGIN CERTIFICATE-----\ntest\n-----END CERTIFICATE-----\n')
(acme_cert_dir / 'vpn.example.test.key').write_text('-----BEGIN PRIVATE KEY-----\ntest\n-----END PRIVATE KEY-----\n')
(relay_dir / 'token').write_text('a' * 40)
relay_stub = relay_dir / 'iroh-relay-stub'
relay_stub.write_text('#!/usr/bin/env bash\nprintf "%s\\n" "$*" > "' + str(relay_dir / 'relay-args') + '"\n')
relay_stub.chmod(0o755)
relay_script = (root / 'deploy/production/relay-entrypoint.sh').read_text()
relay_script = relay_script.replace('/tmp/relay.toml', str(relay_dir / 'relay.toml'))
relay_script = relay_script.replace('/usr/local/bin/iroh-relay', str(relay_stub))
relay_copy = relay_dir / 'relay-entrypoint.sh'
relay_copy.write_text(relay_script)
relay_copy.chmod(0o755)
relay_env = dict(
    os.environ,
    IROH_RELAY_ACCESS_TOKEN_FILE=str(relay_dir / 'token'),
    OCSERV_RELAY_CONFIG_TEMPLATE=str(root / 'deploy/production/integrated/relay.shared-acme.toml'),
    OCSERV_RELAY_PUBLIC_HOST='vpn.example.test',
    OCSERV_RELAY_SHARED_ACME_DIR=str(relay_dir / 'caddy-acme'),
)
subprocess.run(['bash', str(relay_copy), '--dev', '--config-path', str(relay_dir / 'relay.toml')], env=relay_env, check=True)
rendered = (relay_dir / 'relay.toml').read_text()
assert 'cert_mode = "Reloading"' in rendered, rendered
assert f'manual_cert_path = "{acme_cert_dir}/vpn.example.test.crt"' in rendered, rendered
assert f'manual_key_path = "{acme_cert_dir}/vpn.example.test.key"' in rendered, rendered
assert '@OCSERV_RELAY_' not in rendered, rendered
ENTRYPOINTS
