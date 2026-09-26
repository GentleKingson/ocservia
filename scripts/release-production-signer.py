#!/usr/bin/env python3
"""Production Signer and operator CRL acceptance on a disposable Actions node."""
import base64
import json
import os
from pathlib import Path
import socket
import subprocess
import sys
import time
import uuid

ROOT = Path(__file__).resolve().parents[1]
WORK = Path(os.environ['T07_WORK'])
PRIVATE = WORK / 'private'
STATE = WORK / 'production-signer'
EVIDENCE = Path(os.environ['ARTIFACT_DIR'])
CONTAINER = os.environ['T07_SIGNER_CONTAINER']
IMAGE = os.environ['T07_SIGNER_IMAGE']
INTEGRATED = os.environ.get('OCSERV_DEPLOYMENT_MODE') == 'integrated'
SIGNER_URL = 'https://signer:9443' if INTEGRATED else 'https://localhost:19444'
NODE_CRL = Path('/etc/ocservia-p2-crl')


def run(*args, data=None):
    return subprocess.run([str(a) for a in args], input=data, check=True,
                          stdout=subprocess.PIPE).stdout


def admin(*args):
    return run('docker', 'run', '--rm', '--read-only', '--cap-drop=ALL',
               '--security-opt=no-new-privileges:true',
               '-v', f'{STATE}/secrets:/run/secrets:ro',
               '-v', f'{STATE}/input:/input:ro',
               '-v', f'{STATE}/data:/var/lib/ocservia-signer', IMAGE, *args)


def health():
    for _ in range(30):
        result = subprocess.run(['docker', 'exec', CONTAINER, '/ocserv-signer', 'health',
                                 '--url', SIGNER_URL + '/healthz'],
                                stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        if result.returncode == 0:
            return
        time.sleep(1)
    raise RuntimeError('production Signer did not become healthy')


def prepare():
    STATE.mkdir(mode=0o700)
    for folder in ('secrets', 'input', 'data'):
        run('sudo', 'install', '-d', '-o', '65532', '-g', '65532', '-m', '700', STATE / folder)
    run('openssl', 'req', '-x509', '-newkey', 'rsa:2048', '-nodes', '-days', '7',
        '-subj', '/CN=P2-offline-root', '-addext', 'basicConstraints=critical,CA:TRUE,pathlen:1',
        '-addext', 'keyUsage=critical,keyCertSign,cRLSign',
        '-keyout', PRIVATE / 'signer-root.key', '-out', WORK / 'signer-root.pem')
    run('openssl', 'req', '-new', '-newkey', 'rsa:2048', '-nodes', '-subj', '/CN=P2-online-issuer',
        '-keyout', PRIVATE / 'issuer-key.pem', '-out', WORK / 'issuer.csr')
    (WORK / 'issuer.ext').write_text('basicConstraints=critical,CA:TRUE,pathlen:0\n'
                                   'keyUsage=critical,keyCertSign,cRLSign\n'
                                   'subjectKeyIdentifier=hash\nauthorityKeyIdentifier=keyid:always\n')
    run('openssl', 'x509', '-req', '-in', WORK / 'issuer.csr', '-days', '3',
        '-CA', WORK / 'signer-root.pem', '-CAkey', PRIVATE / 'signer-root.key',
        '-set_serial', '2', '-extfile', WORK / 'issuer.ext', '-out', WORK / 'issuer.pem')
    (WORK / 'issuer-chain.pem').write_bytes((WORK / 'issuer.pem').read_bytes() +
                                          (WORK / 'signer-root.pem').read_bytes())
    sources = {'issuer-chain.pem': WORK / 'issuer-chain.pem', 'issuer-key.pem': PRIVATE / 'issuer-key.pem',
               'tls-cert.pem': WORK / 'secrets/tls.crt', 'tls-key.pem': PRIVATE / 'tls.key',
               'api-token': PRIVATE / 'certificate-signer-token', 'tls-ca.pem': WORK / 'ca.crt'}
    if INTEGRATED:
        run('openssl', 'req', '-new', '-newkey', 'rsa:2048', '-nodes', '-subj', '/CN=signer',
            '-keyout', PRIVATE / 'signer-tls.key', '-out', WORK / 'signer-tls.csr')
        (WORK / 'signer-tls.ext').write_text('subjectAltName=DNS:signer\nextendedKeyUsage=serverAuth\n')
        run('openssl', 'x509', '-req', '-days', '1', '-in', WORK / 'signer-tls.csr',
            '-CA', WORK / 'ca.crt', '-CAkey', PRIVATE / 'ca.key', '-CAcreateserial',
            '-extfile', WORK / 'signer-tls.ext', '-out', WORK / 'signer-tls.crt')
        sources.update({'tls-cert.pem': WORK / 'signer-tls.crt', 'tls-key.pem': PRIVATE / 'signer-tls.key'})
    for name, source in sources.items():
        mode = '444' if INTEGRATED and name == 'tls-ca.pem' else '400'
        run('sudo', 'install', '-o', '65532', '-g', '65532', '-m', mode, source, STATE / 'secrets' / name)
    if INTEGRATED:
        return  # The single production lifecycle owns the first init and serve.
    admin('init')
    run('docker', 'run', '-d', '--name', CONTAINER, '--read-only', '--cap-drop=ALL',
        '--security-opt=no-new-privileges:true', '--network', 'container:' + os.environ['T07_OIDC_CONTAINER'],
        '-v', f'{STATE}/secrets:/run/secrets:ro', '-v', f'{STATE}/data:/var/lib/ocservia-signer',
        IMAGE, 'serve', '--listen', ':19444')
    health()


def import_binding():
    node, endpoint = os.environ['T07_NODE'], os.environ['T07_ENDPOINT']
    dsn = PRIVATE / 'export-dsn'
    run('sudo', 'install', '-o', '65534', '-g', '65532', '-m', '400', WORK / 'secrets/database-app-url', dsn)
    try:
        approved = run('docker', 'run', '--rm', '--read-only', '--cap-drop=ALL',
                       '--security-opt=no-new-privileges:true', '--network', 'ocservia-production_database',
                       '-v', f'{dsn}:/run/export-dsn:ro', '--entrypoint', '/usr/local/bin/ocserv-sealing-export',
                       os.environ['OCSERV_CONTROL_IMAGE'], '--database-url-file', '/run/export-dsn',
                       '--workspace', os.environ['T07_WORKSPACE'], '--node', node, '--endpoint', endpoint,
                       '--approval', (WORK / 'node-approval').read_text().strip())
    finally:
        run('sudo', 'rm', '--', dsn)
    public = run('sudo', 'python3', ROOT / 'scripts/export-node-sealing-keys.py', '--node', node,
                 '--endpoint', endpoint, '--user-key', '/etc/ocservia-agent/user-password-seal-private.pem',
                 '--user-key-id', 't07-user', '--p12-key', '/etc/ocservia-agent/p12-password-seal-private.pem',
                 '--p12-key-id', 't07-p12')
    for name, data in [('approved.json', approved), ('public.json', public)]:
        source = WORK / name
        source.write_bytes(data)
        run('sudo', 'install', '-o', '65532', '-g', '65532', '-m', '400', source, STATE / 'input' / name)
    run('docker', 'stop', CONTAINER)
    try:
        admin('import', '--approved', '/input/approved.json', '--public', '/input/public.json',
              '--workspace', os.environ['T07_WORKSPACE'], '--node', node, '--endpoint', endpoint)
    finally:
        run('docker', 'start', CONTAINER)
    health()


def export_crl():
    run('docker', 'stop', CONTAINER)
    try:
        data = admin('crl')
    finally:
        run('docker', 'start', CONTAINER)
    health()
    exported = WORK / 'issuer.crl.pem'
    exported.write_bytes(data)
    run('openssl', 'crl', '-in', exported, '-verify', '-CAfile', WORK / 'issuer.pem', '-noout')
    number = run('openssl', 'crl', '-in', exported, '-crlnumber', '-noout').decode().strip().split('=')[1]
    run('sudo', 'install', '-m', '644', exported, NODE_CRL / 'crl.next')
    run('sudo', 'mv', '-f', NODE_CRL / 'crl.next', NODE_CRL / 'crl.pem')
    assert run('sudo', 'cat', NODE_CRL / 'crl.pem') == data
    return int(number, 16)


def authenticate(control=False):
    args = ['timeout', '20', 'openconnect', '--authenticate', '--non-inter', '--protocol=anyconnect',
            '--cafile', str(WORK / 'ca.crt')]
    if control:
        args += ['--certificate', str(WORK / 'control-chain.pem'), '--sslkey', str(PRIVATE / 'control.key')]
    else:
        args += ['--certificate', str(PRIVATE / 'download.p12'),
                 '--key-password', (PRIVATE / 'p12-password').read_text()]
    result = subprocess.run([*args, 'https://localhost:44444'], capture_output=True)
    # Authentication cookies remain private, never in uploaded evidence.
    (PRIVATE / ('crl-control-auth' if control else 'crl-revoked-auth')).write_bytes(result.stdout)
    (PRIVATE / ('crl-control-stderr' if control else 'crl-revoked-stderr')).write_bytes(result.stderr)
    return result


def signer_request(path, payload, headers):
    (PRIVATE / 'signer-request').write_bytes(payload)
    token = (PRIVATE / 'certificate-signer-token').read_text().strip()
    (PRIVATE / 'signer-curl.conf').write_text('header = "Authorization: Bearer ' + token + '"\n')
    pid = run('docker', 'inspect', '--format', '{{.State.Pid}}', CONTAINER).decode().strip()
    args = []
    for header in headers:
        args += ['-H', header]
    return json.loads(run('sudo', 'nsenter', '--target', pid, '--net', 'curl', '--fail', '--silent',
                          '--show-error', '--cacert', WORK / 'ca.crt', '--config', PRIVATE / 'signer-curl.conf',
                          *args, '--data-binary', '@' + str(PRIVATE / 'signer-request'),
                          *(['--resolve', 'signer:9443:127.0.0.1'] if INTEGRATED else []), SIGNER_URL + path))


def seal():
    result = signer_request('/sign/seal', sys.stdin.buffer.read(),
                            ['Content-Type: application/octet-stream',
                             'X-Ocservia-Node-ID: ' + os.environ['T07_NODE'],
                             'X-Ocservia-Seal-Purpose: user_password'])
    print(json.dumps(dict(version=result['version'], purpose=result['purpose'],
                         key_id=result['key_id'], ciphertext=result['sealed'])))


def before():
    if NODE_CRL.exists():
        raise RuntimeError('refusing existing CRL test node')
    primary_socket = run('sudo', 'stat', '-c', '%d:%i', '/run/occtl.socket').decode().strip()
    run('sudo', 'install', '-d', '-m', '755', NODE_CRL)
    number = export_crl()
    run('openssl', 'pkcs12', '-in', PRIVATE / 'download.p12', '-passin', 'file:' + str(PRIVATE / 'p12-password'),
        '-clcerts', '-nokeys', '-out', WORK / 'revoked-leaf.pem')
    # A second valid identity distinguishes revocation enforcement from outages.
    run('openssl', 'req', '-new', '-newkey', 'rsa:2048', '-nodes', '-subj', '/CN=p2-control',
        '-keyout', PRIVATE / 'control.key', '-outform', 'DER', '-out', WORK / 'control.csr')
    cert_id = str(uuid.uuid4())
    payload = json.dumps({'certificate_id': cert_id,
                          'csr_der': base64.b64encode((WORK / 'control.csr').read_bytes()).decode()}).encode()
    # Execute the HTTP client in the provider namespace; the internal network
    # intentionally has no host gateway. Only task-issued credentials are used.
    response = signer_request('/sign', payload, ['Content-Type: application/json', 'Idempotency-Key: ' + cert_id])
    (WORK / 'control-chain.pem').write_text(response['certificate_chain_pem'])
    for name, source, mode in [('tls.key', PRIVATE / 'tls.key', '600'),
                               ('tls.crt', WORK / 'secrets/tls.crt', '644'),
                               ('ca.pem', WORK / 'issuer-chain.pem', '644')]:
        run('sudo', 'install', '-m', mode, source, NODE_CRL / name)
    config = WORK / 'crl-ocserv.conf'
    config.write_text('auth = "certificate"\ncert-user-oid = 2.5.4.3\nlisten-host = 127.0.0.1\n'
                      'tcp-port = 44444\nudp-port = 0\nrun-as-user = ocservia-vpn\nrun-as-group = ocservia-vpn\n'
                      'socket-file = /run/ocserv-p2-crl.socket\ndevice = p2crl\n'
                      'occtl-socket-file = /run/occtl-p2-crl.socket\npid-file = /run/ocserv-p2-crl.pid\n'
                      'ipv4-network = 10.209.0.0/24\nmax-clients = 8\nmax-same-clients = 4\n'
                      f'server-cert = {NODE_CRL}/tls.crt\nserver-key = {NODE_CRL}/tls.key\n'
                      f'ca-cert = {NODE_CRL}/ca.pem\ncrl = {NODE_CRL}/crl.pem\n')
    run('sudo', 'install', '-m', '600', config, NODE_CRL / 'ocserv.conf')
    run('sudo', 'ocserv', '--test-config', '-c', NODE_CRL / 'ocserv.conf')
    run('sudo', 'systemd-run', '--unit=ocservia-p2-crl', '--property=Type=simple',
        '/usr/sbin/ocserv', '-f', '-c', NODE_CRL / 'ocserv.conf')
    for _ in range(30):
        try:
            with socket.create_connection(('127.0.0.1', 44444), timeout=1):
                break
        except OSError:
            time.sleep(1)
    for control in (False, True):
        result = authenticate(control)
        assert result.returncode == 0 and b'COOKIE=' in result.stdout, 'pre-revoke certificate login failed'
    (EVIDENCE / 'crl-acceptance.json').write_text(json.dumps({'candidate_sha': os.environ['CANDIDATE_SHA'],
                                                           'before_auth': True, 'before_crl_number': number,
                                                           'primary_occtl_identity': primary_socket}))


def after():
    evidence = json.loads((EVIDENCE / 'crl-acceptance.json').read_text())
    number = export_crl()
    assert number > evidence['before_crl_number']
    verified = subprocess.run(['openssl', 'verify', '-CAfile', str(WORK / 'signer-root.pem'),
                               '-untrusted', str(WORK / 'issuer.pem'), '-CRLfile', str(WORK / 'issuer.crl.pem'),
                               '-crl_check', str(WORK / 'revoked-leaf.pem')], capture_output=True)
    assert verified.returncode != 0 and b'certificate revoked' in verified.stderr
    run('sudo', 'systemctl', 'kill', '--kill-whom=main', '--signal=HUP', 'ocservia-p2-crl')
    # SIGHUP reload is asynchronous; retry a bounded number of fresh logins.
    for _ in range(15):
        result = authenticate()
        if result.returncode not in (0, 124, 137) and any(
                word in result.stderr.lower() for word in (b'certificate', b'authentication', b'handshake', b'401')):
            break
        time.sleep(1)
    else:
        raise RuntimeError('node still accepts revoked certificate or did not respond')
    control = authenticate(True)
    assert control.returncode == 0 and b'COOKIE=' in control.stdout, 'non-revoked control login failed'
    journal = run('sudo', 'journalctl', '--no-pager', '-u', 'ocservia-p2-crl')
    assert b'revok' in journal.lower(), 'missing node revocation rejection evidence'
    run('sudo', 'systemctl', 'stop', 'ocservia-p2-crl')
    assert run('sudo', 'stat', '-c', '%d:%i', '/run/occtl.socket').decode().strip() == evidence['primary_occtl_identity']
    assert isinstance(json.loads(run('sudo', 'occtl', '--json', 'show', 'users')), list)
    evidence.update(after_crl_number=number, revoked_rejected=True, control_auth=True,
                    primary_occtl_preserved=True,
                    refresh='signed-export-atomic-install-sighup',
                    distribution='protected operator channel to disposable native node',
                    existing_session_termination='NOT_TESTED')
    (EVIDENCE / 'crl-acceptance.json').write_text(json.dumps(evidence, indent=2) + '\n')


if __name__ == '__main__':
    if os.environ.get('GITHUB_ACTIONS') != 'true' or os.environ.get('RUNNER_ENVIRONMENT') != 'github-hosted':
        raise SystemExit('disposable hosted runner required')
    {'prepare': prepare, 'import': import_binding, 'before': before, 'after': after, 'seal': seal}[sys.argv[1]]()
