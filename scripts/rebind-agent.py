#!/usr/bin/python3
"""Explicit root-only Controller transfer. No history is deleted by this tool."""
import argparse
from contextlib import closing
import fcntl
import hashlib
import json
import os
from pathlib import Path
import pwd
import re
import shlex
import sqlite3
import stat
import subprocess
import sys
import time
import uuid

CONF = Path('/etc/ocservia-agent')
STATE = Path('/var/lib/ocservia-rebind')
AGENT_STATE = Path('/var/lib/ocservia-agent')
PRIVD_STATE = Path('/var/lib/ocservia-privd')
UPGRADE_STATE = Path('/var/lib/ocservia-upgrade')
BIN = Path('/usr/libexec/ocservia')
ACTIVE = CONF / 'active-binding'
HEX = re.compile(r'[0-9a-f]{64}')


def require(condition, detail):
    if not condition:
        raise RuntimeError(detail)


def sync_directory(path):
    fd = os.open(path, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
    try:
        os.fsync(fd)
    finally:
        os.close(fd)


def secure_read(path, limit=65536):
    """Descriptor-relative root trust walk, including the final file."""
    path = Path(path)
    require(path.is_absolute() and '..' not in path.parts, 'trusted path must be absolute')
    fd = os.open('/', os.O_RDONLY | os.O_DIRECTORY)
    try:
        for part in path.parts[1:-1]:
            next_fd = os.open(part, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW, dir_fd=fd)
            os.close(fd)
            fd = next_fd
            meta = os.fstat(fd)
            require(meta.st_uid == 0 and not meta.st_mode & 0o022, 'unsafe trusted path ancestry')
        file_fd = os.open(path.name, os.O_RDONLY | os.O_NOFOLLOW, dir_fd=fd)
        with os.fdopen(file_fd, 'rb') as source:
            meta = os.fstat(source.fileno())
            require(stat.S_ISREG(meta.st_mode) and meta.st_uid == 0 and meta.st_nlink == 1
                    and not meta.st_mode & 0o027, 'trusted file must be root-owned and protected')
            data = source.read(limit + 1)
            require(len(data) <= limit, 'trusted file too large')
            return data
    finally:
        os.close(fd)


def atomic_write(path, data, mode=0o600, gid=0):
    path = Path(path)
    temporary = path.with_name('.' + path.name + '.' + str(uuid.uuid4()))
    fd = os.open(temporary, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, mode)
    with os.fdopen(fd, 'wb') as output:
        os.fchown(output.fileno(), 0, gid)
        os.fchmod(output.fileno(), mode)
        output.write(data)
        output.flush()
        os.fsync(output.fileno())
    os.replace(temporary, path)
    sync_directory(path.parent)


def directory_handle(path, agent_uid=None):
    require(path.is_absolute() and '..' not in path.parts, 'state directory must be absolute')
    fd = os.open('/', os.O_RDONLY | os.O_DIRECTORY)
    try:
        for part in path.parts[1:]:
            next_fd = os.open(part, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW, dir_fd=fd)
            os.close(fd)
            fd = next_fd
            meta = os.fstat(fd)
            require(meta.st_uid in (0, agent_uid) and not meta.st_mode & 0o022,
                    'unsafe state directory ancestry')
        return fd
    except BaseException:
        os.close(fd)
        raise


def managed_directory(path, uid=0, gid=0, mode=0o700):
    parent = directory_handle(path.parent, uid)
    try:
        try:
            directory = os.open(path.name, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW, dir_fd=parent)
        except FileNotFoundError:
            directory = None
        if directory is not None:
            try:
                meta = os.fstat(directory)
                require(meta.st_uid == uid and meta.st_gid == gid and stat.S_IMODE(meta.st_mode) == mode,
                        'existing directory has unsafe metadata: ' + str(path))
            finally:
                os.close(directory)
            return
        # Agent-owned parents are adversarial. Create/chown through a descriptor
        # in root-owned staging before publication, never chown a pathname that
        # an Agent could substitute with an existing root-private directory.
        staging_parent = directory_handle(STATE) if uid else os.dup(parent)
        name = '.directory-' + str(uuid.uuid4())
        try:
            os.mkdir(name, mode=0o700, dir_fd=staging_parent)
            directory = os.open(name, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW, dir_fd=staging_parent)
            try:
                os.fchown(directory, uid, gid)
                os.fchmod(directory, mode)
                os.fsync(directory)
                os.rename(name, path.name, src_dir_fd=staging_parent, dst_dir_fd=parent)
                os.fsync(staging_parent)
                os.fsync(parent)
            finally:
                os.close(directory)
        finally:
            os.close(staging_parent)
    finally:
        os.close(parent)


def publish_identity(stage, target, account):
    source_fd = directory_handle(stage.parent, account.pw_uid)
    try:
        target_fd = directory_handle(target, account.pw_uid)
        try:
            # Both directory descriptors stay pinned even if the Agent renames
            # a parent; root never follows an Agent-supplied destination symlink.
            try:
                os.stat('identity', dir_fd=target_fd, follow_symlinks=False)
            except FileNotFoundError:
                pass
            else:
                raise RuntimeError('target identity already exists')
            os.rename('identity', 'identity', src_dir_fd=source_fd, dst_dir_fd=target_fd)
            os.fsync(source_fd)
            os.fsync(target_fd)
        finally:
            os.close(target_fd)
    finally:
        os.close(source_fd)


def environment():
    result = {}
    for line in secure_read(CONF / 'agent.env').decode().splitlines():
        if not line.strip() or line.lstrip().startswith('#'):
            continue
        key, separator, value = line.partition('=')
        require(separator and key not in result and re.fullmatch(r'[A-Z_]+', key), 'invalid or duplicate agent.env assignment')
        values = shlex.split(value)
        require(len(values) == 1, 'agent.env values must be explicit single values')
        result[key] = values[0]
    return result


def public_key(path):
    pem = secure_read(path, 4096)
    result = subprocess.run(['/usr/bin/openssl', 'pkey', '-pubin', '-outform', 'DER'],
                            input=pem, capture_output=True, check=True, timeout=10).stdout
    prefix = bytes.fromhex('302a300506032b6570032100')
    require(len(result) == 44 and result.startswith(prefix), 'command key must be Ed25519 SPKI PEM')
    return result[12:].hex()


def node_id(text):
    value = uuid.UUID(text)
    require(str(value) == text and value.version == 7, 'Controller node must be canonical UUIDv7')
    return text


def decode_binding(raw):
    fields = raw.decode().splitlines()
    require(len(fields) == 6 and fields[0] == 'ocservia-binding-v1'
            and fields[5] in ('clear', 'blocked'), 'invalid active binding')
    node_id(fields[1])
    require(all(HEX.fullmatch(item) for item in fields[2:5]), 'invalid binding authority')
    require(raw == ('\n'.join(fields) + '\n').encode(), 'noncanonical active binding')
    return dict(node=fields[1], controller=fields[2], endpoint=fields[3], key=fields[4],
                blocked=fields[5] == 'blocked')


def managed_units():
    """Refuse custom launchers/state paths rather than inspect the wrong evidence."""
    common = {'--node-id', '--controller-command-key-file', '--user-password-seal-key-id',
              '--user-password-seal-public-key-sha256', '--p12-password-seal-key-id',
              '--p12-password-seal-public-key-sha256'}
    for name, additional in [('agent', {'--controller'}),
                             ('privd', {'--agent-uid', '--attestation-key-file',
                                        '--user-password-seal-key-file', '--p12-password-seal-key-file'})]:
        result = subprocess.run(['/usr/bin/systemctl', 'show', 'ocservia-' + name + '.service',
                                 '--property=ExecStart', '--value'],
                                check=True, capture_output=True, timeout=10).stdout.decode()
        executable = str(BIN / ('ocservia-' + name))
        launchers = [executable]
        if name == 'agent':
            launchers.append(str(BIN / 'ocservia-agent-relays'))
        require(any('path=' + path + ' ;' in result for path in launchers)
                and result.count('path=') == 1, 'rebind requires packaged managed service launchers')
        require(set(re.findall(r'--[a-z][a-z-]*', result)) <= common | additional,
                'custom service arguments/state paths require explicit recovery before rebind')


def current_binding(env):
    try:
        binding = decode_binding(secure_read(ACTIVE, 1024))
        directory = AGENT_STATE / 'bindings' / binding['node']
        binding.update(identity=str(directory / 'identity'), journal=str(directory / 'agent.db'),
                       effects=str(PRIVD_STATE / 'bindings' / binding['node'] / 'desired-effects.sqlite3'))
        return binding
    except FileNotFoundError:
        require('AGENT_ENDPOINT_ID' in env, 'agent.env lacks the enrolled AGENT_ENDPOINT_ID; independently verify and provision it before rebind')
        node = node_id(env['NODE_ID'])
        endpoint, controller = env['AGENT_ENDPOINT_ID'], env['CONTROLLER_ENDPOINT_ID']
        require(HEX.fullmatch(endpoint) and HEX.fullmatch(controller), 'missing enrolled endpoint identity')
        return dict(node=node, endpoint=endpoint, controller=controller,
                    key=public_key(env['CONTROLLER_COMMAND_VERIFICATION_KEY_FILE']), blocked=False,
                    identity=str(AGENT_STATE / 'identity'), journal=str(AGENT_STATE / 'agent.db'),
                    effects=str(PRIVD_STATE / 'desired-effects.sqlite3'))


def agent(command, account, timeout=90):
    return subprocess.run([str(BIN / 'ocservia-agent')] + command, check=True,
                          user=account.pw_uid, group=account.pw_gid, extra_groups=[],
                          env={'PATH': '/usr/bin:/bin', 'RUST_LOG': 'warn'},
                          capture_output=True, timeout=timeout).stdout.decode().strip()


def save(operation, state):
    atomic_write(operation / 'state.json', (json.dumps(state, sort_keys=True) + '\n').encode())


def read_operation(identifier):
    require(str(uuid.UUID(identifier)) == identifier, 'invalid local operation ID')
    path = STATE / identifier
    state = json.loads(secure_read(path / 'state.json'))
    require(state['id'] == identifier, 'operation identity mismatch')
    return path, state


def relay_arguments():
    # The existing relay trust is independently provisioned, like first enrollment.
    path = CONF / 'relays.env'
    try:
        raw = secure_read(path).decode()
    except FileNotFoundError:
        return []
    values = {}
    for line in raw.splitlines():
        if not line.strip() or line.lstrip().startswith('#'):
            continue
        key, separator, value = line.partition('=')
        require(separator and key not in values, 'invalid relay environment')
        parsed = shlex.split(value)
        require(len(parsed) <= 1, 'invalid relay value')
        values[key] = parsed[0] if parsed else ''
    require(values.get('RELAY_URL_A', '').startswith('https://'), 'relay A is not provisioned')
    args = ['--relay-mode', 'custom', '--relay-url', values['RELAY_URL_A'],
            '--relay-token-file', str(CONF / 'relay-access-token')]
    if values.get('RELAY_URL_B'):
        args += ['--relay-url', values['RELAY_URL_B']]
    if (CONF / 'relay-ca.pem').exists():
        args += ['--relay-ca-file', str(CONF / 'relay-ca.pem')]
    return args


def enroll(operation, state, env, account):
    require(state['phase'] in ('prepared', 'enrolling'), 'operation is not awaiting enrollment')
    require(current_binding(env) == state['source'], 'source authority changed; do not resume this operation')
    stage = AGENT_STATE / 'rebind-staging' / state['id']
    managed_directory(AGENT_STATE / 'rebind-staging', account.pw_uid, account.pw_gid)
    managed_directory(stage, account.pw_uid, account.pw_gid)
    identity = stage / 'identity'
    if not identity.exists():
        agent(['--stage-rebind', state['source']['identity'], str(identity), state['source']['endpoint'],
               state['source']['controller'], state['controller']], account)
    require((identity / 'endpoint.key').is_file() and (identity / 'controller.endpoint').is_file(), 'staged identity is incomplete; retain it for recovery')
    observed = agent(['--identity-dir', str(identity), '--controller', state['controller'], '--prepare-enrollment'], account)
    require(observed == state['source']['endpoint'], 'staged identity changed')
    token = secure_read(operation / 'token', 4096)
    require(hashlib.sha256(token).hexdigest() == state['token_hash'], 'staged token changed')
    state['phase'] = 'enrolling'
    save(operation, state)  # response loss resumes this same token and EndpointID
    args = ['--identity-dir', str(identity), '--controller', state['controller'],
            '--enrollment-token-file', str(operation / 'token'), '--enrollment-environment', state['environment']]
    for flag, key in [('user-password-seal-key-id', 'USER_PASSWORD_SEAL_KEY_ID'),
                      ('user-password-seal-public-key-sha256', 'USER_PASSWORD_SEAL_PUBLIC_KEY_SHA256'),
                      ('p12-password-seal-key-id', 'P12_PASSWORD_SEAL_KEY_ID'),
                      ('p12-password-seal-public-key-sha256', 'P12_PASSWORD_SEAL_PUBLIC_KEY_SHA256')]:
        args += ['--' + flag, env[key]]
    target_node = node_id(agent(args + relay_arguments(), account))
    require(target_node != state['source']['node'], 'target reused the source NodeID')
    state.update(node=target_node, phase='enrolled', enrolled_at=int(time.time()))
    save(operation, state)
    print('ENROLLED_LOCAL operation=' + state['id'] + ' node=' + target_node)
    print('Approve this pending node through the existing Controller approval flow, then run commit.')


def unresolved(path, queries, missing_blocks):
    path = Path(path)
    if not path.exists():
        return missing_blocks
    require(not path.is_symlink() and path.is_file(), 'unsafe recovery database')
    try:
        with closing(sqlite3.connect(path.as_uri() + '?mode=ro', uri=True, timeout=5)) as database:
            database.execute('PRAGMA trusted_schema=OFF')
            database.execute('PRAGMA query_only=ON')
            database.set_progress_handler(lambda: 1, 1000000)
            return any(database.execute(query).fetchone()[0] for query in queries)
    except sqlite3.Error:
        return True  # missing/corrupt recovery evidence never means no work


def unresolved_upgrades(source):
    directory = UPGRADE_STATE / 'operations'
    if Path(source['identity']).parent.name == source['node']:
        directory = UPGRADE_STATE / 'bindings' / source['node'] / 'operations'
    if not directory.exists():
        return False
    try:
        require(not directory.is_symlink(), 'unsafe upgrade evidence')
        for operation in directory.iterdir():
            if operation.is_dir():
                state = secure_read(operation / 'state', 4096).decode().splitlines()[0]
                if state not in ('succeeded', 'failed', 'rolled_back'):
                    return True
        return False
    except (OSError, RuntimeError, UnicodeError, IndexError):
        return True


def target_record(state, blocked):
    return ('\n'.join(['ocservia-binding-v1', state['node'], state['controller'],
                      state['source']['endpoint'], state['key'], 'blocked' if blocked else 'clear']) + '\n').encode()


def commit(operation, state, env, account):
    require(state['phase'] in ('enrolled', 'committing', 'committed', 'verified'), 'enrollment is not confirmed')
    if state['phase'] in ('committed', 'verified'):
        require(secure_active() == bytes.fromhex(state['record_hex']), 'active binding changed; refuse stale operation')
    if state['phase'] == 'verified':
        require(secure_read(ACTIVE) == bytes.fromhex(state['record_hex']), 'active binding changed')
        print('VERIFIED operation=' + state['id'])
        return
    if state['phase'] in ('enrolled', 'committing'):
        current = current_binding(env)
        already_committed = state.get('record_hex') and secure_active() == bytes.fromhex(state['record_hex'])
        require(already_committed or current == state['source'], 'source authority changed')
        subprocess.run(['/usr/bin/systemctl', 'stop', 'ocservia-agent.service', 'ocservia-privd.service'], check=True, timeout=60)
        units = subprocess.run(['/usr/bin/systemctl', 'list-units', '--all', '--plain', '--no-legend',
                                'ocservia-upgrader@*.service'], check=True, capture_output=True, timeout=10)
        names = [line.split()[0] for line in units.stdout.decode().splitlines() if line.split()]
        if names:
            subprocess.run(['/usr/bin/systemctl', 'stop'] + names, check=True, timeout=60)
        state['phase'] = 'committing'
        save(operation, state)
        if not already_committed:
            source = state['source']
            blocked = source['blocked'] or unresolved_upgrades(source) or unresolved(source['journal'], [
                "SELECT EXISTS(SELECT 1 FROM command_journal WHERE state NOT IN ('succeeded','failed'))"], True)
            blocked = blocked or unresolved(source['effects'], [
                "SELECT EXISTS(SELECT 1 FROM authorized_effects WHERE state='prepared')",
                "SELECT EXISTS(SELECT 1 FROM desired_effects WHERE state='prepared')"], False)
            parent = AGENT_STATE / 'bindings'
            managed_directory(parent, account.pw_uid, account.pw_gid)
            target = parent / state['node']
            managed_directory(target, account.pw_uid, account.pw_gid)
            stage = AGENT_STATE / 'rebind-staging' / state['id'] / 'identity'
            if stage.exists():
                require(not (target / 'identity').exists(), 'target identity already exists')
                publish_identity(stage, target, account)
            require((target / 'identity' / 'endpoint.key').is_file() and (target / 'identity' / 'controller.endpoint').is_file(), 'target identity is incomplete')
            observed = agent(['--identity-dir', str(target / 'identity'), '--controller', state['controller'], '--prepare-enrollment'], account)
            require(observed == source['endpoint'], 'target endpoint changed')
            for root in (PRIVD_STATE, UPGRADE_STATE):
                managed_directory(root / 'bindings')
                managed_directory(root / 'bindings' / state['node'])
            record = target_record(state, blocked)
            state.update(record_hex=record.hex(), commit_started_at=int(time.time()), mutations_blocked=blocked)
            save(operation, state)
            # This is the only authority publication. Source files remain intact.
            atomic_write(ACTIVE, record, 0o640, account.pw_gid)
        state['phase'] = 'committed'
        save(operation, state)
    subprocess.run(['/usr/bin/systemctl', 'start', 'ocservia-privd.service', 'ocservia-agent.service'], check=True, timeout=60)
    deadline = time.monotonic() + 60
    journal = AGENT_STATE / 'bindings' / state['node'] / 'agent.db'
    while time.monotonic() < deadline:
        try:
            with closing(sqlite3.connect(journal.as_uri() + '?mode=ro', uri=True, timeout=1)) as database:
                database.execute('PRAGMA trusted_schema=OFF')
                row = database.execute("SELECT value FROM agent_metadata WHERE key='verified_session_grant'").fetchone()
            if row:
                result = subprocess.run([str(BIN / 'ocservia-privd'), '--verify-rebind-session'], input=row[0],
                                        capture_output=True, timeout=5)
                if result.returncode == 0 and int(result.stdout) >= state['commit_started_at'] - 300:
                    state.update(phase='verified', verified_at=int(time.time()), source_state='sealed')
                    save(operation, state)
                    print('VERIFIED operation=' + state['id'] + ' node=' + state['node'])
                    if state['mutations_blocked']:
                        print('Mutations are quarantined: preserve and reconcile unresolved source evidence.')
                    return
        except (sqlite3.Error, ValueError, subprocess.TimeoutExpired):
            pass
        time.sleep(1)
    raise RuntimeError('target session not verified; target remains committed, source evidence retained; resolve approval/connectivity and resume commit')


def secure_active():
    try:
        return secure_read(ACTIVE, 1024)
    except FileNotFoundError:
        return None


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    sub = parser.add_subparsers(dest='action', required=True)
    prepare = sub.add_parser('prepare')
    for name in ('controller', 'command-key-file', 'token-file', 'environment', 'reason'):
        prepare.add_argument('--' + name, required=True)
    prepare.add_argument('--source-disposition', required=True, choices=('revoked', 'unreachable'))
    prepare.add_argument('--dry-run', action='store_true')
    for name in ('resume', 'commit', 'status'):
        sub.add_parser(name).add_argument('operation')
    args = parser.parse_args()
    require(os.geteuid() == 0, 'rebind must run locally as root')
    account = pwd.getpwnam('ocserv-agent')
    require(account.pw_uid != 0, 'Agent account must be unprivileged')
    managed_directory(STATE, 0, account.pw_gid, 0o750)
    managed_directory(UPGRADE_STATE)
    lock_fd = os.open(UPGRADE_STATE / '.binding-lifecycle.lock', os.O_RDWR | os.O_CREAT | os.O_NOFOLLOW, 0o600)
    with os.fdopen(lock_fd, 'wb') as lock:
        fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        env = environment()
        if args.action != 'status':
            managed_units()
        if args.action != 'prepare':
            operation, state = read_operation(args.operation)
            if args.action == 'status':
                print(json.dumps({key: state.get(key) for key in ('id', 'phase', 'node', 'source_disposition', 'mutations_blocked')}, sort_keys=True))
            elif args.action == 'resume':
                enroll(operation, state, env, account)
            else:
                commit(operation, state, env, account)
            return
        for existing in STATE.iterdir():
            if not existing.name.startswith('.') and existing.is_dir():
                previous = json.loads(secure_read(existing / 'state.json'))
                require(previous['phase'] == 'verified', 'unfinished operation ' + previous['id'] + '; resume it rather than creating another enrollment')
        source = current_binding(env)
        require(HEX.fullmatch(args.controller) and args.controller != source['controller'], 'target Controller must be explicitly pinned and different')
        key = public_key(args.command_key_file)
        require(key != source['key'], 'target command signing key must differ from source authority')
        require(0 < len(args.reason.strip()) <= 512 and re.fullmatch(r'[^\s]{1,64}', args.environment), 'reason or environment invalid')
        token = secure_read(args.token_file, 4096)
        require(re.fullmatch(rb'obt1_[A-Za-z0-9_-]{43}\n?', token), 'rebind requires an existing retryable bootstrap token (obt1_)')
        for binary in ('ocservia-agent', 'ocservia-privd', 'ocservia-upgrader'):
            result = subprocess.run([str(BIN / binary), '--binding-version'], check=True, capture_output=True, timeout=5)
            require(result.stdout.strip() == b'1', 'installed binary lacks binding support')
        require(Path(source['identity']).joinpath('endpoint.key').is_file() and Path(source['identity']).joinpath('controller.endpoint').is_file(), 'source identity is incomplete; restore it, never regenerate')
        observed = agent(['--identity-dir', source['identity'], '--controller', source['controller'], '--prepare-enrollment'], account)
        require(observed == source['endpoint'], 'source EndpointID differs from enrolled identity')
        if args.dry_run:
            print('PREFLIGHT_OK endpoint=' + source['endpoint'])
            return
        identifier = str(uuid.uuid4())
        operation = STATE / identifier
        managed_directory(operation, 0, account.pw_gid, 0o750)
        atomic_write(operation / 'token', token, 0o640, account.pw_gid)
        state = dict(id=identifier, phase='prepared', source=source, controller=args.controller, key=key,
                     token_hash=hashlib.sha256(token).hexdigest(), environment=args.environment,
                     reason=args.reason, source_disposition=args.source_disposition, created_at=int(time.time()))
        save(operation, state)
        print('PREPARED operation=' + identifier, flush=True)
        enroll(operation, state, env, account)


if __name__ == '__main__':
    try:
        main()
    except (RuntimeError, OSError, ValueError, KeyError, subprocess.SubprocessError) as error:
        print('Controller rebind stopped: ' + str(error), file=sys.stderr)
        sys.exit(1)
