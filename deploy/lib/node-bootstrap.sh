#!/usr/bin/env bash
# Root provisioning for the token-only installer; embedded into release assets.
# This credential never enters Agent configuration, argv or its environment.
node_bootstrap_data() {
  python3 - "$@" <<'PY'
import base64
import getpass
import json
import os
import re
import stat
import sys
import tempfile
import warnings
from pathlib import Path
from urllib.parse import urlsplit


def reject():
    raise ValueError('invalid protected bootstrap state or response')


def read_file(path, limit=65536, source=False):
    fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    try:
        st = os.fstat(fd)
        owners = {0}
        if source and os.environ.get('SUDO_UID', '').isdigit():
            owners.add(int(os.environ['SUDO_UID']))
        if not stat.S_ISREG(st.st_mode) or st.st_uid not in owners or st.st_nlink != 1 or st.st_mode & 0o077 or st.st_size > limit:
            reject()
        with os.fdopen(fd, 'rb', closefd=False) as f:
            data = f.read(limit + 1)
        if len(data) > limit:
            reject()
        return data
    finally:
        os.close(fd)


def save(name, data):
    if isinstance(data, str):
        data = data.encode()
    path = state / name
    if path.exists() or path.is_symlink():
        read_file(path)
    fd, tmp = tempfile.mkstemp(prefix='.' + name + '.', dir=state)
    try:
        with os.fdopen(fd, 'wb') as f:
            f.write(data)
            f.flush()
            os.fsync(f.fileno())
        os.replace(tmp, path)
        fd = os.open(state, os.O_RDONLY | os.O_DIRECTORY)
        try:
            os.fsync(fd)
        finally:
            os.close(fd)
    finally:
        if os.path.exists(tmp):
            os.unlink(tmp)


def load(name):
    return json.loads(read_file(state / name))


def string(obj, key, pattern=None, limit=8192):
    value = obj.get(key)
    if not isinstance(value, str) or not value or len(value) > limit or '\x00' in value:
        reject()
    if pattern and not re.fullmatch(pattern, value):
        reject()
    return value


def token_origin(token):
    match = re.fullmatch(r'oit1_([A-Za-z0-9_-]+)\.([A-Za-z0-9_-]{43})', token)
    if not match or len(token) > 2048:
        reject()
    secret = base64.urlsafe_b64decode(match[2] + '=')
    if len(secret) != 32 or base64.urlsafe_b64encode(secret).decode().rstrip('=') != match[2]:
        reject()
    encoded = match[1]
    origin = base64.urlsafe_b64decode(encoded + '=' * (-len(encoded) % 4)).decode('ascii')
    if base64.urlsafe_b64encode(origin.encode()).decode().rstrip('=') != encoded:
        reject()
    u = urlsplit(origin)
    if u.scheme != 'https' or not u.hostname or u.username or u.password or u.path or u.query or u.fragment or re.search(r'[\s\\]', origin):
        reject()
    return origin


try:
    action, directory, *args = sys.argv[1:]
    state = Path(directory)
    if os.geteuid() != 0 or not state.is_absolute():
        reject()
    # No root provisioning under writable or symlinked ancestors.
    for parent in reversed([state, *state.parents]):
        if parent == state and not parent.exists():
            parent.mkdir(mode=0o700)
        st = parent.lstat()
        if not stat.S_ISDIR(st.st_mode) or st.st_uid != 0 or st.st_mode & 0o022:
            reject()
    if stat.S_IMODE(state.stat().st_mode) != 0o700:
        reject()
    if action == 'state':
        pass
    elif action == 'init':
        source = args[0]
        if source == 'resume':
            token = read_file(state / 'token', 4096).decode().strip()
        elif source == 'prompt':
            # Never fall back to echoing a credential on redirected input.
            with open('/dev/tty', 'r+') as tty:
                if not os.isatty(tty.fileno()):
                    reject()
                with warnings.catch_warnings():
                    warnings.simplefilter('error', getpass.GetPassWarning)
                    token = getpass.getpass('Controller installation token: ', stream=tty).strip()
        else:
            token = read_file(source, 4096, source=True).decode().strip()
        token_origin(token)
        if (state / 'token').exists() and read_file(state / 'token').decode() != token:
            reject()
        save('token', token)
    elif action == 'origin':
        print(token_origin(read_file(state / 'token').decode()))
    elif action == 'request':
        body = {'token': read_file(state / 'token').decode()}
        if args:
            node, endpoint = args[:2]
            if not re.fullmatch(r'[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}', node) or not re.fullmatch('[0-9a-f]{64}', endpoint):
                reject()
            body.update(node_id=node, endpoint_id=endpoint)
        if len(args) > 2:
            if len(args) != 7 or args[2] != 'registration':
                reject()
            body['registration'] = load('registration.json')
            body.update(zip(('user_password_seal_key_id', 'user_password_seal_public_key_sha256',
                             'p12_password_seal_key_id', 'p12_password_seal_public_key_sha256'), args[3:]))
            for key in ('user_password_seal_key_id', 'p12_password_seal_key_id'):
                string(body, key, '[A-Za-z0-9._-]{1,128}')
            for key in ('user_password_seal_public_key_sha256', 'p12_password_seal_public_key_sha256'):
                string(body, key, '[0-9a-f]{64}')
        save('request.json', json.dumps(body))
    elif action in ('config', 'cached-config'):
        c = load('config.json' if action == 'cached-config' else 'response.json')
        string(c, 'controller_endpoint_id', '[0-9a-f]{64}')
        string(c, 'environment', r'[A-Za-z0-9._-]{1,64}')
        string(c, 'enrollment_token', r'obt2_[A-Za-z0-9_-]{43}')
        key = string(c, 'command_verification_key_hex', '[0-9a-f]{64}')
        relay = string(c, 'relay_url', r'https://[^\s\"\x27`$\\?#@]+')
        token = string(c, 'relay_access_token')
        if any(ord(ch) < 32 or ord(ch) == 127 for ch in token):
            reject()
        if (state / 'config.json').exists() and load('config.json') != c:
            reject()
        save('config.json', json.dumps(c, sort_keys=True))
        save('enrollment-token', c['enrollment_token'])
        save('relay-token', token)
        der = bytes.fromhex('302a300506032b6570032100' + key)
        pem = '-----BEGIN PUBLIC KEY-----\n' + base64.b64encode(der).decode() + '\n-----END PUBLIC KEY-----\n'
        save('command-key.pem', pem)
    elif action == 'field':
        value = string(load(args[0]), args[1])
        print(value)
    elif action == 'challenge':
        c = load('response.json')
        print(string(c, 'controller_nonce_hex', '[0-9a-f]{64}'))
        print(string(c, 'credential_context_sha256_hex', '[0-9a-f]{64}'))
        if type(c.get('registered')) is not bool:
            reject()
        print('true' if c['registered'] else 'false')
    elif action == 'registered':
        c = load('response.json')
        if c.get('state') != 'active' or c.get('key_id') != load('registration.json').get('key_id'):
            reject()
        save('registered.json', json.dumps(c))
    elif action == 'online':
        c = load('response.json')
        if c.get('trust_status') != 'active' or c.get('connection_state') != 'online' or c.get('freshness') != 'fresh' or c.get('attestation_registered') is not True:
            sys.exit(1)
        save('online.json', json.dumps(c))
    else:
        reject()
except (OSError, ValueError, KeyError, TypeError, EOFError, getpass.GetPassWarning):
    print('bootstrap: invalid protected state or Controller response; original identity and token retained for recovery', file=sys.stderr)
    sys.exit(1)
PY
}

node_bootstrap_request() {
  local operation="$1"
  shift
  node_bootstrap_data request "${JOIN_STATE_DIR}" "$@" || fail 'cannot prepare bootstrap request'
  # Do not follow redirects with credentials. TLS verifies the origin encoded
  # in the token against the host trust store; no insecure fallback exists.
  curl --fail --silent --show-error --proto '=https' --tlsv1.2 \
    --connect-timeout 10 --max-time 30 --max-filesize 65536 \
    --header 'Content-Type: application/json' \
    --data-binary "@${JOIN_STATE_DIR}/request.json" \
    --output "${JOIN_STATE_DIR}/response.json" \
    "${JOIN_ORIGIN}/api/v1/node-bootstrap/${operation}" ||
    fail 'Controller bootstrap request failed; preserve root bootstrap state and rerun with the same token'
}

# shellcheck disable=SC2034 # Configuration outputs are consumed by the sourcing managed-node installer.
node_bootstrap_prepare() {
  ((EUID == 0)) || fail 'token installation requires the root lifecycle'
  command -v python3 >/dev/null || fail 'Python 3 is required for bounded bootstrap JSON parsing'
  JOIN_STATE_DIR="${SYSROOT}/var/lib/ocservia-bootstrap"
  command -v flock >/dev/null || fail 'flock is required to serialize installation recovery'
  node_bootstrap_data state "${JOIN_STATE_DIR}" || fail 'unsafe bootstrap state directory'
  exec {JOIN_LOCK_FD}>"${JOIN_STATE_DIR}/lock"
  flock -n "${JOIN_LOCK_FD}" || fail 'another installation is already using this bootstrap state'
  node_bootstrap_data init "${JOIN_STATE_DIR}" "${JOIN_TOKEN_FILE:-prompt}" || fail 'invalid installation token or protected state'
  JOIN_ORIGIN="$(node_bootstrap_data origin "${JOIN_STATE_DIR}")" || fail 'invalid Controller origin'
  if [[ -f "${JOIN_STATE_DIR}/config.json" ]]; then
    # Authenticated configuration is durable before package installation. Reuse
    # it after token expiry only to recover the original bound node; Controller
    # still rejects new enrollment or first key registration after expiry.
    node_bootstrap_data cached-config "${JOIN_STATE_DIR}" || fail 'invalid retained bootstrap configuration'
  else
    node_bootstrap_request config
    node_bootstrap_data config "${JOIN_STATE_DIR}" || fail 'invalid bootstrap configuration'
  fi
  CONTROLLER_ENDPOINT_ID="$(node_bootstrap_data field "${JOIN_STATE_DIR}" config.json controller_endpoint_id)"
  ENROLLMENT_ENVIRONMENT="$(node_bootstrap_data field "${JOIN_STATE_DIR}" config.json environment)"
  RELAY_URL_A="$(node_bootstrap_data field "${JOIN_STATE_DIR}" config.json relay_url)"
  BOOTSTRAP_TOKEN_SOURCE="${JOIN_STATE_DIR}/enrollment-token"
  RELAY_ACCESS_TOKEN_SOURCE="${JOIN_STATE_DIR}/relay-token"
  CONTROLLER_COMMAND_VERIFICATION_KEY_SOURCE="${JOIN_STATE_DIR}/command-key.pem"
}

node_bootstrap_complete() {
  local node_id="$1" challenge nonce context registered key_path
  key_path="${SYSROOT}/var/lib/ocservia-privd/attestation.key"
  node_bootstrap_request attestation "${node_id}" "${ENDPOINT_ID}"
  challenge="$(node_bootstrap_data challenge "${JOIN_STATE_DIR}")" || fail 'invalid privd registration challenge'
  nonce="${challenge%%$'\n'*}"
  context="${challenge#*$'\n'}"
  registered="${context#*$'\n'}"
  context="${context%%$'\n'*}"
  if [[ "${registered}" == true && ( ! -f "${key_path}" || -L "${key_path}" ) ]]; then
    fail 'registered privd key is missing or unsafe; restore the original root key before resuming; installation never recreates a registered key'
  fi
  "${PRIVD_BINARY}" attestation-registration "${key_path}" "${node_id}" "${nonce}" "${context}" \
    >"${JOIN_STATE_DIR}/registration.json" || fail 'root privd attestation signing failed'
  node_bootstrap_request register "${node_id}" "${ENDPOINT_ID}" registration \
    "${USER_PASSWORD_SEAL_KEY_ID}" "${USER_SEAL_DESCRIPTOR}" \
    "${P12_PASSWORD_SEAL_KEY_ID}" "${P12_SEAL_DESCRIPTOR}"
  node_bootstrap_data registered "${JOIN_STATE_DIR}" || fail 'Controller did not acknowledge the exact privd key'
  systemctl enable --now ocservia-privd.service ocservia-agent.service
  # A running process is not evidence of an accepted Controller session.
  # Leave room for config/challenge/register in the shared 30-per-minute budget.
  for _ in {1..20}; do
    node_bootstrap_request status "${node_id}" "${ENDPOINT_ID}"
    if node_bootstrap_data online "${JOIN_STATE_DIR}"; then
      echo 'ONLINE'
      echo "NODE_ID: ${node_id}"
      echo 'Controller confirmed active trust, registered privd key, online session and fresh telemetry.'
      return
    fi
    sleep 3
  done
  fail 'services started but Controller has not confirmed online/fresh state; preserve identity and root bootstrap state, repair connectivity and rerun --resume-install'
}
