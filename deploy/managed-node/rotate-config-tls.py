#!/usr/bin/env python3
"""Activate one registered Certbot renewal on the local managed node."""
import argparse
import fcntl
import hashlib
import json
import os
from pathlib import Path
import re
import socket
import ssl
import subprocess
import tempfile
import time

ROOT = Path('/etc/ocservia-agent/config-tls')
CONFIG = Path('/etc/ocserv/ocserv.conf')
UUID7 = r'[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}'


def require(condition, message):
    if not condition:
        raise ValueError(message)


def protected(path):
    for item in (path, *path.parents):
        meta = item.lstat()
        require(not item.is_symlink() and meta.st_uid == 0 and not meta.st_mode & 0o022,
                f'unsafe ownership or permissions on {item}')


def fingerprint(cert):
    pem = subprocess.check_output(['openssl', 'x509', '-in', cert, '-outform', 'DER'])
    return hashlib.sha256(pem).hexdigest()


def config_binding(text):
    """Require a single managed certificate/key pair and optional matching client CA."""
    bindings = {}
    for match in re.finditer(r'^[ \t]*(server-cert|server-key|ca-cert)[ \t]*=[ \t]*(.*?)[ \t]*$', text, re.M):
        name, path = match.groups()
        require(name not in bindings, f'duplicate {name}')
        bindings[name] = Path(path)
    bundle = bindings['server-cert'].parent
    require(bundle.parent.parent == ROOT and re.fullmatch(UUID7, bundle.parent.name)
            and re.fullmatch(r'[A-Za-z0-9][A-Za-z0-9._-]{0,63}', bundle.name)
            and '..' not in bundle.name, 'active certificate must use an immutable managed bundle')
    for name, file in [('server-cert', 'server-cert.pem'), ('server-key', 'server-key.pem'),
                       ('ca-cert', 'ca-cert.pem')]:
        if name != 'ca-cert' or name in bindings:
            require(bindings.get(name) == bundle / file, f'unexpected {name} binding')
    return bundle, bindings


def probe_context(rollback):
    ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_CLIENT) if rollback else ssl.create_default_context()
    ctx.minimum_version = ssl.TLSVersion.TLSv1_2
    if rollback:
        ctx.check_hostname = False
        ctx.verify_mode = ssl.CERT_NONE
    return ctx


def handshake(host, port, expected, *, rollback=False):
    # Rollback may need to restore a just-expired certificate. Pin its DER hash.
    ctx = probe_context(rollback)
    deadline = time.monotonic() + 20
    while True:
        try:
            with socket.create_connection(('127.0.0.1', port), timeout=3) as sock:
                with ctx.wrap_socket(sock, server_hostname=host) as tls:
                    require(hashlib.sha256(tls.getpeercert(True)).hexdigest() == expected,
                            'ocserv handshake returned a different leaf')
            return
        except (OSError, ValueError):
            if time.monotonic() >= deadline:
                raise
            time.sleep(1)


def atomic_write(path, data):
    with tempfile.NamedTemporaryFile(dir=path.parent, prefix='.ocservia-renew-', delete=False) as file:
        tmp = Path(file.name)
        try:
            file.write(data)
            file.flush()
            os.fsync(file.fileno())
            os.replace(tmp, path)
        finally:
            tmp.unlink(missing_ok=True)
    directory = os.open(path.parent, os.O_DIRECTORY)
    try:
        os.fsync(directory)
    finally:
        os.close(directory)


def checked_spki(cert, private_key):
    public_key = subprocess.check_output(['openssl', 'x509', '-in', cert, '-pubkey', '-noout'])
    public_der = subprocess.check_output(['openssl', 'pkey', '-pubin', '-outform', 'DER'], input=public_key)
    from_key = subprocess.check_output(['openssl', 'pkey', '-in', private_key, '-pubout', '-outform', 'DER'],
                                       stdin=subprocess.DEVNULL)
    require(from_key == public_der, 'renewed certificate/private key mismatch')
    return hashlib.sha256(public_der).hexdigest()


def registered_reference(args, version, spki_hash, ca_hash):
    if args.reference:
        require(re.fullmatch(UUID7, args.reference), 'invalid reference UUID')
        return args.reference
    require(args.registrar.is_absolute() and args.registrar.is_file(), 'registrar must be an absolute file path')
    protected(args.registrar)
    key_path = '/'.join([args.node, version, spki_hash, ca_hash or 'none'])
    request = dict(provider='node-local-tls-v1', key_path=key_path, version=version,
                   reason='Certbot ocserv renewal')
    # Cache only public metadata. A retry after provisioning or restart must use
    # the same Controller-issued UUID. Serialize competing renewal hooks.
    protected(ROOT)
    receipt = ROOT / ('.renewal-' + version + '.json')
    with os.fdopen(os.open(ROOT / '.renewal.lock', os.O_RDWR | os.O_CREAT | os.O_NOFOLLOW, 0o600), 'r+') as lock:
        fcntl.flock(lock, fcntl.LOCK_EX)
        if receipt.exists():
            protected(receipt)
            response = json.loads(receipt.read_bytes())
        else:
            response = json.loads(subprocess.check_output([args.registrar],
                                  input=json.dumps(request).encode(), timeout=60))
        require(all(response.get(k) == request[k] for k in ('provider', 'key_path', 'version'))
                and response.get('state') == 'active' and re.fullmatch(UUID7, response['id']),
                'registrar returned a different or inactive reference')
        if not receipt.exists():
            atomic_write(receipt, json.dumps(response).encode())
        return response['id']


def activate(args):
    require(os.geteuid() == 0, 'run as root')
    require(re.fullmatch(UUID7, args.node), 'invalid node UUID')
    lineage = Path(os.environ['RENEWED_LINEAGE'])
    protected(lineage)
    cert, key, chain = [(lineage / f).resolve(strict=True) for f in
                        ('fullchain.pem', 'privkey.pem', 'chain.pem')]
    for file in (cert, key, chain):
        protected(file)
    subprocess.run(['openssl', 'verify', '-purpose', 'sslserver', '-verify_hostname',
                    args.hostname, '-untrusted', chain, cert], check=True)
    version = fingerprint(cert)
    ca_hash = fingerprint(args.client_ca) if args.client_ca else ''
    spki_hash = checked_spki(cert, key)
    reference = registered_reference(args, version, spki_hash, ca_hash)
    target = ROOT / reference / version
    if not target.exists():
        provision = Path(__file__).resolve().with_name('provision-config-tls.sh')
        subprocess.run(['bash', provision, args.node, reference, version, cert, key,
                        *([args.client_ca] if args.client_ca else [])], check=True)
    protected(CONFIG)
    require(CONFIG.is_file() and CONFIG.stat().st_mode & 0o777 == 0o600, 'expected 0600 ocserv.conf')
    lock = CONFIG.parent / '.ocservia-config.lock'
    with os.fdopen(os.open(lock, os.O_RDWR | os.O_CREAT | os.O_NOFOLLOW, 0o600), 'r+') as handle:
        protected(lock)
        fcntl.flock(handle, fcntl.LOCK_EX)
        pending = CONFIG.parent / '.ocservia-renewal.pending'
        current = CONFIG.read_text()
        original = current
        if pending.exists():
            protected(pending)
            recovery = json.loads(pending.read_bytes())
            require(recovery['target'] == str(target), 'finish the interrupted renewal first')
            original = recovery['original']
        active, bindings = config_binding(original)
        protected(active)
        previous = json.loads((active / 'manifest.json').read_text())
        require(previous['node_id'] == args.node, 'active reference belongs to another node')
        require(ca_hash == previous['ca_sha256'] and bool(ca_hash) == ('ca-cert' in bindings),
                'renewal must preserve the client CA')
        protected(target)
        manifest = json.loads((target / 'manifest.json').read_text())
        require(manifest['node_id'] == args.node and manifest['secret_ref_id'] == reference
                and manifest['version'] == version and manifest['certificate_sha256'] == version
                and manifest['ca_sha256'] == ca_hash and manifest['spki_sha256'] == spki_hash
                and fingerprint(target / 'server-cert.pem') == version,
                'staged TLS bundle does not match the renewal')
        if active == target:
            handshake(args.hostname, args.port, version)
            return
        candidate = re.sub(r'^([ \t]*(?:server-cert|server-key|ca-cert)[ \t]*=[ \t]*)'
                           + re.escape(str(active)) + '/', lambda m: m[1] + str(target) + '/',
                           original, flags=re.M)
        require(config_binding(candidate)[0] == target, 'candidate TLS paths are incomplete')
        require(current in (original, candidate), 'ocserv configuration changed during renewal recovery')
        with tempfile.NamedTemporaryFile(dir=CONFIG.parent, prefix='.ocservia-renew-', mode='w') as staged:
            staged.write(candidate)
            staged.flush()
            subprocess.run(['ocserv', '-t', '-c', staged.name], check=True)
        if not pending.exists():
            atomic_write(pending, json.dumps(dict(original=original, target=str(target))).encode())
        try:
            atomic_write(CONFIG, candidate.encode())
            subprocess.run(['systemctl', 'restart', 'ocserv.service'], check=True)
            handshake(args.hostname, args.port, version)
        except BaseException:
            atomic_write(CONFIG, original.encode())
            subprocess.run(['systemctl', 'restart', 'ocserv.service'], check=True)
            handshake(args.hostname, args.port, previous['certificate_sha256'], rollback=True)
            pending.unlink()
            raise
        pending.unlink()


if __name__ == '__main__':
    os.umask(0o077)
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('node')
    reference = parser.add_mutually_exclusive_group(required=True)
    reference.add_argument('--reference', help='existing Controller UUID for this renewed leaf')
    reference.add_argument('--registrar', type=Path, help='root-owned executable accepting and returning public Controller JSON')
    parser.add_argument('hostname')
    parser.add_argument('port', type=int)
    parser.add_argument('--client-ca', type=Path)
    activate(parser.parse_args())
