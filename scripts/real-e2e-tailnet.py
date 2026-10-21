#!/usr/bin/env python3
"""Run-scoped private Relay metadata and OIDC admission (never print credentials)."""
import base64
import hashlib
import ipaddress
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import urllib.parse
import urllib.request
import uuid

SUBJECT = 'repo:GentleKingson@72605370/ocservia@1319465232:environment:cross-vm-test-relay'
WORKFLOW = 'GentleKingson/ocservia/.github/workflows/real-e2e.yml@refs/heads/main'
URL = 'https://relay.ocservia.test:3443'


def claims_valid(claims, audience, sha):
    expected = dict(iss='https://token.actions.githubusercontent.com', aud=audience,
                    sub=SUBJECT, repository='GentleKingson/ocservia',
                    repository_id='1319465232', repository_owner_id='72605370',
                    ref='refs/heads/main', workflow_ref=WORKFLOW, workflow_sha=sha,
                    environment='cross-vm-test-relay', runner_environment='github-hosted')
    if any(claims.get(k) != v for k, v in expected.items()):
        raise ValueError('OIDC scope mismatch')
    if claims.get('job_workflow_ref', WORKFLOW) != WORKFLOW:
        raise ValueError('untrusted reusable workflow')
    if claims.get('job_workflow_sha', sha) != sha:
        raise ValueError('untrusted reusable revision')


def infra():
    # These non-secret IDs/audiences and the administrator's policy attestation
    # live in the same main-only Environment as the existing Relay token.
    for role in ('PEER', 'RELAY'):
        for key in ('CLIENT_ID', 'AUDIENCE'):
            value = os.environ.get('TS_' + role + '_' + key, '')
            if not value or any(c.isspace() for c in value):
                raise ValueError('missing WIF configuration')
    if os.environ['TS_PEER_CLIENT_ID'] == os.environ['TS_RELAY_CLIENT_ID']:
        raise ValueError('separate role identities required')
    if os.environ.get('TS_POLICY_REVIEWED') != 'true':
        raise ValueError('administrator policy review required')
    token = os.environ.get('REAL_E2E_RELAY_TOKEN', '')
    if not 32 <= len(token.encode()) <= 512 or any(c.isspace() for c in token):
        raise ValueError('missing protected token')


def oidc():
    audience = os.environ['TS_AUDIENCE']
    endpoint = os.environ['ACTIONS_ID_TOKEN_REQUEST_URL']
    endpoint += ('&' if '?' in endpoint else '?') + urllib.parse.urlencode({'audience': audience})
    request = urllib.request.Request(endpoint, headers={
        'Authorization': 'Bearer ' + os.environ['ACTIONS_ID_TOKEN_REQUEST_TOKEN']})
    with urllib.request.urlopen(request, timeout=20) as response:
        token = json.load(response)['value']
    payload = token.split('.')[1]
    claims = json.loads(base64.urlsafe_b64decode(payload + '=' * (-len(payload) % 4)))
    # Obtained directly from GitHub's authenticated runner endpoint. This local
    # check is defense in depth; Tailscale verifies JWT signatures and claim rules.
    claims_valid(claims, audience, os.environ['GITHUB_SHA'])


def validate_ready(item, ca, local):
    if set(item) != {'url', 'ip', 'ca_sha256', 'source_commit', 'run_id', 'attempt', 'boot_id'}:
        raise ValueError('unexpected ready fields')
    if item['url'] != URL or ipaddress.ip_address(item['ip']) not in ipaddress.ip_network('100.64.0.0/10'):
        raise ValueError('unexpected Relay address')
    if item['ca_sha256'] != hashlib.sha256(ca.encode()).hexdigest() or 'PRIVATE KEY' in ca:
        raise ValueError('invalid public CA')
    subprocess.run(['openssl', 'x509', '-noout'], input=ca.encode(), check=True, capture_output=True)
    if str(uuid.UUID(item['boot_id'])) != item['boot_id'] or item['boot_id'] == local['boot_id']:
        raise ValueError('Relay is not a distinct VM')
    if (item['source_commit'] != local['source_commit'] or
            f"{item['run_id']}-{item['attempt']}" != local['run_id']):
        raise ValueError('mixed Relay source or run')
    if not re.fullmatch('[0-9a-f]{40}', item['source_commit']):
        raise ValueError('invalid source')


def consume(directory, environment):
    directory, environment = Path(directory), Path(environment)
    if {p.name for p in directory.iterdir()} != {'ready.json', 'ca.pem'}:
        raise ValueError('unexpected ready artifact members')
    item = json.loads((directory / 'ready.json').read_text())
    ca = (directory / 'ca.pem').read_text()
    local = json.loads(environment.read_text())
    validate_ready(item, ca, local)
    local['relay_vm'] = item
    environment.write_text(json.dumps(local, indent=2) + '\n')
    # Only validated, fixed host and CGNAT IPv4 reach hosts/Compose configuration.
    print(item['ip'])


if __name__ == '__main__':
    try:
        if sys.argv[1] == 'infra':
            infra()
        elif sys.argv[1] == 'oidc':
            oidc()
        elif sys.argv[1] == 'consume':
            consume(*sys.argv[2:])
        else:
            raise ValueError('unknown phase')
    except Exception:
        print('BLOCKED_INFRA: require protected WIF identities, exact OIDC scope, reviewed Tailnet policy and run-bound Relay readiness', file=sys.stderr)
        sys.exit(1)
