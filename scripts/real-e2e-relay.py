#!/usr/bin/env python3
"""Validate explicit test-Relay configuration without disclosing credentials."""
import hashlib
import json
import os
from pathlib import Path
import subprocess
import sys
from urllib.parse import urlsplit


def configuration():
    values = [os.environ.get('REAL_E2E_RELAY_' + key, '') for key in ('URL', 'CA', 'TOKEN')]
    if not all(values):
        raise ValueError('BLOCKED_INFRA: configure CROSS_VM_RELAY_URL, CROSS_VM_RELAY_CA and CROSS_VM_RELAY_TOKEN')
    url, ca, token = values
    parsed = urlsplit(url)
    if (parsed.scheme != 'https' or not parsed.hostname or parsed.username is not None
            or parsed.password is not None or parsed.query or parsed.fragment
            or parsed.path not in ('', '/') or '?' in url or '#' in url
            or any(c.isspace() for c in url)):
        raise ValueError('BLOCKED_INFRA: one credential-free HTTPS Relay URL is required')
    if parsed.port is not None and not 1 <= parsed.port <= 65535:
        raise ValueError('BLOCKED_INFRA: invalid Relay port')
    if not 32 <= len(token.encode()) <= 512 or any(c.isspace() for c in token):
        raise ValueError('BLOCKED_INFRA: Relay token must contain 32..512 non-whitespace bytes')
    result = subprocess.run(['openssl', 'x509', '-noout'], input=ca.encode(), capture_output=True)
    if result.returncode or 'PRIVATE KEY' in ca:
        raise ValueError('BLOCKED_INFRA: Relay CA must be a PEM certificate without private keys')
    return url.rstrip('/'), ca, token


if __name__ == '__main__':
    try:
        url, ca, token = configuration()
        if len(sys.argv) == 2:
            directory = Path(sys.argv[1])
            directory.mkdir(mode=0o700, parents=True, exist_ok=False)
            for name, value in [('url', url), ('ca.pem', ca), ('token', token)]:
                path = directory / name
                path.write_text(value)
                path.chmod(0o600)
            (directory / 'metadata.json').write_text(json.dumps({
                'url': url, 'ca_sha256': hashlib.sha256(ca.encode()).hexdigest(),
            }) + '\n')
        elif len(sys.argv) != 1:
            raise ValueError('usage: real-e2e-relay.py [NEW_PRIVATE_DIRECTORY]')
    except (ValueError, OSError):
        # Parsing exceptions can contain the offending URL or other secret input.
        print('BLOCKED_INFRA: explicit HTTPS test Relay, PEM CA and protected token configuration required', file=sys.stderr)
        sys.exit(1)
