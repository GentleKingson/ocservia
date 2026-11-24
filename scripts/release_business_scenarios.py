#!/usr/bin/env python3
"""Release Check business smoke acceptance scenarios."""
import base64
import hashlib
import http.client
import json
import os
from pathlib import Path
import secrets
import socket
import ssl
import subprocess
import sys
import time
import urllib.error

from release_business_runtime import ROOT, api, client, environment, record, run, sql, timed, wait_for

COMPOSE = str(ROOT / 'deploy/production/compose.sh')
OCSERV_FILES = ('/etc/ocserv/ocserv.conf', '/etc/ocserv/ocpasswd')


def _signer_state():
    return environment().work / 'production-signer'


def _signer_admin(*args):
    state = _signer_state()
    return run('docker', 'run', '--rm', '--read-only', '--cap-drop=ALL',
               '--security-opt=no-new-privileges:true',
               '-v', f'{state}/secrets:/run/secrets:ro',
               '-v', f'{state}/input:/input:ro',
               '-v', f'{state}/data:/var/lib/ocservia-signer',
               os.environ['OCSERVIA_BUSINESS_TEST_SIGNER_IMAGE'], *args)


def _signer_health():
    container = os.environ['OCSERVIA_BUSINESS_TEST_SIGNER_CONTAINER']
    for _ in range(30):
        result = subprocess.run(['docker', 'exec', container, '/ocserv-signer', 'health',
                                 '--url', 'https://signer:9443/healthz'],
                                stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        if result.returncode == 0:
            return
        time.sleep(1)
    raise RuntimeError('production Signer did not become healthy')


def signer_prepare():
    work = environment().work
    private = work / 'private'
    state = _signer_state()
    state.mkdir(mode=0o700)
    for folder in ('secrets', 'input', 'data'):
        run('sudo', 'install', '-d', '-o', '65532', '-g', '65532', '-m', '700', str(state / folder))
    run('openssl', 'req', '-x509', '-newkey', 'rsa:2048', '-nodes', '-days', '7',
        '-subj', '/CN=signer-offline-root', '-addext', 'basicConstraints=critical,CA:TRUE,pathlen:1',
        '-addext', 'keyUsage=critical,keyCertSign,cRLSign',
        '-keyout', str(private / 'signer-root.key'), '-out', str(work / 'signer-root.pem'))
    run('openssl', 'req', '-new', '-newkey', 'rsa:2048', '-nodes', '-subj', '/CN=signer-online-issuer',
        '-keyout', str(private / 'issuer-key.pem'), '-out', str(work / 'issuer.csr'))
    (work / 'issuer.ext').write_text('basicConstraints=critical,CA:TRUE,pathlen:0\n'
                                     'keyUsage=critical,keyCertSign,cRLSign\n'
                                     'subjectKeyIdentifier=hash\nauthorityKeyIdentifier=keyid:always\n')
    run('openssl', 'x509', '-req', '-in', str(work / 'issuer.csr'), '-days', '3',
        '-CA', str(work / 'signer-root.pem'), '-CAkey', str(private / 'signer-root.key'),
        '-set_serial', '2', '-extfile', str(work / 'issuer.ext'), '-out', str(work / 'issuer.pem'))
    (work / 'issuer-chain.pem').write_bytes((work / 'issuer.pem').read_bytes() +
                                            (work / 'signer-root.pem').read_bytes())
    run('openssl', 'req', '-new', '-newkey', 'rsa:2048', '-nodes', '-subj', '/CN=signer',
        '-keyout', str(private / 'signer-tls.key'), '-out', str(work / 'signer-tls.csr'))
    (work / 'signer-tls.ext').write_text('subjectAltName=DNS:signer\nextendedKeyUsage=serverAuth\n')
    run('openssl', 'x509', '-req', '-days', '1', '-in', str(work / 'signer-tls.csr'),
        '-CA', str(work / 'ca.crt'), '-CAkey', str(private / 'ca.key'), '-CAcreateserial',
        '-extfile', str(work / 'signer-tls.ext'), '-out', str(work / 'signer-tls.crt'))
    sources = {
        'issuer-chain.pem': work / 'issuer-chain.pem',
        'issuer-key.pem': private / 'issuer-key.pem',
        'tls-cert.pem': work / 'signer-tls.crt',
        'tls-key.pem': private / 'signer-tls.key',
        'api-token': private / 'certificate-signer-token',
        'tls-ca.pem': work / 'ca.crt',
    }
    for name, source in sources.items():
        mode = '444' if name == 'tls-ca.pem' else '400'
        run('sudo', 'install', '-o', '65532', '-g', '65532', '-m', mode, str(source), str(state / 'secrets' / name))


def signer_import():
    work = environment().work
    private = work / 'private'
    state = _signer_state()
    node = os.environ['OCSERVIA_BUSINESS_TEST_NODE_ID']
    endpoint = os.environ['OCSERVIA_BUSINESS_TEST_AGENT_ENDPOINT_ID']
    container = os.environ['OCSERVIA_BUSINESS_TEST_SIGNER_CONTAINER']
    dsn = private / 'export-dsn'
    run('sudo', 'install', '-o', '65534', '-g', '65532', '-m', '400', str(work / 'secrets/database-app-url'), str(dsn))
    try:
        approved = subprocess.run([
            'docker', 'run', '--rm', '--read-only', '--cap-drop=ALL',
            '--security-opt=no-new-privileges:true', '--network', 'ocservia-production_database',
            '-v', f'{dsn}:/run/export-dsn:ro', '--entrypoint', '/usr/local/bin/ocserv-sealing-export',
            os.environ['OCSERV_CONTROL_IMAGE'], '--database-url-file', '/run/export-dsn', '--bundled-postgres',
            '--workspace', os.environ['OCSERVIA_BUSINESS_TEST_WORKSPACE_ID'], '--node', node, '--endpoint', endpoint,
        ], capture_output=True, check=True).stdout
    finally:
        run('sudo', 'rm', '--', str(dsn))
    public = subprocess.run([
        'sudo', 'python3', str(ROOT / 'scripts/export-node-sealing-keys.py'), '--node', node,
        '--endpoint', endpoint, '--user-key', '/etc/ocservia-agent/user-password-seal-private.pem',
        '--user-key-id', 'business-test-user-seal', '--p12-key', '/etc/ocservia-agent/p12-password-seal-private.pem',
        '--p12-key-id', 'business-test-p12-seal',
    ], capture_output=True, check=True).stdout
    for name, data in (('approved.json', approved), ('public.json', public)):
        source = work / name
        source.write_bytes(data)
        run('sudo', 'install', '-o', '65532', '-g', '65532', '-m', '400', str(source), str(state / 'input' / name))
    run('docker', 'stop', container)
    try:
        _signer_admin('import', '--approved', '/input/approved.json', '--public', '/input/public.json',
                      '--workspace', os.environ['OCSERVIA_BUSINESS_TEST_WORKSPACE_ID'],
                      '--node', node, '--endpoint', endpoint)
    finally:
        run('docker', 'start', container)
    _signer_health()


def signer_seal(payload):
    work = environment().work
    private = work / 'private'
    container = os.environ['OCSERVIA_BUSINESS_TEST_SIGNER_CONTAINER']
    (private / 'signer-request').write_bytes(payload)
    token = (private / 'certificate-signer-token').read_text().strip()
    (private / 'signer-curl.conf').write_text('header = "Authorization: Bearer ' + token + '"\n')
    pid = run('docker', 'inspect', '--format', '{{.State.Pid}}', container).strip()
    result = json.loads(run(
        'sudo', 'nsenter', '--target', pid, '--net', 'curl', '--fail', '--silent', '--show-error',
        '--cacert', str(work / 'ca.crt'), '--config', str(private / 'signer-curl.conf'),
        '-H', 'Content-Type: application/octet-stream',
        '-H', 'X-Ocservia-Node-ID: ' + os.environ['OCSERVIA_BUSINESS_TEST_NODE_ID'],
        '-H', 'X-Ocservia-Seal-Purpose: user_password',
        '--data-binary', '@' + str(private / 'signer-request'),
        '--resolve', 'signer:9443:127.0.0.1', 'https://signer:9443/sign/seal',
    ))
    return {'version': result['version'], 'purpose': result['purpose'],
            'key_id': result['key_id'], 'ciphertext': result['sealed']}


def integrated_entry():
    env = environment()
    work = env.work
    config = json.loads(run(COMPOSE, 'config', '--format', 'json'))
    ports = sorted((name, str(port['published']), port['protocol'])
                   for name, service in config['services'].items() for port in service.get('ports', []))
    assert ports == [('edge', '443', 'tcp'), ('relay', '7842', 'udp')], ports
    manifest = json.loads((Path(os.environ['DEPLOYMENT_CONFIG_DIR']) /
                           f"controller-release-{os.environ['CONTROLLER_ARCH']}.json").read_text())
    allowed = set(manifest['images'].values())
    for service in config['services'].values():
        assert 'build' not in service and service['image'] in allowed

    def tls_get(name, path, host=None):
        with socket.create_connection((name, 443), timeout=10) as sock:
            with env.tls_context.wrap_socket(sock, server_hostname=name) as tls:
                fingerprint = hashlib.sha256(tls.getpeercert(binary_form=True)).hexdigest()
                tls.sendall(f'GET {path} HTTP/1.1\r\nHost: {host or name}\r\nConnection: close\r\n\r\n'.encode())
                response = http.client.HTTPResponse(tls)
                response.begin()
                return response.status, response.read(), fingerprint

    control_name, relay_name = os.environ['OCSERV_PUBLIC_HOST'], os.environ['OCSERV_RELAY_PUBLIC_HOST']
    status, body, control_cert = tls_get(control_name, '/api/v1/version')
    assert status == 200 and json.loads(body)['commit'] == os.environ['SOURCE_COMMIT']
    status, _, relay_cert = tls_get(relay_name, '/healthz')
    assert status == 200 and control_cert != relay_cert
    assert tls_get(control_name, '/api/v1/version', 'wrong.invalid')[0] == 421
    for server_name in (None, 'unknown.invalid'):
        unverified = ssl.SSLContext(ssl.PROTOCOL_TLS_CLIENT)
        unverified.minimum_version = ssl.TLSVersion.TLSv1_2
        unverified.check_hostname = False
        unverified.verify_mode = ssl.CERT_NONE
        try:
            with socket.create_connection((control_name, 443), timeout=8) as sock:
                with unverified.wrap_socket(sock, server_hostname=server_name):
                    raise AssertionError('unknown or missing SNI accepted')
        except (ssl.SSLError, ConnectionResetError):
            pass

    signer = json.loads(run('docker', 'inspect', run(COMPOSE, 'ps', '-q', 'signer').strip()))[0]
    control = json.loads(run('docker', 'inspect', run(COMPOSE, 'ps', '-q', 'control-plane').strip()))[0]
    assert not signer['HostConfig']['PortBindings']
    address = signer['NetworkSettings']['Networks']['ocservia-production_signer']['IPAddress']
    network = json.loads(run('docker', 'network', 'inspect', 'ocservia-production_signer'))[0]
    assert network['Internal']
    probe = ['nsenter', '--target', str(control['State']['Pid']), '--net', 'curl', '--fail',
             '--silent', '--show-error', '--max-time', '8', '--noproxy', '*']
    subprocess.run([*probe, '--cacert', str(work / 'ca.crt'), '--resolve', f'signer:9443:{address}',
                    'https://signer:9443/healthz'], check=True, capture_output=True)
    wrong_ca = work / 'untrusted-ca.crt'
    (work / 'empty-trust').mkdir(exist_ok=True)
    if not wrong_ca.exists():
        run('openssl', 'req', '-x509', '-newkey', 'rsa:2048', '-nodes', '-days', '1',
            '-subj', '/CN=untrusted-acceptance-ca', '-keyout', str(work / 'untrusted-ca.key'), '-out', str(wrong_ca))
    assert subprocess.run([*probe, '--cacert', str(wrong_ca), '--capath', str(work / 'empty-trust'),
                           '--resolve', f'signer:9443:{address}', 'https://signer:9443/healthz'],
                          capture_output=True, check=False).returncode == 60
    assert subprocess.run([*probe, '--cacert', str(work / 'ca.crt'), '--resolve', f'wrong.invalid:9443:{address}',
                           'https://wrong.invalid:9443/healthz'],
                          capture_output=True, check=False).returncode == 60
    record('entry_tls_and_topology', ports=ports, controller_certificate_sha256=control_cert,
           relay_certificate_sha256=relay_cert)


def wait_for_relay_outage(node):
    # Stopping Relay does not immediately invalidate a buffered QUIC session.
    wait_for('single Relay ownership invalidation', lambda: sql(
        "SELECT NOT EXISTS (SELECT 1 FROM connection_owner_fencing "
        f"WHERE node_id=decode('{node.replace('-', '')}','hex') "
        "AND active);") == 't')
    record('single_relay_ownership_invalidated')


def local():
    api('auth/login', {'username': 'business-test-requester',
                       'password': (environment().work / 'private/requester-password').read_text().strip()},
        status=204)
    cookies = list(client('requester')[1])
    assert any(c.name == '__Host-ocservia_session' and c.secure and c.path == '/' for c in cookies)
    api('auth/login', {'username': 'business-test-requester', 'password': 'incorrect-business-test-password'}, role='wrong', status=401)
    api('nodes', role='anonymous', status=401)
    api('nodes', headers={'X-Workspace-ID': '00000000-0000-7000-8000-000000000072'}, status=403)
    api('nodes')
    ids = sql("SELECT string_agg(id::text, ',' ORDER BY id) FROM identities WHERE issuer='local';").split(',')
    assert len(ids) == 1
    record('local_auth_and_workspace_isolation', identity_ids=ids)


def transport_ready():
    container = run(COMPOSE, 'ps', '-q', 'transportd').strip()
    assert container

    def healthy():
        state = json.loads(run('docker', 'inspect', '--format', '{{json .State}}', container))
        if state['Status'] != 'running' or state['Health']['Status'] == 'unhealthy':
            raise RuntimeError('transport failed during ordered authentication startup: ' + json.dumps(state))
        return state['Health']['Status'] == 'healthy'

    wait_for('transport health after Controller readiness', healthy)
    record('auth_transport_ready', container_id=container)


def bootstrap_token():
    # Token-only installation issues an EndpointID-unbound oit1_ token.
    result = api('node-bootstrap-tokens', {'installation': True, 'workspace_id': environment().workspace, 'environment': 'production',
                                           'expected_node_name': 'business-test-agent',
                                           'reason': 'Business test: isolated native bootstrap'}, status=201)
    assert result['token'].startswith('oit1_')
    # The private copy is redaction input only; the probe removes the staged token file.
    (environment().work / 'private' / 'bootstrap-token').write_text(result['token'])
    (environment().work / 'bootstrap-token-id').write_text(result['id'])
    source = environment().work / 'bootstrap' / 'join-token'
    source.parent.mkdir(mode=0o700, exist_ok=True)
    with os.fdopen(os.open(source, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600), 'w') as output:
        output.write(result['token'])
    assert sql(f"SELECT expected_endpoint_id IS NULL AND consumed_at IS NULL FROM node_bootstrap_tokens WHERE id='{result['id']}';") == 't'
    record('bootstrap_token_created_unbound', token_id=result['id'])


def bootstrap_verify():
    node, endpoint = os.environ['OCSERVIA_BUSINESS_TEST_NODE_ID'], os.environ['OCSERVIA_BUSINESS_TEST_AGENT_ENDPOINT_ID']
    token_id = (environment().work / 'bootstrap-token-id').read_text()
    root_token = (environment().work / 'private' / 'bootstrap-token').read_text().strip()
    child_digest = hashlib.sha256(b'ocservia/install-enrollment/v1\x00' + root_token.encode()).digest()
    child_token = 'obt2_' + base64.urlsafe_b64encode(child_digest).decode().rstrip('=')
    digest = hashlib.sha256(child_token.encode()).hexdigest()
    row = json.loads(sql("SELECT row_to_json(s) FROM (SELECT encode(token_hash,'hex') token_hash,"
                         "encode(bound_endpoint_id,'hex') bound_endpoint,expected_endpoint_id IS NULL unbound_at_creation,"
                         "consumed_node_id,consumed_at IS NOT NULL consumed "
                         f"FROM node_bootstrap_tokens WHERE id='{token_id}') s;"))
    assert row == {'token_hash': digest, 'bound_endpoint': endpoint, 'unbound_at_creation': True,
                   'consumed_node_id': node, 'consumed': True}, 'bootstrap token binding'
    assert sql(f"SELECT status FROM nodes WHERE id='{node}';") == 'active'
    assert sql(f"SELECT encode(endpoint_id,'hex') FROM node_endpoint_keys WHERE node_id='{node}';") == endpoint
    assert sql(f"SELECT count(*) FROM node_privd_attestation_keys WHERE node_id='{node}' AND state='active';") == '1'
    record('bootstrap_token_consumed_and_bound_active', token_id=token_id, node_id=node)


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
    (environment().evidence / ('cross-check-' + operation['id'] + '.json')).write_text(json.dumps(snapshot))
    return snapshot


def browser_prepare():
    api('nodes/' + os.environ['OCSERVIA_BUSINESS_TEST_NODE_ID'])


def config_prepare():
    node = os.environ['OCSERVIA_BUSINESS_TEST_NODE_ID']
    certificate = subprocess.run(['openssl', 'x509', '-in', '/etc/ocserv/business-test.crt', '-outform', 'DER'],
                                 capture_output=True, check=True).stdout
    public_pem = subprocess.run(['openssl', 'x509', '-in', '/etc/ocserv/business-test.crt', '-pubkey', '-noout'],
                                capture_output=True, check=True).stdout
    public_der = subprocess.run(['openssl', 'pkey', '-pubin', '-outform', 'DER'], input=public_pem,
                                capture_output=True, check=True).stdout
    binding = node + '/' + hashlib.sha256(certificate).hexdigest() + '/' + hashlib.sha256(public_der).hexdigest() + '/none'
    reference = api('secret-provider-refs', {'provider': 'node-local-tls-v1', 'key_path': binding,
                                             'version': 'v1', 'reason': 'Business test: operator-provisioned TLS'}, status=201)
    output = json.loads(run('sudo', 'bash', str(ROOT / 'deploy/managed-node/provision-config-tls.sh'),
                            node, reference['id'], 'v1', '/etc/ocserv/business-test.crt', '/etc/ocserv/business-test.key'))
    assert output['key'] == binding and output['secret_ref_id'] == reference['id']
    bundle = '/etc/ocservia-agent/config-tls/' + reference['id'] + '/v1'
    for name in ('server-cert.pem', 'server-key.pem', 'manifest.json'):
        assert run('sudo', 'stat', '-c', '%u:%g:%a:%h', bundle + '/' + name).strip() == '0:0:400:1'
    duplicate = subprocess.run(['sudo', 'bash', str(ROOT / 'deploy/managed-node/provision-config-tls.sh'),
                                node, reference['id'], 'v1', '/etc/ocserv/business-test.crt', '/etc/ocserv/business-test.key'], capture_output=True)
    assert duplicate.returncode != 0
    (environment().work / 'config-reference.json').write_text(json.dumps(reference))
    record('complete_config_tls_provisioned', reference_id=reference['id'], version='v1', file_identity='0:0:400:1', overwrite_rejected=True)


def smoke_user():
    node = os.environ['OCSERVIA_BUSINESS_TEST_NODE_ID']
    password = secrets.token_hex(24)
    (environment().work / 'private/smoke-vpn-password').write_text(password)
    if os.environ.get('PRODUCTION_SIGNER_ACCEPTANCE') == 'true':
        sealed = signer_seal(password.encode())
        record('production_signer_password_sealed')
    else:
        encrypted = subprocess.run(['openssl', 'pkeyutl', '-encrypt', '-pubin', '-inkey', str(environment().work / 'user.pub.pem'),
                                    '-pkeyopt', 'rsa_padding_mode:oaep', '-pkeyopt', 'rsa_oaep_md:sha256'],
                                   input=password.encode(), capture_output=True, check=True).stdout
        sealed = {'version': 1, 'purpose': 'user_password', 'key_id': 'business-test-user-seal',
                  'ciphertext': base64.b64encode(encrypted).decode()}
    operation = api(f'nodes/{node}/users', {'name': 'business-test-smoke-user', 'sealed_password': sealed,
                                            'reason': 'Business test: Release Business Smoke'},
                    headers={'Idempotency-Key': secrets.token_hex(16), 'If-Match': '"revision-0"'}, status=202)

    def user_ready():
        state = api('operations/' + operation['id'])['state']
        if state in ('failed', 'expired', 'cancelled'):
            raise RuntimeError('smoke VPN user ' + state)
        return state == 'succeeded'

    wait_for('smoke VPN user', user_ready)
    record('smoke_vpn_user_ready', operation_id=operation['id'])


def owner():
    node = os.environ['OCSERVIA_BUSINESS_TEST_NODE_ID'].replace('-', '')
    return json.loads(sql("SELECT row_to_json(s) FROM (SELECT encode(connection_id,'hex') connection_id,"
                          "session_epoch FROM connection_owner_fencing "
                          f"WHERE node_id=decode('{node}','hex') AND active) s;") or "null")


def fresh_owner(before):
    current = owner()
    return current if (current and current['session_epoch'] > before['session_epoch'] and
                       current['connection_id'] != before['connection_id'] and
                       api('nodes/' + os.environ['OCSERVIA_BUSINESS_TEST_NODE_ID'])['connection_state'] == 'online') else None


def ready():
    try:
        return api('readyz', role='anonymous').get('status') == 'ok'
    except (RuntimeError, urllib.error.URLError):
        return False


def check_unchanged(*paths):
    for path in map(Path, paths):
        run('sudo', 'cmp', '-s', environment().work / 'unchanged' / path.name, path)


def check_enrolled_identity():
    check_unchanged('/var/lib/ocservia-agent/identity/endpoint.key', '/var/lib/ocservia-agent/identity/controller.endpoint')


def completed(operation):
    result = api('operations/' + operation['id'])
    (environment().evidence / ('operation-' + operation['id'] + '.json')).write_text(json.dumps(result))
    if result['state'] in ('failed', 'expired', 'cancelled'):
        raise RuntimeError(f"operation {operation['id']} reached {result['state']}")
    return result if result['state'] == 'succeeded' else None


def authorized_reload(name):
    node = 'nodes/' + os.environ['OCSERVIA_BUSINESS_TEST_NODE_ID']
    key = secrets.token_hex(16)
    body = {'reason': name, 'ttl_seconds': 300}
    headers = {'Idempotency-Key': key, 'If-Match': f'"revision-{api(node)["version"]}"'}
    operation = api(node + '/service:reload', body, headers=headers, status=202)
    wait_for(name, lambda: completed(operation))
    return operation


def single_relay_argv():
    observed = api('nodes/' + os.environ['OCSERVIA_BUSINESS_TEST_NODE_ID'])
    pid = run('systemctl', 'show', 'ocservia-agent', '-p', 'MainPID', '--value').strip()
    agent_argv = run('sudo', 'cat', f'/proc/{int(pid)}/cmdline').rstrip('\0').split('\0')
    transport_argv = run('docker', 'exec', os.environ['OCSERVIA_BUSINESS_TEST_TRANSPORT_CONTAINER'], 'sh', '-c',
                         'for exe in /proc/[0-9]*/exe; do '
                         'if [ "$(readlink "$exe")" = /usr/local/bin/ocservia-transportd ]; then '
                         'cat "${exe%/exe}/cmdline"; fi; done').rstrip('\0').split('\0')
    for argv in (agent_argv, transport_argv):
        assert [argv[i + 1] for i, arg in enumerate(argv) if arg == '--relay-url'] == [os.environ['OCSERV_RELAY_URL_A']]
        assert argv[argv.index('--relay-mode') + 1] == 'custom'
    assert observed['path']['mode'] == 'relay'


def agent_stack_recovery():
    before = owner()
    # One systemd transaction orders privd before its Requires= dependent Agent.
    run('sudo', 'systemctl', 'restart', 'ocservia-privd', 'ocservia-agent')
    for service in ('ocservia-privd', 'ocservia-agent'):
        wait_for(service + ' active', lambda: run(
            'systemctl', 'show', service, '-p', 'ActiveState', '--value').strip() == 'active')
    wait_for('fresh Agent stack session', lambda: fresh_owner(before))
    check_enrolled_identity()
    record('resilience_agent')


def native_service_pids():
    pids = {}
    for service in ('ocserv', 'ocservia-agent', 'ocservia-privd'):
        pid = int(run('systemctl', 'show', service, '-p', 'MainPID', '--value').strip())
        assert pid > 0, f'{service} MainPID must be positive'
        pids[service] = pid
    return pids


def smoke_relay_recovery(ping, outage_settle_seconds=10):
    single_relay_argv()
    node = os.environ['OCSERVIA_BUSINESS_TEST_NODE_ID']
    prefix = 'nodes/' + node
    before = owner()
    pids = native_service_pids()
    run('sudo', 'cp', '--', *OCSERV_FILES, environment().work / 'unchanged')
    effects = node_side_effects()
    relay = os.environ['OCSERVIA_BUSINESS_TEST_RELAY_CONTAINER']
    body = {'reason': 'Relay outage offline queue recovery', 'ttl_seconds': 300}
    key = secrets.token_hex(16)
    operation = None
    headers = None
    try:
        run('docker', 'stop', relay)
        wait_for_relay_outage(node)
        wait_for('node offline during Relay outage', lambda: api(prefix)['connection_state'] == 'offline')
        for _ in range(3):
            headers = {'Idempotency-Key': key, 'If-Match': f'"revision-{api(prefix)["version"]}"'}
            candidate = api(prefix + '/service:reload', body, headers=headers, status=(202, 409))
            if 'id' in candidate:
                operation = candidate
                break
            assert candidate['type'] == 'https://ocservia.dev/problems/stale-revision'
        assert operation is not None and 'id' in operation
        if outage_settle_seconds > 0:
            time.sleep(outage_settle_seconds)
        command = operation['command_id']
        assert api('operations/' + operation['id'])['state'] == 'queued'
        assert sql(f"SELECT count(*) FROM outbox_events WHERE command_id='{command}' AND published_at IS NULL;") == '1'
        assert sql(f"SELECT count(*) FROM command_attempts WHERE command_id='{command}' AND state='sent';") == '0'
        check_unchanged(*OCSERV_FILES)
        assert node_side_effects() == effects
        assert ping(), 'existing VPN session dropped during Relay outage'
        check_enrolled_identity()
        assert native_service_pids() == pids
    finally:
        run('docker', 'start', relay)
    after = wait_for('fresh Agent session after Relay recovery', lambda: fresh_owner(before), seconds=90)
    settled = wait_for('queued operation after Relay recovery', lambda: completed(operation))
    command = settled['command_id']
    snapshot = cross_check(settled)
    assert snapshot['database_state'] == 'succeeded'
    assert snapshot['journal_count_state_error_receipt'] == '1|succeeded||1'
    assert snapshot['root_count_state_response'] == '1|applied|1'
    assert sql(f"SELECT count(*) FROM outbox_events WHERE command_id='{command}' AND published_at IS NOT NULL;") == '1'
    assert int(sql(f"SELECT count(*) FROM command_attempts WHERE command_id='{command}' AND state='sent';")) >= 1
    assert sql(f"SELECT count(*) FROM audit_events WHERE command_id='{command}';") != '0'
    wait_for('native reload after Relay recovery', lambda: node_side_effects()['reloads'] == effects['reloads'] + 1, seconds=15)
    after_effects = node_side_effects()
    # authorized_effects prunes expired applied rows on write, so total row
    # count can shrink while the exact command row is verified by cross_check.
    assert after_effects['journal'] == effects['journal'] + 1
    assert after_effects['reloads'] == effects['reloads'] + 1
    check_unchanged(*OCSERV_FILES)
    replay = api(prefix + '/service:reload', body, headers=headers, status=202)
    assert replay['id'] == settled['id'] and replay['command_id'] == command
    check_unchanged(*OCSERV_FILES)
    assert node_side_effects() == after_effects
    check_enrolled_identity()
    assert native_service_pids() == pids
    wait_for('VPN after Relay recovery', ping, 40)
    record('resilience_relay',
           operation_id=settled['id'],
           command_id=command,
           before_epoch=before['session_epoch'],
           after_epoch=after['session_epoch'],
           offline_queued_zero_side_effects=True,
           idempotent_replay=True)


def resilience():
    with timed('controller_recovery'):
        before = owner()
        run(COMPOSE, 'restart', 'control-plane', 'transportd')
        wait_for('Controller stack readiness', ready)
        transport_ready()
        wait_for('fresh Controller stack session', lambda: fresh_owner(before))
        authorized_reload('Controller stack recovery')
        record('resilience_controller')
    with timed('agent_recovery'):
        agent_stack_recovery()
    with timed('database_recovery'):
        try:
            run(COMPOSE, 'stop', 'postgres')
            outage = api('readyz', role='anonymous', status=503)
            assert outage['type'] == 'https://ocservia.dev/problems/database-unavailable'
        finally:
            run(COMPOSE, 'start', 'postgres')
        wait_for('database readiness recovered', ready)
        wait_for('Agent online after database recovery', lambda: api(
            'nodes/' + os.environ['OCSERVIA_BUSINESS_TEST_NODE_ID'])['connection_state'] == 'online')
        authorized_reload('database reconnect business')
        record('resilience_database', backend='postgres')
    with timed('relay_recovery'):
        vpn_smoke('relay_recovery', relay_recovery=True)


def assert_browser_csp(entries, evidence):
    csp = next(entry for entry in entries if entry['name'] == 'browser_csp')
    assert csp['flow_violations'] == 0 and csp['flow_console_reports'] == 0, 'browser CSP violation: see ' + evidence
    assert sorted(csp['control']) == ['img-src/enforce', 'script-src-elem/enforce'], \
        'browser CSP control not enforced and observed exactly'


def browser_verify():
    entries = json.loads((environment().evidence / 'browser-checkpoints.json').read_text())
    required = {'browser_user_mutation', 'browser_direct_admin_reload',
                'browser_idempotency_rejects_changed_intent',
                'browser_complete_config_apply', 'browser_login_workspace_forbidden_refresh', 'browser_csp'}
    for name in required:
        assert sum(entry['name'] == name for entry in entries) == 1, 'missing or duplicate browser checkpoint: ' + name
    assert next(entry for entry in entries if entry['name'] == 'browser_login_workspace_forbidden_refresh')['status'] == 'PASS'
    assert_browser_csp(entries, 'browser-csp.json')
    operation_checks = {'browser_user_mutation', 'browser_direct_admin_reload', 'browser_complete_config_apply'}
    for entry in entries:
        operation_id = entry.get('operation_id') or entry.get('operation', {}).get('id')
        if entry['name'] in operation_checks:
            assert operation_id, 'missing browser operation: ' + entry['name']
        if not operation_id:
            continue
        operation = wait_for('browser operation', lambda: (value if (value := api('operations/' + operation_id))['state'] == 'succeeded' else None))
        snapshot = cross_check(operation)
        assert snapshot['database_state'] == 'succeeded'
        assert snapshot['journal_count_state_error_receipt'] == '1|succeeded||1'
        assert snapshot['root_count_state_response'] == '1|applied|1'
    applied = next(entry for entry in entries if entry['name'] == 'browser_complete_config_apply')
    operation_id = applied['operation']['id']
    assert api('operations/' + operation_id)['config_apply_state'] == 'succeeded'
    assert api('config-plans/' + applied['plan_id'])['materialized_hash'] == applied['materialized_hash']
    assert api(f"nodes/{os.environ['OCSERVIA_BUSINESS_TEST_NODE_ID']}")['config_revision'] == 1
    assert run('sudo', 'sha256sum', '/etc/ocserv/ocserv.conf').split()[0] == applied['materialized_hash']
    record('browser_operations_durable_cross_check')


def node_side_effects():
    return {
        'journal': int(run('sudo', 'sqlite3', '-readonly', '/var/lib/ocservia-agent/agent.db', 'SELECT count(*) FROM command_journal;').strip()),
        'privd': int(run('sudo', 'sqlite3', '-readonly', '/var/lib/ocservia-privd/desired-effects.sqlite3',
                         'SELECT count(*) FROM authorized_effects;').strip()),
        'reloads': run('sudo', 'journalctl', '--no-pager', '-u', 'ocserv', '-o', 'cat').count('Reloaded ocserv.service'),
    }


def revocation():
    # Terminal stage: the node loses its Controller authority for good.
    node = os.environ['OCSERVIA_BUSINESS_TEST_NODE_ID']
    prefix = 'nodes/' + node
    wait_for('online before revocation', lambda: api(prefix)['connection_state'] == 'online')
    before = owner()
    agent_pid = run('systemctl', 'show', 'ocservia-agent', '-p', 'MainPID', '--value').strip()
    run('sudo', 'cp', '--', *OCSERV_FILES, environment().work / 'unchanged')
    effects = node_side_effects()
    path, body = prefix + '/revocation', {'reason': 'Business test: terminal node revocation'}
    api(path, body, role='anonymous', status=401)
    api(path, body, headers={'Origin': 'https://attacker.invalid'}, status=403)
    assert sql(f"SELECT status FROM nodes WHERE id='{node}';") == 'active'
    record('revocation_unauthorized_rejected')
    started = int(time.time())
    # 200 means the revoked trust and the connection close were both applied in
    # the request. A 503 leaves them to the background worker, which never counts
    # lost ownership as delivery, so it never stands in for them.
    with timed('revocation_request'):
        api(path, body)
    status, revision = sql(f"SELECT status||'|'||authorization_revision FROM nodes WHERE id='{node}';").split('|')
    assert status == 'revoked'
    assert sql(f"SELECT state FROM node_endpoint_keys WHERE node_id='{node}';") == 'revoked'
    assert api(prefix)['trust_status'] == 'revoked'
    # The request settled exactly this revoked intent with its fenced update and close.
    assert sql("SELECT desired_state='revoked' AND update_applied AND close_required AND close_applied FROM node_trust_convergence "
               f"WHERE node_id='{node}' AND revision={int(revision)};") == 't'
    with timed('revocation_ownership_invalidation'):
        wait_for('revoked ownership invalidation', lambda: sql(
            "SELECT NOT EXISTS (SELECT 1 FROM connection_owner_fencing "
            f"WHERE node_id=decode('{node.replace('-', '')}','hex') AND active);") == 't')
    with timed('revocation_node_offline'):
        wait_for('revoked node offline', lambda: api(prefix)['connection_state'] == 'offline', seconds=180)
    # The same Agent process keeps redialing, and no new session is admitted.
    assert run('systemctl', 'show', 'ocservia-agent', '-p', 'MainPID', '--value').strip() == agent_pid
    assert run('systemctl', 'show', 'ocservia-agent', '-p', 'ActiveState', '--value').strip() == 'active'
    assert sql("SELECT count(*) FROM connection_owner_fencing "
               f"WHERE node_id=decode('{node.replace('-', '')}','hex') AND session_epoch>{int(before['session_epoch'])};") == '0'
    ended = run('sudo', 'journalctl', '--no-pager', '-u', 'ocservia-agent', '--since', f'@{started}', '-o', 'cat').count(
        'controller connection ended')
    assert ended >= 1
    record('node_revoked_trust_converged_and_disconnected',
           authorization_revision=int(revision), agent_session_ends=ended)
    key = secrets.token_hex(16)
    denied = api(prefix + '/service:reload', {'reason': 'Business test: old authority after revocation', 'ttl_seconds': 300},
                 headers={'Idempotency-Key': key, 'If-Match': f'"revision-{api(prefix)["version"]}"'}, status=404)
    assert denied['type'] == 'https://ocservia.dev/problems/not-found'
    assert sql(f"SELECT count(*) FROM operations WHERE workspace_id='{environment().workspace}' AND idempotency_key='{key}';") == '0'
    check_unchanged(*OCSERV_FILES)
    assert node_side_effects() == effects
    record('revoked_node_old_authority_without_privd_effect', side_effects=effects)


def vpn_smoke(phase, relay_recovery=False):
    password = (environment().work / 'private/smoke-vpn-password').read_text().strip()
    with (environment().work / 'private' / f'openconnect-{phase}.log').open('wb') as log:
        vpn = subprocess.Popen(['sudo', 'ip', 'netns', 'exec', 'business-test-client', 'openconnect', '--non-inter',
                                '--protocol=anyconnect', '--user=business-test-smoke-user', '--passwd-on-stdin',
                                '--cafile', str(environment().work / 'ca.crt'), '--script', str(environment().work / 'vpn-script'),
                                'https://10.207.0.1:44443'], stdin=subprocess.PIPE, stdout=log, stderr=log)
        try:
            vpn.stdin.write((password + '\n').encode())
            vpn.stdin.close()

            def connected():
                result = subprocess.run(['sudo', 'ip', 'netns', 'exec', 'business-test-client', 'ping',
                                         '-c', '2', '-W', '2', '10.208.0.1'], capture_output=True)
                return vpn.poll() is None and result.returncode == 0

            wait_for('real VPN ICMP ' + phase, connected, 40)
            assert api(f"nodes/{os.environ['OCSERVIA_BUSINESS_TEST_NODE_ID']}")['config_revision'] == 1
            record('real_vpn_' + phase)
            if relay_recovery:
                smoke_relay_recovery(connected)
        finally:
            subprocess.run(['sudo', 'ip', 'netns', 'exec', 'business-test-client', 'pkill', '-INT', '-x', 'openconnect'],
                           capture_output=True, check=False)
            vpn.wait(timeout=15)


PHASES = {
    'signer_prepare': signer_prepare,
    'signer_import': signer_import,
    'integrated_entry': integrated_entry,
    'local': local,
    'transport_ready': transport_ready,
    'bootstrap_token': bootstrap_token,
    'bootstrap_verify': bootstrap_verify,
    'check_enrolled_identity': check_enrolled_identity,
    'config_prepare': config_prepare,
    'browser_prepare': browser_prepare,
    'browser_verify': browser_verify,
    'smoke_user': smoke_user,
    'vpn_after_config_apply': lambda: vpn_smoke('after_config_apply'),
    'resilience': resilience,
    'revocation': revocation,
}

if __name__ == '__main__':
    phase = sys.argv[1] if len(sys.argv) > 1 else ''
    if phase not in PHASES:
        raise SystemExit('unknown phase')
    PHASES[phase]()
