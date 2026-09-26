#!/usr/bin/env python3
"""Real Integrated entry and recovery checks; no replacement service or image build."""
import hashlib
import http.client
import importlib.util
import ipaddress
import json
import os
from pathlib import Path
import secrets
import shutil
import socket
import ssl
import subprocess
import sys
import time
import urllib.request

ROOT = Path(__file__).resolve().parents[1]
WORK = Path(os.environ['T07_WORK'])
EVIDENCE = Path(os.environ['ARTIFACT_DIR'])
CONTEXT = ssl.create_default_context(cafile=str(WORK / 'ca.crt'))
COMPOSE = str(ROOT / 'deploy/production/compose.sh')


def run(*args, check=True):
    return subprocess.run([str(arg) for arg in args], capture_output=True, text=True, check=check)


def container(name):
    return run(COMPOSE, 'ps', '-q', name).stdout.strip()


def inspect(name):
    return json.loads(run('docker', 'inspect', container(name)).stdout)[0]


def record(name, **data):
    with (EVIDENCE / 'integrated-checkpoints.jsonl').open('a') as output:
        output.write(json.dumps(dict(name=name, status='PASS', candidate_sha=os.environ['CANDIDATE_SHA'], **data)) + '\n')


def module(name):
    spec = importlib.util.spec_from_file_location(name, ROOT / 'scripts' / (name + '.py'))
    value = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(value)
    return value


def request(name, path, host=None):
    with socket.create_connection((name, 443), timeout=10) as sock:
        with CONTEXT.wrap_socket(sock, server_hostname=name) as tls:
            fingerprint = hashlib.sha256(tls.getpeercert(binary_form=True)).hexdigest()
            tls.sendall(f'GET {path} HTTP/1.1\r\nHost: {host or name}\r\nConnection: close\r\n\r\n'.encode())
            response = http.client.HTTPResponse(tls)
            response.begin()
            return response.status, response.read(), fingerprint


def entry():
    config = json.loads(run(COMPOSE, 'config', '--format', 'json').stdout)
    ports = sorted((name, str(port['published']), port['protocol'])
                   for name, service in config['services'].items() for port in service.get('ports', []))
    assert ports == [('edge', '443', 'tcp'), ('relay', '7842', 'udp')], ports
    manifest = json.loads((Path(os.environ['CANDIDATE_BUNDLE']) /
                           f"controller-release-{os.environ['CONTROLLER_ARCH']}.json").read_text())
    allowed = set(manifest['images'].values())
    for service in config['services'].values():
        assert 'build' not in service and service['image'] in allowed
    control_name, relay_name = os.environ['OCSERV_PUBLIC_HOST'], os.environ['OCSERV_RELAY_PUBLIC_HOST']
    status, body, control_cert = request(control_name, '/api/v1/version')
    assert status == 200 and json.loads(body)['commit'] == os.environ['CANDIDATE_SHA']
    status, _, relay_cert = request(relay_name, '/healthz')
    assert status == 200 and control_cert != relay_cert
    assert request(control_name, '/api/v1/version', 'wrong.invalid')[0] == 421
    for server_name in (None, 'unknown.invalid'):
        context = ssl.SSLContext(ssl.PROTOCOL_TLS_CLIENT)
        context.check_hostname = False
        context.verify_mode = ssl.CERT_NONE
        try:
            with socket.create_connection((control_name, 443), timeout=8) as sock:
                with context.wrap_socket(sock, server_hostname=server_name):
                    raise AssertionError('unknown or missing SNI accepted')
        except (ssl.SSLError, ConnectionResetError):
            pass
    signer, control = inspect('signer'), inspect('control-plane')
    assert not signer['HostConfig']['PortBindings']
    address = signer['NetworkSettings']['Networks']['ocservia-production_signer']['IPAddress']
    network = json.loads(run('docker', 'network', 'inspect', 'ocservia-production_signer').stdout)[0]
    assert network['Internal']
    command = ['nsenter', '--target', str(control['State']['Pid']), '--net', 'curl', '--fail',
               '--silent', '--show-error', '--max-time', '8', '--noproxy', '*']
    assert run(*command, '--cacert', WORK / 'ca.crt', '--resolve', f'signer:9443:{address}',
               'https://signer:9443/healthz').returncode == 0
    wrong_ca = WORK / 'untrusted-ca.crt'
    (WORK / 'empty-trust').mkdir(exist_ok=True)
    if not wrong_ca.exists():
        run('openssl', 'req', '-x509', '-newkey', 'rsa:2048', '-nodes', '-days', '1',
            '-subj', '/CN=untrusted-acceptance-ca', '-keyout', WORK / 'untrusted-ca.key', '-out', wrong_ca)
    assert run(*command, '--cacert', wrong_ca, '--capath', WORK / 'empty-trust',
               '--resolve', f'signer:9443:{address}', 'https://signer:9443/healthz', check=False).returncode == 60
    assert run(*command, '--cacert', WORK / 'ca.crt', '--resolve', f'wrong.invalid:9443:{address}',
               'https://wrong.invalid:9443/healthz', check=False).returncode == 60
    record('entry_tls_and_topology', ports=ports, controller_certificate_sha256=control_cert,
           relay_certificate_sha256=relay_cert, signer_tls='trusted CA required; hostname verified; no host port')


def sse(api, seconds):
    api.api('events/stream', role='anonymous', status=401)
    request = urllib.request.Request(api.ORIGIN + '/api/v1/events/stream', headers={
        'Origin': api.ORIGIN, 'X-Workspace-ID': api.WORKSPACE, 'Accept': 'text/event-stream'})
    started = time.monotonic()
    connections, heartbeats = 0, 0
    lifetimes = []
    while time.monotonic() - started < seconds:
        with api.client('requester')[0].open(request, timeout=20) as response:
            assert response.status == 200 and response.headers['Content-Type'] == 'text/event-stream'
            connections += 1
            connected = time.monotonic()
            while time.monotonic() - started < seconds:
                line = response.readline()
                if not line:
                    lifetimes.append(time.monotonic() - connected)
                    break
                if line.startswith(b': keepalive'):
                    heartbeats += 1
        assert connections <= 3, 'unexpected repeated stream disconnects'
    assert heartbeats > 0
    if seconds > 1800:
        assert connections == 2 and len(lifetimes) == 1 and 1770 <= lifetimes[0] <= 1830, lifetimes
    record('authorized_sse', seconds=round(time.monotonic() - started), heartbeats=heartbeats,
           connections=connections, completed_lifetimes=lifetimes, unauthorized_status=401,
           boundary=('30-minute application lifetime and reconnect observed; keepalive prevents Edge inactivity'
                     if seconds > 1800 else 'short authorized stream; lifetime boundary not exercised'))


def rejected_upgrade():
    state = Path(os.environ['OCSERV_CONTROLLER_STATE_ROOT']) / 'current-release.json'
    before = state.read_bytes()
    containers = run(COMPOSE, 'ps', '-q').stdout
    bundle = WORK / 'invalid-bundle'
    shutil.copytree(os.environ['CANDIDATE_BUNDLE'], bundle)
    signature = bundle / 'SHA256SUMS.sig'
    signature.write_bytes(bytes(len(signature.read_bytes())))
    result = run(ROOT / 'deploy/production/controller.sh', 'upgrade', '--release-file',
                 bundle / f"controller-release-{os.environ['CONTROLLER_ARCH']}.json", check=False)
    assert result.returncode != 0 and 'release bundle authenticity verification failed' in result.stderr
    assert state.read_bytes() == before and run(COMPOSE, 'ps', '-q').stdout == containers
    record('invalid_signature_rejected_before_service_stop')
    # Hold only this disposable environment's published port to force a real
    # activation failure after verification/pull, then retry the same manifest.
    run('docker', 'stop', container('edge'))
    with socket.socket() as occupied:
        occupied.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
        occupied.bind(('0.0.0.0', 443))
        occupied.listen()
        result = run(ROOT / 'deploy/production/controller.sh', 'upgrade', '--release-file',
                     Path(os.environ['CANDIDATE_BUNDLE']) /
                     f"controller-release-{os.environ['CONTROLLER_ARCH']}.json", check=False)
    assert result.returncode != 0 and 'activation started but was not confirmed successful' in result.stderr
    assert state.read_bytes() == before
    pending = json.loads((state.parent / 'pending-release.json').read_text())
    assert pending['phase'] == 'failed' and pending['manifest']['source_commit'] == os.environ['CANDIDATE_SHA']
    assert (Path(os.environ['OCSERV_SIGNER_STATE_DIR']) / 'ledger.db').is_file()
    for name in ('postgres', 'backup'):
        service = run(COMPOSE, 'ps', '-a', '-q', name).stdout.strip()
        assert service
        state = json.loads(run('docker', 'inspect', service).stdout)[0]['State']
        if state['Status'] == 'created':
            run('docker', 'start', service)
        # Backup's configured health interval is five minutes on engines
        # without fast startup probes; do not retry upgrade while it is starting.
        deadline = time.monotonic() + 360
        while time.monotonic() < deadline:
            state = json.loads(run('docker', 'inspect', service).stdout)[0]['State']
            assert state['Status'] == 'running' and state['Health']['Status'] != 'unhealthy', state['Status']
            if state['Health']['Status'] == 'healthy':
                break
            time.sleep(2)
        assert state['Health']['Status'] == 'healthy'
    record('activation_failure_preserves_current_and_pending_state')


def public_sources():
    if not os.environ.get('INTEGRATED_PUBLIC_ADDRESS'):
        return
    deadline = time.monotonic() + 900
    directory = EVIDENCE / 'public-clients'
    while time.monotonic() < deadline:
        files = list(directory.glob('*/result.json'))
        if len(files) == 2:
            break
        time.sleep(5)
    assert len(files) == 2, 'two external public client results required'
    rows = [json.loads(path.read_text()) for path in files]
    assert len({row['public_client_ip'] for row in rows}) == 2
    logs = run('docker', 'logs', container('control-plane')).stdout
    events = []
    for line in logs.splitlines():
        try:
            events.append(json.loads(line))
        except json.JSONDecodeError:
            continue
    for row in rows:
        assert row['status'] == 'PASS' and row['candidate_sha'] == os.environ['CANDIDATE_SHA']
        assert row['server_ip'] == os.environ['INTEGRATED_PUBLIC_ADDRESS']
        assert ipaddress.ip_address(row['public_client_ip']).is_global
        matched = [event for event in events if event.get('event') == 'auth.result'
                   and event.get('request_id') == row['request_id']]
        assert len(matched) == 1 and matched[0]['source_ip'] == row['public_client_ip'], matched
        row['source_ip_status'] = 'PASS: matched server auth.result; spoofed headers ignored'
    record('external_public_sources', clients=rows, spoofed_headers_ignored=True)


def public_relay():
    if not os.environ.get('INTEGRATED_PUBLIC_ADDRESS'):
        return
    probe = Path(os.environ['INTEGRATED_RELAY_PROBE'])
    digest = hashlib.sha256(probe.read_bytes()).hexdigest()
    assert digest == os.environ['INTEGRATED_RELAY_PROBE_SHA256']
    result = run(probe, os.environ['OCSERV_RELAY_URL_A'], WORK / 'ca.crt', WORK / 'private/relay-access-token')
    lines = result.stdout.splitlines()
    assert 'authenticated_tcp=PASS' in lines and 'quic_address_discovery=PASS' in lines
    assert any(line.startswith('wrong_token=PASS') for line in lines)
    addresses = [line.split('=', 1)[1] for line in lines if line.startswith('quic_global_v4=')]
    assert len(addresses) == 1 and addresses[0].rsplit(':', 1)[0] == os.environ['INTEGRATED_PUBLIC_ADDRESS']
    record('public_tcp_udp_same_host', discovered_address=addresses[0], probe_sha256=digest,
           authenticated_tcp=True, wrong_token_rejected=True, https_discovery_fallback=False)


def revocation_check():
    os.environ['T07_SIGNER_CONTAINER'] = container('signer')
    signer = module('release-production-signer')
    number = signer.export_crl()
    verify = run('openssl', 'verify', '-CAfile', WORK / 'signer-root.pem', '-untrusted', WORK / 'issuer.pem',
                 '-CRLfile', WORK / 'issuer.crl.pem', '-crl_check', WORK / 'revoked-leaf.pem', check=False)
    assert verify.returncode != 0 and 'certificate revoked' in verify.stderr
    run('openssl', 'verify', '-CAfile', WORK / 'signer-root.pem', '-untrusted', WORK / 'issuer.pem',
        '-CRLfile', WORK / 'issuer.crl.pem', '-crl_check', WORK / 'control-chain.pem')
    return number


def rollback_and_upgrade(api, identities, identity_files):
    if not os.environ.get('INTEGRATED_BASELINE_SOURCE'):
        return
    before = revocation_check()
    checkpoint = Path(os.environ['OCSERV_CONTROLLER_STATE_ROOT']) / 'signer-checkpoint.json'
    previous_checkpoint = json.loads(checkpoint.read_text())
    run(ROOT / 'deploy/production/controller.sh', 'rollback')
    status, body, _ = request(os.environ['OCSERV_PUBLIC_HOST'], '/api/v1/version')
    assert status == 200 and json.loads(body)['commit'] == os.environ['INTEGRATED_BASELINE_SHA']
    state = Path(os.environ['OCSERV_CONTROLLER_STATE_ROOT']) / 'current-release.json'
    baseline = json.loads(state.read_text())
    os.environ['T07_SIGNER_IMAGE'] = baseline['images']['signer']
    reverted = revocation_check()
    assert reverted > before and run('sha256sum', *identity_files).stdout == identities
    run(ROOT / 'deploy/production/controller.sh', 'upgrade', '--release-file',
        Path(os.environ['CANDIDATE_BUNDLE']) / f"controller-release-{os.environ['CONTROLLER_ARCH']}.json")
    os.environ['T07_SIGNER_IMAGE'] = os.environ['OCSERV_SIGNER_IMAGE']
    assert revocation_check() > reverted
    api.transport_ready()
    api.trust_controller()
    assert run('sha256sum', *identity_files).stdout == identities
    current_checkpoint = json.loads(checkpoint.read_text())
    assert all(current_checkpoint[key] == previous_checkpoint[key] for key in ('state_version', 'issuer_sha256', 'policy'))
    assert current_checkpoint['revision'] >= previous_checkpoint['revision']
    record('rollback_and_reupgrade_preserve_revocation_and_identity',
           baseline_sha=baseline['source_commit'], crl_before=before, crl_after_rollback=reverted,
           checkpoint_before=previous_checkpoint, checkpoint_after=current_checkpoint)


def recovery():
    api = module('release-business-api')
    identity_files = ['/var/lib/ocservia-agent/identity/endpoint.key',
                      '/var/lib/ocservia-agent/identity/controller.endpoint',
                      '/etc/ocservia-agent/user-password-seal-private.pem',
                      '/etc/ocservia-agent/p12-password-seal-private.pem',
                      WORK / 'production-signer/secrets/issuer-key.pem',
                      WORK / 'production-signer/secrets/issuer-chain.pem']
    identities = run('sha256sum', *identity_files).stdout
    agent_start = run('systemctl', 'show', 'ocservia-agent', '-p', 'ExecMainStartTimestampMonotonic', '--value').stdout
    old = {name: container(name) for name in ('control-plane', 'transportd', 'gateway', 'relay', 'edge', 'signer')}
    run(COMPOSE, 'up', '-d', '--no-build', '--no-deps', '--force-recreate', '--wait', *old)
    assert all(container(name) != value for name, value in old.items())
    api.transport_ready()
    api.trust_controller()
    os.environ['T07_SIGNER_CONTAINER'] = container('signer')
    entry()
    rollback_and_upgrade(api, identities, identity_files)
    node = os.environ['T07_NODE']
    api.wait_for('established Agent reconnect after recreation',
                 lambda: api.api('nodes/' + node)['connection_state'] == 'online')
    approval = api.approval('service.reload', 'node', node)
    operation = api.api('nodes/' + node + '/service:reload', {'reason': 'Integrated recreation acceptance', 'ttl_seconds': 300},
                        headers={'Idempotency-Key': secrets.token_hex(16),
                                 'If-Match': f'"revision-{api.api("nodes/" + node)["version"]}"',
                                 'X-Approval-ID': approval}, status=202)
    def completed():
        result = api.api('operations/' + operation['id'])
        assert result['state'] not in ('failed', 'expired', 'cancelled', 'unknown'), result['state']
        return result['state'] == 'succeeded'
    api.wait_for('approved command after recreation', completed)
    receipt = api.cross_check(operation)
    assert receipt['journal_count_state_error_receipt'] == '1|succeeded||1'
    assert receipt['root_count_state_response'] == '1|applied|1'
    assert run('sha256sum', *identity_files).stdout == identities
    assert run('systemctl', 'show', 'ocservia-agent', '-p', 'ExecMainStartTimestampMonotonic', '--value').stdout == agent_start
    revocation_check()
    record('recreation_preserves_identity_and_revocation', operation_id=operation['id'], agent_restarted=False)
    public_relay()
    if os.environ.get('INTEGRATED_PUBLIC_ADDRESS'):
        (EVIDENCE / 'public-entry.json').write_text(json.dumps(dict(
            address=os.environ['INTEGRATED_PUBLIC_ADDRESS'], ca_pem=(WORK / 'ca.crt').read_text(),
            candidate_sha=os.environ['CANDIDATE_SHA'])))
    sse(api, 2160 if os.environ.get('INTEGRATED_PUBLIC_ADDRESS') else 12)
    public_sources()


if __name__ == '__main__':
    run('bash', ROOT / 'scripts/release-business-environment.sh')
    {'entry': entry, 'recovery': recovery, 'rejected_upgrade': rejected_upgrade}[sys.argv[1]]()
