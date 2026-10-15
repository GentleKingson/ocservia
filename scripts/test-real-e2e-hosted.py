#!/usr/bin/env python3
"""Local contract/negative checks; never a Cross-VM acceptance verdict."""
import importlib.util
import os
from pathlib import Path
import re
import subprocess
import tempfile
import uuid
import yaml

ROOT = Path(__file__).resolve().parent.parent
spec = importlib.util.spec_from_file_location('evidence', ROOT / 'scripts/real-e2e-evidence.py')
evidence = importlib.util.module_from_spec(spec)
spec.loader.exec_module(evidence)
local = dict(role='controller', boot_id=str(uuid.uuid4()), hostname='controller',
             runner_environment='github-hosted', runner_os='ubuntu-24.04', pid1='systemd',
             default_routes=[{'dev':'eth0'}], network_namespace='net:[1]', source_commit='a'*40)
peer = {**local, 'role':'node', 'boot_id':str(uuid.uuid4()), 'hostname':'agent'}
evidence.validate(local, peer)
for key, value in [('boot_id',local['boot_id']), ('hostname','controller'), ('role','controller'),
                   ('source_commit','b'*40), ('runner_environment','self-hosted'), ('default_routes',[])]:
    bad = {**peer, key:value}
    try:
        evidence.validate(local, bad)
    except ValueError:
        pass
    else:
        raise AssertionError('accepted invalid ' + key)

workflow = yaml.safe_load((ROOT / '.github/workflows/real-e2e.yml').read_text())
assert set(workflow[True]) == {'workflow_dispatch'}
assert workflow['permissions'] == {'contents':'read', 'actions':'read'}
for role in ('controller', 'node'):
    job = workflow['jobs'][role]
    assert job['runs-on'] == 'ubuntu-24.04' and 'needs' not in job and 'container' not in job
    assert job['timeout-minutes'] <= 60
    uploads = [step for step in job['steps'] if step.get('uses','').startswith('actions/upload-artifact@')]
    assert all(step['with']['if-no-files-found'] == 'error' for step in uploads)
    for step in job['steps']:
        if 'uses' in step:
            assert re.fullmatch(r'[^@]+@[a-f0-9]{40}', step['uses'])
    cleanup = next(step for step in job['steps'] if step.get('name') == 'Clean private files and task resources')
    assert cleanup['if'] == 'always()' and cleanup['timeout-minutes'] == 5
    if role == 'controller':
        token = next(step for step in uploads if step.get('name') == 'Publish enrollment-token')
        assert token['with']['path'].endswith('/encrypted-token')
assert workflow['jobs']['verdict']['needs'] == ['controller', 'node']

with tempfile.TemporaryDirectory(prefix='real-e2e-test-') as directory:
    temp = Path(directory)
    def run(*args, ok=True, env=None):
        result = subprocess.run(args, cwd=ROOT, env=env, capture_output=True)
        assert (result.returncode == 0) == ok, (args, result.stderr.decode())
        return result
    # Execute the actual wrapper's encryption and decryption phases, mocking
    # only the role scripts so no services, identities or VM claims are invented.
    fixture = temp / 'repo'
    scripts = fixture / 'scripts'
    scripts.mkdir(parents=True)
    for name in ('real-e2e-hosted.sh', 'real-e2e-evidence.py'):
        (scripts / name).write_bytes((ROOT / 'scripts' / name).read_bytes())
    (scripts / 'real-e2e-controller.sh').write_text('''#!/bin/bash
set -euo pipefail
work="$RUNNER_TEMP/ocservia-real-e2e-controller-$RUN_ID"
mkdir -p "$work/outbox/enrollment-token"
printf '%043d' 1 >"$work/outbox/enrollment-token/enrollment-token"
''')
    (scripts / 'real-e2e-node.sh').write_text('''#!/bin/bash
set -euo pipefail
case "$1" in
prepare) mkdir -p "$RUNNER_TEMP/ocservia-real-e2e-node-$RUN_ID/outbox/agent-endpoint" ;;
enroll) test "$(cat "$2/enrollment-token")" = "$(printf '%043d' 1)" ;;
cleanup) rm -rf "$RUNNER_TEMP/ocservia-real-e2e-node-$RUN_ID" "$RUNNER_TEMP/ocservia-real-e2e-target-$RUN_ID" ;;
esac
''')
    env = {**os.environ, 'RUN_ID':'4242-1','GITHUB_RUN_ID':'4242','GITHUB_RUN_ATTEMPT':'1',
           'RUNNER_TEMP':str(temp), 'REAL_E2E_ROLE':'node'}
    for role, item in [('controller',local), ('node',peer)]:
        import json
        output = temp / f'real-e2e-evidence-{role}'
        output.mkdir()
        (output / 'environment.json').write_text(json.dumps(item))
    received = temp / 'real-e2e-private-4242-1-node/controller-ready'
    received.mkdir(parents=True)
    (received / 'environment.json').write_bytes((temp / 'real-e2e-evidence-controller/environment.json').read_bytes())
    wrapper = str(scripts / 'real-e2e-hosted.sh')
    run('bash', wrapper, 'prepare', env=env)
    import shutil
    shutil.copytree(temp / 'ocservia-real-e2e-node-4242-1/outbox/agent-endpoint',
                    temp / 'real-e2e-private-4242-1-controller/agent-endpoint')
    run('bash', wrapper, 'issue-token', env={**env, 'REAL_E2E_ROLE':'controller'})
    encrypted = temp / 'real-e2e-private-4242-1-controller/encrypted-token'
    assert sorted(p.name for p in encrypted.iterdir()) == ['token.oaep']
    assert b'0000000000000000000000000000000000000000001' not in (encrypted / 'token.oaep').read_bytes()
    shutil.copytree(encrypted, temp / 'real-e2e-private-4242-1-node/encrypted-token')
    cipher = temp / 'real-e2e-private-4242-1-node/encrypted-token/token.oaep'
    original = cipher.read_bytes()
    cipher.write_bytes(b'bad ciphertext')
    run('bash', wrapper, 'enroll', env=env, ok=False)
    cipher.write_bytes(original)
    run('bash', wrapper, 'enroll', env=env)
    assert not (temp / 'real-e2e-private-4242-1-node/exchange.key').exists()
    run('bash', wrapper, 'cleanup', env=env)
    assert not (temp / 'real-e2e-private-4242-1-node').exists()
    assert (temp / 'real-e2e-evidence-node/cleanup.txt').read_text() == 'CLEANUP=PASS\n'
print('hosted workflow, peer rejection, real OAEP round-trip/corruption and cleanup checks passed')
