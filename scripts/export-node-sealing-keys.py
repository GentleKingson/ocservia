#!/usr/bin/env python3
"""Run on the node as root; export only purpose-specific public SPKI material."""
import argparse
import base64
import hashlib
import json
import os
from pathlib import Path
import stat
import subprocess
import uuid


def public_key(filename):
    path = Path(filename)
    if not path.is_absolute() or '..' in path.parts:
        raise ValueError('absolute protected key path required')
    for directory in path.parents:
        info = directory.lstat()
        if not stat.S_ISDIR(info.st_mode) or info.st_uid != 0 or info.st_mode & 0o022:
            raise ValueError('unprotected key ancestry')
    info = path.lstat()
    if (not stat.S_ISREG(info.st_mode) or info.st_uid != 0 or
            info.st_nlink != 1 or stat.S_IMODE(info.st_mode) not in (0o400, 0o600)):
        raise ValueError('unprotected private key')
    return subprocess.run(['/usr/bin/openssl', 'rsa', '-in', str(path),
                           '-pubout', '-outform', 'DER'], check=True,
                          stdout=subprocess.PIPE, stderr=subprocess.DEVNULL).stdout


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--node', required=True)
    parser.add_argument('--endpoint', required=True)
    for purpose in ('user', 'p12'):
        parser.add_argument(f'--{purpose}-key', required=True)
        parser.add_argument(f'--{purpose}-key-id', required=True)
    args = parser.parse_args()
    if os.geteuid() != 0 or str(uuid.UUID(args.node)) != args.node or len(bytes.fromhex(args.endpoint)) != 32:
        raise ValueError('invalid node identity or operator')
    keys = []
    for prefix, purpose in [('user', 'user_password'), ('p12', 'certificate_p12_password')]:
        der = public_key(getattr(args, prefix + '_key'))
        key_id = getattr(args, prefix + '_key_id')
        if not 1 <= len(key_id) <= 128:
            raise ValueError('invalid key ID')
        keys.append(dict(purpose=purpose, version=1, key_id=key_id,
                         public_key_sha256=hashlib.sha256(der).hexdigest(),
                         public_key_der=base64.b64encode(der).decode()))
    if keys[0]['key_id'] == keys[1]['key_id'] or keys[0]['public_key_sha256'] == keys[1]['public_key_sha256']:
        raise ValueError('distinct purpose keys required')
    print(json.dumps(dict(node_id=args.node, endpoint_id=args.endpoint, keys=keys)))


if __name__ == '__main__':
    try:
        main()
    except (ValueError, OSError, subprocess.CalledProcessError):
        raise SystemExit('node public-key export rejected')
