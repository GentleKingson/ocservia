#!/usr/bin/env python3
"""Controller checks for the disposable cross-VM native business run."""
import importlib.util
import json
import os
from pathlib import Path
import re
import secrets
import subprocess
import sys
import time

ROOT = Path(__file__).resolve().parents[1]
WORK = Path(os.environ['T07_WORK'])
OUTBOX = WORK / 'outbox'
STATE = WORK / 'state'
PRIVATE = WORK / 'private'
spec = importlib.util.spec_from_file_location('release_business', ROOT / 'scripts/release-business-api.py')
business = importlib.util.module_from_spec(spec)
spec.loader.exec_module(business)


def hex64(path):
    value = path.read_text().strip()
    assert re.fullmatch(r'[0-9a-f]{64}', value), path.name
    return value


def node_id():
    value = (STATE / 'node-id').read_text().strip()
    assert re.fullmatch(r'[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}', value)
    return value


def encrypt(source, public, output):
    with source.open('rb') as input_file, output.open('wb') as output_file:
        subprocess.run(['openssl', 'pkeyutl', '-encrypt', '-pubin', '-inkey', str(public),
                        '-pkeyopt', 'rsa_padding_mode:oaep', '-pkeyopt', 'rsa_oaep_md:sha256',
                        '-pkeyopt', 'rsa_mgf1_md:sha256'], stdin=input_file, stdout=output_file,
                       stderr=subprocess.DEVNULL, check=True)


def plan_result(plan_id):
    item = business.api('config-plans/' + plan_id)
    operation = business.api('operations/' + item['operation_id'])
    states = {'queued', 'dispatched', 'succeeded', 'failed', 'unknown', 'expired', 'cancelled', 'pending', 'valid'}
    status = {key: value if value in states else 'unclassified' for key, value in (
        ('state', item['state']), ('validation', item['validation']), ('operation_state', operation['state']))}
    status.update({key: item.get(key) is True for key in ('current_unchanged', 'staging_cleaned')})
    (business.EVIDENCE / 'config-plan-status.json').write_text(json.dumps(status) + '\n')
    if item['state'] in ('failed', 'expired', 'cancelled'):
        raise RuntimeError('cross-VM configuration plan reached a terminal failure')
    return item if item['state'] == 'succeeded' else None


def issue_token(endpoint):
    assert endpoint == hex64(STATE / 'agent-endpoint-id')
    response = business.api('enrollment-tokens', {
        'workspace_id': os.environ['T07_WORKSPACE'], 'environment': 'development',
        'expected_node_name': 'github-hosted-node', 'expected_endpoint_id': endpoint,
        'ttl_seconds': 600, 'reason': 'cross-VM native business enrollment'}, status=201)
    assert re.fullmatch(r'[A-Za-z0-9_-]{43}', response['token'])
    token = OUTBOX / 'enrollment-token' / 'enrollment-token'
    token.parent.mkdir(parents=True, exist_ok=True)
    token.write_text(response['token'] + '\n')
    token.chmod(0o600)
    (STATE / 'token-id').write_text(response['id'] + '\n')


def authority(peer_dir):
    node = node_id()
    assert business.sql(f"SELECT status FROM nodes WHERE id='{node}';") == 'pending'
    observed = business.api('nodes/' + node)
    assert observed['trust_status'] == 'pending'
    caps = json.loads(business.sql(
        f"SELECT json_agg(capability ORDER BY capability) FROM node_capabilities WHERE node_id='{node}';"))
    binding = {'labels': {}, 'policy': 'standard', 'capabilities': caps}
    business.api(f'nodes/{node}/approval', {**binding, 'reason': 'cross-VM approval'}, status=400)
    decision = business.approval('node.approve', 'node', node, {'node_approval': binding})
    (STATE / 'node-approval-id').write_text(decision + '\n')
    business.api(f'nodes/{node}/approval', {**binding, 'reason': 'cross-VM approval'},
                 headers={'X-Approval-ID': decision})
    assert business.sql(f"SELECT status FROM nodes WHERE id='{node}';") == 'active'
    credential = business.api(f'nodes/{node}/privd-attestation-credentials',
                              {'ttl_seconds': 600, 'reason': 'cross-VM native receipt'}, status=201)
    credential_file = PRIVATE / 'privd-credential'
    credential_file.write_text(credential['credential'])
    credential_file.chmod(0o600)
    key = f"{node}/{hex64(peer_dir / 'vpn-cert-sha256')}/{hex64(peer_dir / 'vpn-spki-sha256')}/none"
    reference = business.api('secret-provider-refs', {
        'provider': 'node-local-tls-v1', 'key_path': key, 'version': 'v1',
        'reason': 'cross-VM node-local TLS'}, status=201)
    target = OUTBOX / 'business-authority'
    target.mkdir(parents=True, exist_ok=True)
    encrypt(credential_file, peer_dir / 'exchange.pub', target / 'credential.oaep')
    (target / 'authority.json').write_text(json.dumps({
        'node_id': node, 'agent_endpoint_id': hex64(STATE / 'agent-endpoint-id'),
        'controller_nonce_hex': credential['controller_nonce_hex'],
        'credential_context_sha256_hex': credential['credential_context_sha256_hex'],
        'secret_ref_id': reference['id'], 'secret_ref_key': key}) + '\n')
    (STATE / 'secret-ref-id').write_text(reference['id'] + '\n')


def register_authority(ready_dir):
    node = node_id()
    ready = json.loads((ready_dir / 'ready.json').read_text())
    assert ready['node_id'] == node
    assert ready['agent_endpoint_id'] == hex64(STATE / 'agent-endpoint-id')
    proof = json.loads((ready_dir / 'proof.json').read_text())
    proof['credential'] = (PRIVATE / 'privd-credential').read_text()
    business.api(f'nodes/{node}/privd-attestation-keys:register', proof, role='anonymous', status=201)
    target = OUTBOX / 'business-authority-registered'
    target.mkdir(parents=True, exist_ok=True)
    (target / 'ready.json').write_text(json.dumps({
        'node_id': node, 'agent_endpoint_id': ready['agent_endpoint_id']}) + '\n')


def run_business(ready_dir, peer_dir):
    node = node_id()
    ready = json.loads((ready_dir / 'ready.json').read_text())
    assert ready['node_id'] == node
    assert ready['agent_endpoint_id'] == hex64(STATE / 'agent-endpoint-id')
    os.environ['T07_NODE'] = node
    def online():
        item = business.api('nodes/' + node)
        checks = {'active': item['trust_status'] == 'active',
                  'online': item['connection_state'] == 'online',
                  'fresh': item['freshness'] == 'fresh',
                  'heartbeat': bool(item.get('last_heartbeat_at')),
                  'ocserv_version': bool(item.get('ocserv_version')),
                  'agent_version': item.get('agent_version') == '0.0.0'}
        print('native_online=' + json.dumps(checks), flush=True)
        return item if all(checks.values()) else None
    business.wait_for('native Agent online with fresh heartbeat', online, 180)
    assert business.sql(f"SELECT encode(endpoint_id,'hex') FROM node_endpoint_keys WHERE node_id='{node}';") == hex64(STATE / 'agent-endpoint-id')
    owner = business.owner()
    assert owner and owner['connection_id']
    reference = (STATE / 'secret-ref-id').read_text().strip()
    values = {'auth': 'plain[passwd=/etc/ocserv/ocpasswd]', 'cookie-timeout': '300',
              'device': 'vpns', 'dns': '1.1.1.1', 'ipv4-network': '10.208.0.0/24',
              'max-clients': '4', 'max-same-clients': '2', 'socket-file': '/run/ocserv.socket',
              'tcp-port': '44443', 'udp-port': '0'}
    directives = [{'name': name, 'value': value} for name, value in values.items()]
    directives += [{'name': name, 'secret_ref': {'secret_ref_id': reference}}
                   for name in ('server-cert', 'server-key')]
    planned = business.api(f'nodes/{node}/config-plans', {
        'expected_revision': 0, 'template': {'name': 'cross-vm-native', 'directives': directives},
        'ttl_seconds': 900, 'reason': 'cross-VM native configuration'},
        headers={'Idempotency-Key': secrets.token_hex(16)}, status=202)
    plan = business.wait_for('cross-VM configuration plan', lambda: plan_result(planned['id']))
    assert plan['validation'] == 'valid' and re.fullmatch(r'[0-9a-f]{64}', plan['materialized_hash'])
    approval_id = business.approval('config.apply', 'config_plan', plan['id'])
    applied = business.api('config-plans/' + plan['id'] + '/apply', {
        'approval_id': approval_id, 'reason': 'cross-VM native configuration'},
        headers={'Idempotency-Key': secrets.token_hex(16)}, status=202)
    applied = business.wait_for('cross-VM configuration apply', lambda: business.completed(applied))
    assert applied['config_apply_state'] == 'succeeded'
    assert business.api('nodes/' + node)['config_revision'] == 1
    reload_operation = business.authorized_reload('cross-VM native service reload')
    (WORK / 'user.pub.pem').write_bytes((peer_dir / 'user.pub.pem').read_bytes())
    business.smoke_user()
    target = OUTBOX / 'business-controller-done'
    target.mkdir(parents=True, exist_ok=True)
    encrypt(PRIVATE / 'smoke-vpn-password', peer_dir / 'exchange.pub', target / 'vpn-password.oaep')
    operations = [applied, reload_operation]
    (target / 'done.json').write_text(json.dumps({
        'node_id': node, 'agent_endpoint_id': hex64(STATE / 'agent-endpoint-id'),
        'owner_epoch': owner['owner_epoch'], 'config_hash': plan['materialized_hash'],
        'operations': [{'id': item['id'], 'command_id': item['command_id']}
                       for item in operations]}) + '\n')


def verify_operations(actual_operations, expected_operations):
    assert len(actual_operations) == len(expected_operations)
    checked = []
    for actual, wanted in zip(actual_operations, expected_operations):
        assert actual['id'] == wanted['id'] and actual['command_id'] == wanted['command_id']
        assert actual['journal'] == '1|succeeded||1'
        assert actual['privd'] == '1|applied|1'
        op = business.api('operations/' + wanted['id'])
        assert op['state'] == 'succeeded' and op['command_id'] == wanted['command_id']
        assert business.sql(f"SELECT state FROM operations WHERE id='{wanted['id']}';") == 'succeeded'
        assert business.sql(f"SELECT count(*) FROM outbox_events WHERE command_id='{wanted['command_id']}' AND published_at IS NOT NULL;") == '1'
        assert int(business.sql(f"SELECT count(*) FROM command_attempts WHERE command_id='{wanted['command_id']}' AND state='sent';")) >= 1
        checked.append({'id': wanted['id'], 'command_id': wanted['command_id'],
                        'database_state': 'succeeded', 'journal': actual['journal'],
                        'privd': actual['privd'], 'outbox_published': True})
    return checked


def verify_result(result_dir):
    result = json.loads((result_dir / 'result.json').read_text())
    expected = json.loads((OUTBOX / 'business-controller-done' / 'done.json').read_text())
    assert result['node_id'] == expected['node_id']
    assert result['agent_endpoint_id'] == expected['agent_endpoint_id']
    assert result['config_hash'] == expected['config_hash']
    assert result['native_services'] == ['ocserv', 'ocservia-agent', 'ocservia-privd']
    assert result['native_reload_events'] >= 1
    assert result['vpn']['connected'] and result['vpn']['native_session'] and result['vpn']['icmp']
    checked = verify_operations(result['operations'], expected['operations'])
    evidence = Path(os.environ['ARTIFACT_DIR'])
    (evidence / 'business.json').write_text(json.dumps({
        'node_id': expected['node_id'], 'agent_endpoint_id': expected['agent_endpoint_id'],
        'config_hash': expected['config_hash'], 'node_approval_id': (STATE / 'node-approval-id').read_text().strip(),
        'independent_approval_negative_controls': ['self', 'request_hash', 'duplicate'],
        'native_services': result['native_services'], 'native_reload_events': result['native_reload_events'],
        'operations': checked, 'vpn_native_session': result['vpn']['native_session'],
        'vpn_icmp': result['vpn']['icmp']}, sort_keys=True) + '\n')
    (evidence / 'business.txt').write_text(
        'B01=PASS\nB02=PASS\nB03=PASS\nB04=PASS\nB05=PASS\nCROSS_VM_PRIVATE_BUSINESS=PASS\n')


def recovery(phase, peer_dir):
    node = node_id()
    os.environ['T07_NODE'] = node
    prefix = 'nodes/' + node
    baseline_path = OUTBOX / 'recovery-controller-baseline/result.json'
    queued_path = OUTBOX / 'recovery-controller-queued/result.json'
    if phase == 'baseline':
        observed = business.api(prefix)
        assert observed['connection_state'] == 'online' and observed['freshness'] == 'fresh'
        assert observed['path']['mode'] == 'relay'
        container = business.run('docker', 'ps', '-q', '--filter',
                                 'label=com.docker.compose.project=' + os.environ['COMPOSE_PROJECT'],
                                 '--filter', 'label=com.docker.compose.service=transportd').strip()
        assert re.fullmatch(r'[0-9a-f]{12,64}', container)
        argv = business.run('docker', 'exec', container, 'sh', '-c',
                            'for exe in /proc/[0-9]*/exe; do '
                            'if [ "$(readlink "$exe")" = /usr/local/bin/ocservia-transportd ]; then '
                            'cat "${exe%/exe}/cmdline"; fi; done').rstrip('\0').split('\0')
        assert [argv[i+1] for i, arg in enumerate(argv) if arg == '--relay-url'] == [(WORK / 'relay/url').read_text().strip()]
        assert argv[argv.index('--relay-mode')+1] == 'custom'
        value = {'node_id':node, 'agent_endpoint_id':hex64(STATE / 'agent-endpoint-id'),
                 'owner':business.owner(), 'path_mode':'relay', 'single_controller_relay':True}
        assert value['owner'] and value['owner']['connection_id']
    elif phase == 'queued':
        business.wait_for_relay_outage(node)
        business.wait_for('node offline during Relay outage',
                          lambda: business.api(prefix)['connection_state'] == 'offline')
        approval_id = business.approval('service.reload', 'node', node)
        body = {'reason':'cross-VM Relay recovery', 'ttl_seconds':300}
        key = secrets.token_hex(16)
        for _ in range(3):
            headers = {'Idempotency-Key':key, 'X-Approval-ID':approval_id,
                       'If-Match':f'"revision-{business.api(prefix)["version"]}"'}
            operation = business.api(prefix + '/service:reload', body, headers=headers, status=(202, 409))
            if 'id' in operation:
                break
            assert operation['type'] == 'https://ocservia.dev/problems/stale-revision'
        assert 'id' in operation
        (STATE / 'recovery-request.json').write_text(json.dumps({'body':body, 'headers':headers}))
        time.sleep(10)
        assert business.api('operations/' + operation['id'])['state'] == 'queued'
        command = operation['command_id']
        assert business.sql(f"SELECT count(*) FROM outbox_events WHERE command_id='{command}' AND published_at IS NULL;") == '1'
        assert business.sql(f"SELECT count(*) FROM command_attempts WHERE command_id='{command}' AND state='sent';") == '0'
        value = {'node_id':node, 'operation':{'id':operation['id'], 'command_id':command},
                 'owner_lease_invalid':True, 'offline':True, 'state':'queued',
                 'unpublished_outbox':1, 'sent_attempts':0}
    elif phase == 'done':
        before = json.loads(baseline_path.read_text())
        queued = json.loads(queued_path.read_text())
        after = business.wait_for('fresh Agent session after Relay recovery',
                                  lambda: business.fresh_owner(before['owner']), seconds=90)
        operation = business.wait_for('same operation after Relay recovery',
                                      lambda: business.completed(queued['operation']))
        request = json.loads((STATE / 'recovery-request.json').read_text())
        replay = business.api(prefix + '/service:reload', request['body'], headers=request['headers'], status=202)
        assert replay['id'] == operation['id'] and replay['command_id'] == operation['command_id']
        assert business.api('approval-requests/' + request['headers']['X-Approval-ID'])['status'] == 'consumed'
        value = {**queued, 'owner':after, 'idempotent_replay':True}
    elif phase == 'verify':
        result = json.loads((peer_dir / 'result.json').read_text())
        before = json.loads(baseline_path.read_text())
        queued = json.loads(queued_path.read_text())
        done = json.loads((OUTBOX / 'recovery-controller-done/result.json').read_text())
        assert result['node_id'] == before['node_id'] and result['agent_endpoint_id'] == before['agent_endpoint_id']
        assert result['queued'] == queued
        assert before['path_mode'] == 'relay' and before['single_controller_relay'] is True
        assert queued['owner_lease_invalid'] and queued['offline'] and queued['state'] == 'queued'
        assert queued['unpublished_outbox'] == 1 and queued['sent_attempts'] == 0
        assert done['owner']['owner_epoch'] > before['owner']['owner_epoch']
        assert done['owner']['connection_id'] != before['owner']['connection_id'] and done['idempotent_replay'] is True
        old, new = result['before'], result['after']
        assert set(old['identity']) == {'endpoint.key', 'controller.endpoint'}
        assert set(old['services']) == {'ocserv', 'ocservia-agent', 'ocservia-privd'}
        assert all(item['pid'] > 0 and item['started'] > 0 for item in old['services'].values())
        assert old['identity'] == new['identity'] and old['services'] == new['services']
        assert old['vpn_session'] == new['vpn_session']
        assert old['vpn_session'] == json.loads((Path(os.environ['ARTIFACT_DIR']) / 'business.json').read_text())['vpn_native_session']
        assert old['icmp'] and new['icmp'] and old['udp_isolated'] and new['udp_isolated']
        assert old['single_agent_relay'] and new['single_agent_relay']
        assert result['queued_native'] == {'journal':0, 'privd':0, 'reload_delta':0, 'vpn_unchanged':True, 'relay_unreachable':True}
        assert new['reloads'] == old['reloads'] + 1
        checked = verify_operations(result['operations'], [queued['operation']])
        value = {**result, 'before_owner':before['owner'], 'after_owner':done['owner'], 'operations':checked}
        evidence = Path(os.environ['ARTIFACT_DIR'])
        (evidence / 'recovery.json').write_text(json.dumps(value, sort_keys=True) + '\n')
        (evidence / 'recovery.txt').write_text(
            'R01=PASS\nR02=PASS\nR03=PASS\nR04=PASS\nR05=PASS\nDEDICATED_RELAY_RECOVERY=PASS\n')
        return
    else:
        raise ValueError('unknown recovery phase')
    target = OUTBOX / ('recovery-controller-' + phase)
    target.mkdir(parents=True, exist_ok=True)
    (target / 'result.json').write_text(json.dumps(value, sort_keys=True) + '\n')


if __name__ == '__main__':
    phase = sys.argv[1]
    if phase.startswith('recovery-'):
        recovery(phase.removeprefix('recovery-'), Path(sys.argv[2]) if len(sys.argv) > 2 else None)
    elif phase == 'login':
        business.local()
    elif phase == 'issue-token':
        issue_token(sys.argv[2])
    elif phase == 'authority':
        authority(Path(sys.argv[2]))
    elif phase == 'register-privd':
        register_authority(Path(sys.argv[2]))
    elif phase == 'run':
        run_business(Path(sys.argv[2]), Path(sys.argv[3]))
    elif phase == 'verify':
        verify_result(Path(sys.argv[2]))
    else:
        raise SystemExit('unknown business phase')
