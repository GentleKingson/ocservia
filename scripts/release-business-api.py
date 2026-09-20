#!/usr/bin/env python3
"""Bounded HTTPS/real-node checks for release-business-probe.sh, not a simulator."""
import base64
import http.cookiejar
import json
import os
from pathlib import Path
import secrets
import ssl
import subprocess
import sys
import time
import urllib.error
import urllib.request

ROOT = Path(__file__).resolve().parents[1]
WORK = Path(os.environ['T07_WORK'])
EVIDENCE = Path(os.environ['ARTIFACT_DIR'])
WORKSPACE = os.environ['T07_WORKSPACE']
ORIGIN = 'https://localhost'
CONTEXT = ssl.create_default_context(cafile=str(WORK / 'ca.crt'))
CLIENTS = {}


def run(*args, data=None):
    result = subprocess.run(args, input=data, capture_output=True, check=False)
    if result.returncode:
        raise RuntimeError(f'{args[0]} failed with exit {result.returncode}')
    return result.stdout.decode()


def sql(query):
    return run(str(ROOT / 'deploy/production/compose.sh'), 'exec', '-T', 'postgres',
               'psql', '-XAt', '-v', 'ON_ERROR_STOP=1', '-U', 'ocservia_owner', '-d', 'ocservia',
               data=query.encode()).strip()


def record(name, **details):
    with (EVIDENCE / 'api-checkpoints.jsonl').open('a') as output:
        output.write(json.dumps({'name': name, 'status': 'PASS', 'time': time.time(), **details}) + '\n')


def client(role):
    if role not in CLIENTS:
        jar = http.cookiejar.LWPCookieJar(str(WORK / 'private' / f'{role}.cookies'))
        if Path(jar.filename).exists():
            jar.load(ignore_discard=True)
        CLIENTS[role] = (urllib.request.build_opener(urllib.request.HTTPSHandler(context=CONTEXT),
                                                   urllib.request.HTTPCookieProcessor(jar)), jar)
    return CLIENTS[role]


def api(path, body=None, *, role='requester', status=200, headers=None, method=None):
    opener, jar = client(role)
    effective = {'Origin': ORIGIN, 'X-Workspace-ID': WORKSPACE, 'Content-Type': 'application/json'}
    effective.update(headers or {})
    request = urllib.request.Request(ORIGIN + '/api/v1/' + path,
                                    data=None if body is None else json.dumps(body).encode(),
                                    headers=effective, method=method)
    try:
        response = opener.open(request, timeout=15)
    except urllib.error.HTTPError as error:
        response = error
    raw = response.read()
    expected = (status,) if isinstance(status, int) else status
    if response.status not in expected:
        # Never include a response body: successful token/download endpoints
        # and auth errors must not accidentally export credential material.
        raise RuntimeError(f'{request.get_method()} {path}: HTTP {response.status}, expected {status}')
    jar.save(ignore_discard=True)
    return json.loads(raw) if raw else None


def wait_for(description, fn, seconds=120):
    deadline = time.monotonic() + seconds
    while time.monotonic() < deadline:
        value = fn()
        if value:
            return value
        time.sleep(2)
    raise RuntimeError(f'timeout: {description}')


def approval(action, resource_type, resource_id, extra=None):
    request = api('approval-requests', {'action': action, 'resource_type': resource_type,
                                      'resource_id': resource_id, 'reason': 'T07 isolated validation',
                                      'ttl_seconds': 600, **(extra or {})}, status=201)
    decision = {'reason': 'T07 independent identity', 'expected_request_hash': request['request_hash']}
    api(f"approval-requests/{request['id']}:approve", decision, status=403)
    api(f"approval-requests/{request['id']}:approve", decision, role='approver')
    record('self_approval_rejected', approval_id=request['id'])
    return request['id']


def local():
    for role in ('requester', 'approver'):
        api('auth/login', {'username': 't07-' + role,
                           'password': (WORK / 'private' / f'{role}-password').read_text().strip()},
            role=role, status=204)
        cookies = list(client(role)[1])
        assert any(c.name == '__Host-ocservia_session' and c.secure and c.path == '/' for c in cookies)
    api('auth/login', {'username': 't07-requester', 'password': 'incorrect-T07-password'}, role='wrong', status=401)
    api('nodes', role='anonymous', status=401)
    api('nodes', headers={'X-Workspace-ID': '00000000-0000-7000-8000-000000000072'}, status=403)
    api('nodes')
    ids = sql("SELECT string_agg(id::text, ',' ORDER BY id) FROM identities WHERE issuer='local';").split(',')
    assert len(ids) == 2 and len(set(ids)) == 2
    record('local_auth_and_workspace_isolation', identity_ids=ids)


def token():
    result = api('enrollment-tokens', {'workspace_id': WORKSPACE, 'environment': 'production',
                                      'expected_node_name': 't07-native',
                                      'expected_endpoint_id': os.environ['T07_ENDPOINT'],
                                      'reason': 'T07 isolated native node'}, status=201)
    (WORK / 'private' / 'enrollment-token').write_text(result['token'])


def approve():
    node = os.environ['T07_NODE']
    assert sql(f"SELECT status FROM nodes WHERE id='{node}';") == 'pending'
    caps = json.loads(sql(f"SELECT json_agg(capability ORDER BY capability) FROM node_capabilities WHERE node_id='{node}';"))
    binding = {'labels': {}, 'policy': 'standard', 'capabilities': caps}
    api(f'nodes/{node}/approval', {**binding, 'reason': 'T07 isolated validation'}, status=400)
    decision = approval('node.approve', 'node', node, {'node_approval': binding})
    api(f'nodes/{node}/approval', {**binding, 'reason': 'T07 isolated validation'}, headers={'X-Approval-ID': decision})
    assert sql(f"SELECT status FROM nodes WHERE id='{node}';") == 'active'
    credential = api(f'nodes/{node}/privd-attestation-credentials',
                     {'ttl_seconds': 300, 'reason': 'T07 receipt authority'}, status=201)
    proof = json.loads(run('sudo', '/usr/libexec/ocservia/ocservia-privd', 'attestation-registration',
                          '/var/lib/ocservia-privd/attestation.key', node,
                          credential['controller_nonce_hex'], credential['credential_context_sha256_hex']))
    proof['credential'] = credential['credential']
    api(f'nodes/{node}/privd-attestation-keys:register', proof, role='anonymous', status=201)
    record('pending_approve_active_and_receipt_authority', node_id=node)


def business():
    node = os.environ['T07_NODE']
    prefix = f'nodes/{node}'
    wait_for('online node', lambda: api(prefix).get('connection_state') == 'online')
    observed = api(prefix)
    assert observed['agent_version'] == os.environ['VERSION']
    pid = run('systemctl', 'show', 'ocservia-agent', '-p', 'MainPID', '--value').strip()
    agent_argv = run('sudo', 'cat', f'/proc/{int(pid)}/cmdline').rstrip('\0').split('\0')
    transport_argv = run('docker', 'exec', os.environ['T07_TRANSPORT_CONTAINER'], 'sh', '-c',
                         'for exe in /proc/[0-9]*/exe; do '
                         'if [ "$(readlink "$exe")" = /usr/local/bin/ocservia-transportd ]; then '
                         'cat "${exe%/exe}/cmdline"; fi; done').rstrip('\0').split('\0')
    for argv in (agent_argv, transport_argv):
        assert [argv[i + 1] for i, arg in enumerate(argv) if arg == '--relay-url'] == [os.environ['RELAY_URL_A']]
        assert argv[argv.index('--relay-mode') + 1] == 'custom'
    record('node_online_single_relay_argv', agent_version=observed['agent_version'], relay=os.environ['RELAY_URL_A'])
    operations = []

    def completed(operation):
        result = api('operations/' + operation['id'])
        (EVIDENCE / ('operation-' + operation['id'] + '.json')).write_text(json.dumps(result))
        if result['state'] in ('failed', 'expired', 'cancelled'):
            raise RuntimeError(f"operation {operation['id']} reached {result['state']}")
        return result if result['state'] == 'succeeded' else None

    def mutation(path, body, revision, method='POST', key=None, extra_headers=None):
        headers = {'Idempotency-Key': key or secrets.token_hex(16), 'If-Match': f'"revision-{revision}"'}
        headers.update(extra_headers or {})
        operation = api(prefix + '/' + path, {**body, 'reason': 'T07 isolated validation'},
                        status=202, headers=headers, method=method)
        wait_for('operation ' + operation['id'], lambda: completed(operation))
        operations.append(operation)
        return operation, headers

    def sealed(password):
        encrypted = subprocess.run(['openssl', 'pkeyutl', '-encrypt', '-pubin', '-inkey', str(WORK / 'user.pub.pem'),
                                    '-pkeyopt', 'rsa_padding_mode:oaep', '-pkeyopt', 'rsa_oaep_md:sha256'],
                                   input=password.encode(), capture_output=True, check=True).stdout
        return {'version': 1, 'purpose': 'user_password', 'key_id': 't07-user',
                'ciphertext': base64.b64encode(encrypted).decode()}

    def user_revision():
        state = api(prefix + '/user-group-state')
        return next(item['desired_version'] for item in state['items'] if item['kind'] == 'user' and item['name'] == 't07-vpn')

    def authenticate(password, success):
        result = subprocess.run(['sudo', 'ip', 'netns', 'exec', 't07-client', 'timeout', '20', 'openconnect',
                                 '--authenticate', '--non-inter', '--protocol=anyconnect', '--user=t07-vpn',
                                 '--passwd-on-stdin', '--cafile', str(WORK / 'ca.crt'), 'https://10.207.0.1:44443'],
                                input=(password + '\n').encode(), capture_output=True)
        if success:
            assert result.returncode == 0 and b'COOKIE=' in result.stdout
        else:
            assert result.returncode not in (0, 124, 137)
            assert any(word in result.stderr.lower() for word in (b'authentication', b'cookie', b'password', b'401'))

    password1, password2 = secrets.token_hex(24), secrets.token_hex(24)
    (WORK / 'private' / 'vpn-password-1').write_text(password1)
    (WORK / 'private' / 'vpn-password-2').write_text(password2)
    operation, headers = mutation('users', {'name': 't07-vpn', 'sealed_password': sealed(password1)}, 0)
    authenticate(password1, True)
    api(prefix + '/users', {'name': 't07-vpn', 'password': password1, 'reason': 'T07 reject plaintext'},
        headers={'Idempotency-Key': secrets.token_hex(16), 'If-Match': '"revision-0"'}, status=400)
    record('user_create_real_vpn_auth_and_plaintext_rejection', operation_id=operation['id'])
    mutation('groups/t07-staff', {'members': ['t07-vpn']}, 0, method='PUT')
    group = run('sudo', 'awk', '-F:', '$1 == "t07-vpn" {print $2}', '/etc/ocserv/ocpasswd').strip()
    assert 't07-staff' in group.split(',')
    record('group_authoritative_state')
    mutation('users/t07-vpn:rotate-password', {'sealed_password': sealed(password2)}, user_revision())
    authenticate(password1, False)
    authenticate(password2, True)
    record('vpn_password_rotation')
    mutation('users/t07-vpn:disable', {}, user_revision())
    authenticate(password2, False)
    mutation('users/t07-vpn:enable', {}, user_revision())
    authenticate(password2, True)
    record('vpn_disable_restore')
    # The network namespace confines tunnel addresses/routes to the test client.
    client_log = (WORK / 'private' / 'openconnect.log').open('wb')
    vpn = subprocess.Popen(['sudo', 'ip', 'netns', 'exec', 't07-client', 'openconnect', '--non-inter',
                            '--protocol=anyconnect', '--user=t07-vpn', '--passwd-on-stdin',
                            '--cafile', str(WORK / 'ca.crt'), '--script', str(WORK / 'vpn-script'),
                            'https://10.207.0.1:44443'], stdin=subprocess.PIPE, stdout=client_log, stderr=client_log)
    vpn.stdin.write((password2 + '\n').encode())
    vpn.stdin.close()

    def ping():
        result = subprocess.run(['sudo', 'ip', 'netns', 'exec', 't07-client', 'ping', '-c', '2', '-W', '2', '10.208.0.1'],
                                capture_output=True)
        return result.returncode == 0 and vpn.poll() is None

    wait_for('real VPN ICMP', ping, 40)
    def native_session():
        rows = json.loads(run('sudo', 'occtl', '--json', 'show', 'users'))
        return next(row['ID'] for row in rows if row['Username'] == 't07-vpn')

    live_session = native_session()
    ocserv_started = run('systemctl', 'show', 'ocserv', '-p', 'ExecMainStartTimestampMonotonic', '--value')
    session = wait_for('Controller session telemetry', lambda: api(prefix + '/sessions').get('items'))
    assert any(row.get('username') == 't07-vpn' for row in session)
    api(prefix + '/ip-bans')
    record('live_vpn_session_and_bans_read', session_count=len(session))
    identity_before = run('sudo', 'sha256sum', '/var/lib/ocservia-agent/identity/endpoint.key',
                          '/var/lib/ocservia-agent/identity/controller.endpoint')
    started_before = run('systemctl', 'show', 'ocservia-agent', '-p', 'ExecMainStartTimestampMonotonic', '--value')
    control = run(str(ROOT / 'deploy/production/compose.sh'), 'ps', '-q', 'control-plane').strip()
    try:
        run('docker', 'pause', control)
        for _ in range(3):
            assert ping()
            assert native_session() == live_session
        record('controller_pause_preserves_live_vpn', native_session_id=live_session)
    finally:
        run('docker', 'unpause', control)
    relay = os.environ['T07_RELAY_CONTAINER']
    reload_approval = approval('service.reload', 'node', node)

    def reload_count():
        return run('sudo', 'journalctl', '--no-pager', '-u', 'ocserv', '-o', 'cat').count('Reloaded ocserv.service')

    reloads_before = reload_count()
    try:
        run('docker', 'stop', relay)
        for _ in range(3):
            assert ping()
            assert native_session() == live_session
        # Retain the single-relay fixture's non-idempotent reload assertion;
        # never substitute an invisible duplicate enable or force Unknown green.
        key = secrets.token_hex(16)
        body = {'reason': 'T07 isolated validation', 'ttl_seconds': 300}
        for _ in range(3):
            revision = api(prefix)['version']
            headers = {'Idempotency-Key': key, 'If-Match': f'"revision-{revision}"',
                       'X-Approval-ID': reload_approval}
            pending = api(prefix + '/service:reload', body, headers=headers, status=(202, 409))
            if 'id' in pending:
                break
            assert pending.get('type') == 'https://ocservia.dev/problems/stale-revision'
        assert 'id' in pending
        time.sleep(10)
        assert api('operations/' + pending['id'])['state'] != 'succeeded'
        record('single_relay_outage_preserves_live_vpn_and_queues_operation')
    finally:
        run('docker', 'start', relay)
    wait_for('same operation after relay recovery', lambda: completed(pending))
    replay = api(prefix + '/service:reload', body, headers=headers, status=202)
    assert replay['id'] == pending['id'] and replay['command_id'] == pending['command_id']
    operations.append(pending)
    assert identity_before == run('sudo', 'sha256sum', '/var/lib/ocservia-agent/identity/endpoint.key',
                                  '/var/lib/ocservia-agent/identity/controller.endpoint')
    assert started_before == run('systemctl', 'show', 'ocservia-agent', '-p', 'ExecMainStartTimestampMonotonic', '--value')
    assert ping()
    assert native_session() == live_session
    assert ocserv_started == run('systemctl', 'show', 'ocserv', '-p', 'ExecMainStartTimestampMonotonic', '--value')
    time.sleep(3)
    assert reload_count() == reloads_before + 1
    record('single_relay_recovery_identity_and_idempotent_replay', operation_id=pending['id'],
           native_reload_delta=1, native_session_id=live_session)
    cross_checks = []
    for operation in operations:
        command = operation['command_id']
        journal = run('sudo', 'sqlite3', '-readonly', '/var/lib/ocservia-agent/agent.db',
                      "SELECT count(*),state,length(privileged_result_proof)>0 FROM command_journal "
                      f"WHERE hex(command_id)=upper('{command.replace('-', '')}');").strip()
        assert journal == '1|succeeded|1'
        db_state = sql(f"SELECT state FROM operations WHERE id='{operation['id']}';")
        assert db_state == 'succeeded'
        cross_checks.append({'operation_id': operation['id'], 'command_id': command,
                             'database_state': db_state, 'journal_count_state_receipt': journal})
    audit = api('audit/events')
    assert audit.get('items')
    record('api_database_agent_journal_root_receipt_and_audit', cross_checks=cross_checks)
    run('sudo', 'ip', 'netns', 'exec', 't07-client', 'pkill', '-INT', '-x', 'openconnect')
    vpn.wait(timeout=15)
    client_log.close()


if __name__ == '__main__':
    phase = sys.argv[1]
    if phase not in ('local', 'token', 'approve', 'business'):
        raise SystemExit('unknown phase')
    globals()[phase]()
