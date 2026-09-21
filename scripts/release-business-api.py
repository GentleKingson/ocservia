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
import urllib.parse
import uuid

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


def api(path, body=None, *, role='requester', status=200, headers=None, method=None, raw=False):
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
    payload = response.read()
    expected = (status,) if isinstance(status, int) else status
    if response.status not in expected:
        # Never include a response body: successful token/download endpoints
        # and auth errors must not accidentally export credential material.
        raise RuntimeError(f'{request.get_method()} {path}: HTTP {response.status}, expected {status}')
    jar.save(ignore_discard=True)
    return payload if raw else (json.loads(payload) if payload else None)


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
    approved = api(f"approval-requests/{request['id']}")
    assert approved['request_hash'] == request['request_hash']
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


def trust_controller():
    container = run(str(ROOT / 'deploy/production/compose.sh'), 'ps', '-q', 'control-plane').strip()
    pid = run('docker', 'inspect', '--format', '{{.State.Pid}}', container).strip()
    run('docker', 'exec', '--user', '0', '-i', container, 'sh', '-c',
        'cat > /tmp/t07-ca.crt; chmod 444 /tmp/t07-ca.crt', data=(WORK / 'ca.crt').read_bytes())
    run('sudo', 'nsenter', '--target', pid, '--mount', '--root', '--wd=/',
        'mount', '--bind', '/tmp/t07-ca.crt', '/etc/ssl/certs/ca-certificates.crt')


def oidc():
    class NoRedirect(urllib.request.HTTPRedirectHandler):
        def redirect_request(self, *_args):
            return None

    issuer = os.environ['OCSERV_OIDC_ISSUER']

    def login(role, fault=''):
        jar = client(role)[1]
        opener = urllib.request.build_opener(urllib.request.HTTPSHandler(context=CONTEXT),
                                            urllib.request.HTTPCookieProcessor(jar), NoRedirect())

        def request(url, headers=None):
            try:
                return opener.open(urllib.request.Request(url, headers=headers or {}), timeout=15)
            except urllib.error.HTTPError as error:
                return error

        (WORK / 'oidc-fault/mode').write_text(fault)
        start = request(ORIGIN + '/api/v1/auth/login')
        assert start.status == 302
        location = start.headers['Location']
        assert location.startswith(issuer + '/authorize?')
        query = urllib.parse.parse_qs(urllib.parse.urlparse(location).query)
        assert query['code_challenge_method'] == ['S256'] and query['state'][0] and query['nonce'][0]
        secret = (WORK / 'private/oidc-client-secret').read_text().strip()
        authorized = request(location, {'Authorization': 'Basic ' + base64.b64encode(('upgrade:' + secret).encode()).decode()})
        assert authorized.status == 302
        callback = authorized.headers['Location']
        if fault in ('code', 'state'):
            parts = urllib.parse.urlparse(callback)
            values = urllib.parse.parse_qs(parts.query)
            values[fault] = ['invalid-t07-value']
            callback = parts._replace(query=urllib.parse.urlencode(values, doseq=True)).geturl()
        response = request(callback)
        assert response.status == (401 if fault else 302)
        if fault:
            assert not any(c.name == '__Host-ocservia_session' for c in jar)
        else:
            assert any(c.name == '__Host-ocservia_session' and c.secure for c in jar)
            assert request(callback).status == 401  # one-use code/state
        jar.save(ignore_discard=True)

    local_enabled = os.environ['OCSERV_LOCAL_AUTH_ENABLED'] == 'true'
    assert api('auth/methods', role='anonymous') == {'local': local_enabled, 'oidc': True}
    if not local_enabled:
        login('oidc-only')
        api('nodes', role='oidc-only')
        api('auth/login', {'username': 't07-requester', 'password': 'disabled'}, role='disabled-local', status=404)
        record('external_oidc_only_and_local_disabled')
        return
    for fault in ('issuer', 'signature', 'nonce', 'code', 'state'):
        login('oidc-' + fault, fault)
        api('nodes', role='oidc-' + fault, status=401)
        record('oidc_reject_' + fault)
    login('oidc')
    api('nodes', role='oidc', status=403)
    identity = sql("SELECT id FROM identities WHERE issuer='" + issuer + "' AND subject='upgrade-operator';")
    uuid.UUID(identity)
    api('role-bindings', {'identity_id': identity, 'workspace_id': WORKSPACE, 'role': 'Viewer',
                          'resource_type': 'workspace', 'reason': 'T07 OIDC scope'}, status=201)
    api('nodes', role='oidc')
    api('nodes', role='oidc', headers={'X-Workspace-ID': '00000000-0000-7000-8000-000000000072'}, status=403)
    run(str(ROOT / 'deploy/production/compose.sh'), 'restart', 'control-plane')
    trust_controller()
    def ready():
        try:
            return api('readyz', role='anonymous').get('status') == 'ok'
        except (RuntimeError, urllib.error.URLError):
            return False
    wait_for('Controller after restart', ready)
    api('nodes', role='oidc')
    api('auth/logout', {}, role='oidc', status=204)
    api('nodes', role='oidc', status=401)
    record('external_https_oidc_pkce_callback_workspace_restart_logout', identity_id=identity)


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


def cross_check(operation):
    command = operation['command_id'].replace('-', '')
    journal = run('sudo', 'sqlite3', '-readonly', '/var/lib/ocservia-agent/agent.db',
                  "SELECT count(*),state,error_code,length(privileged_result_proof)>0 FROM command_journal "
                  f"WHERE hex(command_id)=upper('{command}');").strip()
    root_effect = run('sudo', 'sqlite3', '-readonly', '/var/lib/ocservia-privd/desired-effects.sqlite3',
                      "SELECT count(*),state,length(response)>0 FROM authorized_effects "
                      f"WHERE hex(command_id)=upper('{command}');").strip()
    snapshot = {'operation_id': operation['id'], 'command_id': operation['command_id'],
                'database_state': sql(f"SELECT state FROM operations WHERE id='{operation['id']}';"),
                'journal_count_state_error_receipt': journal, 'root_count_state_response': root_effect}
    snapshot['operation'] = json.loads(sql("SELECT row_to_json(s) FROM (SELECT id,state,version,created_at,updated_at,completed_at "
                                          f"FROM operations WHERE id='{operation['id']}') s;"))
    snapshot['outbox'] = json.loads(sql("SELECT coalesce(json_agg(s),'[]') FROM (SELECT id,attempts,last_error,published_at,available_at "
                                       f"FROM outbox_events WHERE command_id='{operation['command_id']}') s;"))
    snapshot['attempts'] = json.loads(sql("SELECT coalesce(json_agg(s),'[]') FROM (SELECT attempt_number,state,started_at,finished_at,error_code "
                                         f"FROM command_attempts WHERE command_id='{operation['command_id']}' ORDER BY attempt_number) s;"))
    snapshot['journal'] = json.loads(run('sudo', 'sqlite3', '-readonly', '-json', '/var/lib/ocservia-agent/agent.db',
                                        "SELECT hex(command_id) command_id,hex(payload_sha256) semantic_hash,payload_hash_version,state,error_code,"
                                        "accepted_at,updated_at,hex(privileged_result_proof) receipt_hex FROM command_journal "
                                        f"WHERE hex(command_id)=upper('{command}');") or '[]')
    snapshot['root'] = json.loads(run('sudo', 'sqlite3', '-readonly', '-json', '/var/lib/ocservia-privd/desired-effects.sqlite3',
                                     "SELECT hex(command_id) command_id,hex(payload_sha256) semantic_hash,state,authorization_revision,"
                                     "effect_kind,resource_key,effect_revision,delivery_mode,updated_at,length(response) response_bytes "
                                     f"FROM authorized_effects WHERE hex(command_id)=upper('{command}');") or '[]')
    (EVIDENCE / ('cross-check-' + operation['id'] + '.json')).write_text(json.dumps(snapshot))
    return snapshot


def certificate():
    node = os.environ['T07_NODE']
    node_path = 'nodes/' + node
    wait_for('online before PKI', lambda: api(node_path)['connection_state'] == 'online')
    body = {'expected_version': api(node_path)['version'], 'common_name': 't07-client',
            'dns_names': ['client.example.test'], 'key_bits': 2048, 'reason': 'T07 node-local CSR'}
    headers = {'Idempotency-Key': secrets.token_hex(16)}
    cert = api(node_path + '/certificates', body, headers=headers, status=202)
    assert api(node_path + '/certificates', body, headers=headers, status=202)['id'] == cert['id']
    cert_path = 'certificates/' + cert['id']
    operation_ids = [cert['operation_id']]
    wait_for('root CSR', lambda: api(cert_path)['state'] == 'csr_ready')
    key_path = '/var/lib/ocservia-privd/certificates/' + cert['id'] + '.key.pem'
    key_stat = run('sudo', 'stat', '-c', '%u:%g:%a:%h', key_path).strip()
    assert key_stat.split(':')[0] == '0' and key_stat.split(':')[2:] == ['600', '1']
    # Compare only a public-key digest; never export the unwrapped node key.
    public_before = run('sudo', 'openssl', 'pkey', '-in', key_path, '-pubout')
    run('sudo', 'systemctl', 'restart', 'ocservia-privd', 'ocservia-agent')
    wait_for('node after CSR restart', lambda: api(node_path)['connection_state'] == 'online')
    assert public_before == run('sudo', 'openssl', 'pkey', '-in', key_path, '-pubout')
    issue_approval = approval('certificate.issue', 'certificate', cert['id'])
    cert = api(cert_path + ':issue', {'approval_id': issue_approval, 'reason': 'T07 signed CSR'})
    assert cert['state'] == 'issued'
    reason = 'T07 one-use P12 export'
    artifact = str(uuid.UUID(int=(int(time.time() * 1000) << 80) | (7 << 76) |
                            (secrets.randbits(12) << 64) | (2 << 62) | secrets.randbits(62)))
    export_approval = approval('certificate.private_key.export', 'certificate', cert['id'],
                              {'certificate': {'expected_version': cert['version'], 'purpose': 'certificate_p12',
                                               'artifact_request_id': artifact, 'reason': reason}})
    grant = api(cert_path + ':p12', {'expected_version': api(node_path)['version'],
                                   'certificate_version': cert['version'], 'approval_id': export_approval,
                                   'reason': reason}, headers={'Idempotency-Key': secrets.token_hex(16)}, status=202)
    operation_ids.append(grant['operation']['id'])
    # Credentials stay in the private redaction input, never in the artifact.
    (WORK / 'private/p12-grant').write_text(json.dumps(grant))
    (WORK / 'private/p12-password').write_text(grant['password'])
    (WORK / 'private/p12-token').write_text(grant['download_token'])
    wait_for('P12 root export', lambda: api('operations/' + operation_ids[-1])['state'] == 'succeeded')
    run('sudo', 'systemctl', 'restart', 'ocservia-privd', 'ocservia-agent')
    wait_for('node after P12 restart', lambda: api(node_path)['connection_state'] == 'online')
    download_headers = {'X-Artifact-Token': grant['download_token']}
    blob = api('artifacts/' + grant['artifact_id'], headers=download_headers, raw=True)
    (WORK / 'private/download.p12').write_bytes(blob)
    run('openssl', 'pkcs12', '-in', str(WORK / 'private/download.p12'),
        '-passin', 'file:' + str(WORK / 'private/p12-password'), '-noout')
    api('artifacts/' + grant['artifact_id'], headers=download_headers, status=403, raw=True)
    reason = 'T07 revoke issued certificate'
    revoke_approval = approval('certificate.revoke', 'certificate', cert['id'],
                              {'certificate': {'expected_version': cert['version'], 'reason': reason}})
    revoked = api(cert_path + ':revoke', {'expected_version': api(node_path)['version'],
                                        'certificate_version': cert['version'], 'approval_id': revoke_approval,
                                        'reason': reason}, headers={'Idempotency-Key': secrets.token_hex(16)}, status=202)
    wait_for('certificate revoked', lambda: api(cert_path)['state'] == 'revoked')
    assert (WORK / 'signer-revoked').read_text() == cert['id']
    assert run('sudo', 'test', '!', '-e', key_path) == ''
    # Revoke returns its operation, unlike the CSR certificate resource.
    operation_ids.append(revoked['id'])
    for operation_id in operation_ids:
        snapshot = cross_check(api('operations/' + operation_id))
        assert snapshot['database_state'] == 'succeeded'
        assert snapshot['journal_count_state_error_receipt'] == '1|succeeded||1'
        assert snapshot['root_count_state_response'] == '1|applied|1'
    record('certificate_csr_issue_p12_one_use_revoke_restart', certificate_id=cert['id'],
           operation_ids=operation_ids, node_key_stat=key_stat)


def browser_prepare():
    (WORK / 'browser-approval').write_text(approval('service.reload', 'node', os.environ['T07_NODE']))


def browser_verify():
    for entry in json.loads((EVIDENCE / 'browser-checkpoints.json').read_text()):
        operation_id = entry.get('operation_id') or entry.get('operation', {}).get('id')
        if not operation_id:
            continue
        operation = wait_for('browser operation', lambda: (value if (value := api('operations/' + operation_id))['state'] == 'succeeded' else None))
        snapshot = cross_check(operation)
        assert snapshot['database_state'] == 'succeeded'
        assert snapshot['journal_count_state_error_receipt'] == '1|succeeded||1'
        assert snapshot['root_count_state_response'] == '1|applied|1'
    record('browser_operations_durable_cross_check')


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
    privd_pid = run('systemctl', 'show', 'ocservia-privd', '-p', 'MainPID', '--value').strip()
    process = dict(line.split(':', 1) for line in run('sudo', 'cat', f'/proc/{int(privd_pid)}/status').splitlines())
    agent_group = run('id', '-g', 'ocserv-agent').strip()
    assert process['Uid'].split() == ['0'] * 4
    assert process['Gid'].split() == [agent_group] * 4
    assert set(process['Groups'].split()) == {'0', agent_group}
    assert int(process['CapEff'].strip(), 16) == 2  # CAP_DAC_OVERRIDE only.
    agent_process = dict(line.split(':', 1) for line in run('sudo', 'cat', f'/proc/{int(pid)}/status').splitlines())
    assert '0' not in agent_process['Uid'].split() + agent_process['Groups'].split()
    assert int(agent_process['CapEff'].strip(), 16) == 0
    socket_stat = run('sudo', 'stat', '-c', '%u:%g:%a', '/run/ocserv-platform/privd.sock').strip()
    assert socket_stat == '0:' + agent_group + ':660'
    record('native_privd_permissions', privd={key: process[key].split() for key in ('Uid', 'Gid', 'Groups', 'CapEff')},
           agent={key: agent_process[key].split() for key in ('Uid', 'Gid', 'Groups', 'CapEff')}, socket=socket_stat)
    operations = []

    def completed(operation):
        result = api('operations/' + operation['id'])
        (EVIDENCE / ('operation-' + operation['id'] + '.json')).write_text(json.dumps(result))
        if result['state'] in ('failed', 'expired', 'cancelled'):
            raise RuntimeError(f"operation {operation['id']} reached {result['state']}")
        return result if result['state'] == 'succeeded' else None

    def verify_completed_operations():
        for operation in operations:
            snapshot = cross_check(operation)
            assert snapshot['database_state'] == 'succeeded'
            assert snapshot['journal_count_state_error_receipt'] == '1|succeeded||1'
            assert snapshot['root_count_state_response'] == '1|applied|1'
        record('api_database_agent_journal_root_receipt', operation_ids=[op['id'] for op in operations])

    def mutation(path, body, revision, method='POST', key=None, extra_headers=None):
        headers = {'Idempotency-Key': key or secrets.token_hex(16), 'If-Match': f'"revision-{revision}"'}
        headers.update(extra_headers or {})
        operation = api(prefix + '/' + path, {**body, 'reason': 'T07 isolated validation'},
                        status=202, headers=headers, method=method)
        try:
            wait_for('operation ' + operation['id'], lambda: completed(operation))
        finally:
            command = operation['command_id'].replace('-', '')
            journal = run('sudo', 'sqlite3', '-readonly', '/var/lib/ocservia-agent/agent.db',
                          "SELECT state,error_code,length(privileged_result_proof)>0 FROM command_journal "
                          f"WHERE hex(command_id)=upper('{command}');").strip()
            (EVIDENCE / ('journal-' + operation['id'] + '.txt')).write_text(journal + '\n')
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
    password_stat = run('sudo', 'stat', '-c', '%u:%g:%a:%h', '/etc/ocserv/ocpasswd').strip()
    assert password_stat == '0:0:600:1'
    record('password_file_ownership', stat=password_stat)
    verify_completed_operations()
    audit = api('audit/events?page_size=200')['items']
    assert audit
    audit_ids = ','.join("'" + item['id'] + "'" for item in audit)
    assert int(sql(f"SELECT count(*) FROM audit_events WHERE workspace_id='{WORKSPACE}' AND id IN ({audit_ids});")) == len(audit)
    record('audit_api_database_readback', event_ids=[item['id'] for item in audit])
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
    (EVIDENCE / 'identity-before-fault.json').write_text(json.dumps({
        'agent_endpoint': os.environ['T07_ENDPOINT'], 'controller_endpoint': os.environ['OCSERV_CONTROLLER_ENDPOINT_ID'],
        'identity_file_digests': identity_before, 'agent_start_monotonic': started_before.strip(),
        'ocserv_start_monotonic': ocserv_started.strip(), 'native_session_id': live_session}))
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
    record('reload_before_fault', native_reload_count=reloads_before)
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
    recovery_error = None
    try:
        wait_for('same operation after relay recovery', lambda: completed(pending))
    except RuntimeError as error:
        recovery_error = error
    finally:
        # Query-only diagnostics also survive Unknown. Never retry a mutation
        # or invent a receipt to make the strict recovery assertion green.
        recovery_snapshot = cross_check(pending)
        recovery_snapshot['node'] = api(prefix)
        recovery_snapshot['owner'] = json.loads(sql("SELECT row_to_json(s) FROM (SELECT encode(connection_id,'hex') connection_id,"
                                                   "owner_epoch,lease_until,updated_at FROM connection_owner_fencing "
                                                   f"WHERE node_id=decode('{node.replace('-', '')}','hex')) s;"))
        recovery_snapshot['native_reload_count'] = reload_count()
        (EVIDENCE / 'recovery-final.json').write_text(json.dumps(recovery_snapshot))
    assert identity_before == run('sudo', 'sha256sum', '/var/lib/ocservia-agent/identity/endpoint.key',
                                  '/var/lib/ocservia-agent/identity/controller.endpoint')
    assert started_before == run('systemctl', 'show', 'ocservia-agent', '-p', 'ExecMainStartTimestampMonotonic', '--value')
    assert ping()
    assert native_session() == live_session
    assert ocserv_started == run('systemctl', 'show', 'ocserv', '-p', 'ExecMainStartTimestampMonotonic', '--value')
    assert api(prefix)['connection_state'] == 'online'
    record('single_relay_restored_same_identity_and_live_vpn', native_session_id=live_session,
           operation_state=recovery_snapshot['database_state'], native_reload_delta=reload_count() - reloads_before)
    if recovery_error is not None:
        raise recovery_error
    replay = api(prefix + '/service:reload', body, headers=headers, status=202)
    assert replay['id'] == pending['id'] and replay['command_id'] == pending['command_id']
    operations.append(pending)
    time.sleep(3)
    assert reload_count() == reloads_before + 1
    record('single_relay_recovery_identity_and_idempotent_replay', operation_id=pending['id'],
           native_reload_delta=1, native_session_id=live_session)
    verify_completed_operations()
    run('sudo', 'ip', 'netns', 'exec', 't07-client', 'pkill', '-INT', '-x', 'openconnect')
    vpn.wait(timeout=15)
    client_log.close()


if __name__ == '__main__':
    phase = sys.argv[1]
    if phase not in ('local', 'oidc', 'trust_controller', 'token', 'approve', 'certificate',
                     'browser_prepare', 'browser_verify', 'business'):
        raise SystemExit('unknown phase')
    globals()[phase]()
