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
             default_routes=[{'dev':'eth0'}], network_namespace='net:[1]', source_commit='a'*40,
             runner_arch='x86_64', run_id='4242-1',
             relay={'url':'https://relay.e2e.test', 'ca_sha256':'c'*64})
peer = {**local, 'role':'node', 'boot_id':str(uuid.uuid4()), 'hostname':'agent'}
evidence.validate(local, peer)
evidence.validate(local, {**peer, 'hostname':local['hostname']})
for key, value in [('boot_id',local['boot_id']), ('role','controller'),
                   ('source_commit','b'*40), ('runner_environment','self-hosted'), ('default_routes',[]),
                   ('source_commit',''), ('runner_arch','aarch64'), ('run_id','4242-2'),
                   ('relay', {**peer['relay'], 'ca_sha256':'d'*64})]:
    bad = {**peer, key:value}
    try:
        evidence.validate(local, bad)
    except ValueError:
        pass
    else:
        raise AssertionError('accepted invalid ' + key)

workflow = yaml.safe_load((ROOT / '.github/workflows/real-e2e.yml').read_text())
assert set(workflow[True]) == {'workflow_dispatch', 'workflow_call', 'schedule'}
assert workflow[True]['schedule'] == [{'cron':'17 3 * * *'}]
assert set(workflow[True]['workflow_call']['secrets']) == {
    'CROSS_VM_RELAY_TOKEN'}
assert 'secrets: inherit' not in (ROOT / '.github/workflows/real-e2e.yml').read_text()
assert workflow['permissions'] == {'contents':'read', 'actions':'read'}
preflight = workflow['jobs']['preflight']
assert preflight['needs'] == 'admission' and preflight['if'] == "needs.admission.outputs.mode == 'run'"
admission = workflow['jobs']['admission']
assert admission['permissions'] == {} and 'environment' not in admission and 'secrets.' not in str(admission)
assert admission['outputs'] == {'mode':'${{ steps.trust.outputs.mode }}'}
trust = admission['steps'][0]
assert trust['env']['SCHEDULE_ENABLED'] == 'false'
assert trust['name'] == 'Require trusted main workflow before repository code'
assert 'uses' not in trust and 'scripts/' not in trust['run']
assert set(trust['env']) == {'SOURCE_REPOSITORY', 'SOURCE_REF', 'SOURCE_SHA', 'JOB_CONTEXT', 'EVENT_NAME', 'SCHEDULE_ENABLED'}
for role in ('preflight', 'relay', 'controller', 'node'):
    assert workflow['jobs'][role]['environment'] == {'name':'cross-vm-test-relay', 'deployment':False}
assert 'environment' not in workflow['jobs']['verdict']
assert 'secrets.' not in str(workflow['jobs']['verdict'])
for role in ('controller', 'node'):
    job = workflow['jobs'][role]
    assert job['runs-on'] == 'ubuntu-24.04' and job['needs'] == 'preflight' and 'container' not in job
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
assert workflow['jobs']['verdict']['needs'] == ['admission', 'preflight', 'relay', 'controller', 'node']
compose = yaml.safe_load((ROOT / 'deploy/real-e2e/controller.compose.yaml').read_text())
for name in ('transportd', 'transport-endpoint-bootstrap'):
    command = compose['services'][name]['command']
    assert command[command.index('--relay-mode') + 1] == 'custom'
    assert command.count('--relay-url') == 1
    assert '--relay-token-file' in command and '--relay-ca-file' in command

with tempfile.TemporaryDirectory(prefix='real-e2e-test-') as directory:
    temp = Path(directory)
    def run(*args, ok=True, env=None):
        result = subprocess.run(args, cwd=ROOT, env=env, capture_output=True)
        assert (result.returncode == 0) == ok, (args, result.stderr.decode())
        return result
    # No network is involved: malformed or absent infrastructure must fail
    # before either expensive VM role runs, without reflecting secret input.
    relay_helper = str(ROOT / 'scripts/real-e2e-relay.py')
    config_env = {key:value for key,value in os.environ.items() if not key.startswith('REAL_E2E_RELAY_')}
    missing = run('python3', relay_helper, ok=False, env=config_env)
    assert b'BLOCKED_INFRA' in missing.stderr
    run('openssl', 'req', '-x509', '-newkey', 'rsa:2048', '-nodes', '-days', '1',
        '-subj', '/CN=local-test-ca', '-keyout', str(temp / 'ca.key'), '-out', str(temp / 'ca.pem'))
    config_env.update(REAL_E2E_RELAY_URL='https://relay.e2e.test:3443',
                      REAL_E2E_RELAY_CA=(temp / 'ca.pem').read_text(), REAL_E2E_RELAY_TOKEN='t'*32)
    configured = temp / 'configured'
    run('python3', relay_helper, str(configured), env=config_env)
    assert (configured / 'token').stat().st_mode & 0o777 == 0o600
    assert (configured / 'token').read_text() == 't'*32
    for key, value in [('URL','http://relay.e2e.test'), ('URL','https://private-token@relay.e2e.test'),
                       ('URL','https://relay.e2e.test:invalid'), ('URL','https://relay.e2e.test?a=secret'),
                       ('URL','https://relay.e2e.test/#'), ('TOKEN',''), ('TOKEN','t'*32+'\n'),
                       ('CA','invalid private CA')]:
        result = run('python3', relay_helper, ok=False,
                     env={**config_env, 'REAL_E2E_RELAY_' + key:value})
        assert value.encode() not in result.stderr if value else b'BLOCKED_INFRA' in result.stderr
    # Execute the final verdict shell with success, failure, cancellation,
    # skipped jobs and missing/extra keys, including cleanup-step failures
    # represented by their enclosing role's job result.
    import json
    verdict = workflow['jobs']['verdict']['steps'][-1]['run']
    results = {name:{'result':'success'} for name in ('admission', 'controller', 'node', 'preflight', 'relay')}
    results['admission']['outputs'] = {'mode':'run'}
    trusted_env = {**os.environ, 'SOURCE_REPOSITORY':'GentleKingson/ocservia',
                   'SOURCE_REF':'refs/heads/main', 'SOURCE_SHA':'a'*40,
                   'JOB_CONTEXT':json.dumps({'workflow_ref':'GentleKingson/ocservia/.github/workflows/real-e2e.yml@refs/heads/main', 'workflow_sha':'a'*40}),
                   'EVENT_NAME':'workflow_dispatch',
                   'SCHEDULE_ENABLED':''}
    def trust_check(changes, ok, mode='run'):
        summary = temp / 'trust-summary'
        summary.write_text('')
        output = temp / 'trust-output'
        output.write_text('')
        run('bash', '-euo', 'pipefail', '-c', trust['run'], ok=ok,
            env={**trusted_env, **changes, 'GITHUB_STEP_SUMMARY':str(summary), 'GITHUB_OUTPUT':str(output)})
        assert ('BLOCKED_AUTH' in summary.read_text()) != ok
        assert output.read_text() == ('mode=' + mode + '\n' if ok else '')
    trust_check({}, True)
    for key, value in [('SOURCE_REF','refs/heads/feature'), ('SOURCE_REF','refs/tags/main'),
                       ('SOURCE_REPOSITORY','fork/ocservia'), ('JOB_CONTEXT',trusted_env['JOB_CONTEXT'].replace('heads/main','heads/feature')),
                       ('JOB_CONTEXT',trusted_env['JOB_CONTEXT'].replace('a'*40, 'b'*40)),
                       ('JOB_CONTEXT','{}'), ('JOB_CONTEXT','null'), ('JOB_CONTEXT','invalid JSON'), ('SOURCE_SHA',''),
                       ('EVENT_NAME','pull_request_target'), ('EVENT_NAME','workflow_run')]:
        trust_check({key:value}, False)
    for enabled in ('', 'false', 'TRUE', 'True', 'unexpected'):
        trust_check({'EVENT_NAME':'schedule', 'SCHEDULE_ENABLED':enabled}, True, mode='disabled')
    trust_check({'EVENT_NAME':'schedule', 'SCHEDULE_ENABLED':'true'}, True)
    trust_check({'EVENT_NAME':'push', 'SCHEDULE_ENABLED':'false'}, True)
    def verdict_check(items, ok, changes=None, acceptance=None):
        summary = temp / 'verdict-summary'
        summary.write_text('')
        run('bash', '-euo', 'pipefail', '-c', verdict, ok=ok,
            env={**trusted_env, **(changes or {}), 'RESULTS':json.dumps(items), 'GITHUB_STEP_SUMMARY':str(summary)})
        assert ('CROSS_VM_ENROLLMENT=PASS' in summary.read_text()) == (ok if acceptance is None else acceptance)
    verdict_check(results, True)
    for name in results:
        for status in ('failure', 'cancelled', 'skipped'):
            verdict_check({**results, name:{'result':status}}, False)
        verdict_check({key:value for key,value in results.items() if key != name}, False)
    verdict_check({**results, 'extra':{'result':'success'}}, False)
    disabled = {name:{'result':'skipped'} for name in ('controller', 'node', 'preflight', 'relay')}
    disabled['admission'] = {'result':'success', 'outputs':{'mode':'disabled'}}
    verdict_check(disabled, True, {'EVENT_NAME':'schedule'}, acceptance=False)
    verdict_check(results, True, {'EVENT_NAME':'schedule'})
    verdict_check({**disabled, 'preflight':{'result':'success'}}, False, {'EVENT_NAME':'schedule'})
    verdict_check({**disabled, 'admission':results['admission']}, False, {'EVENT_NAME':'schedule'})
    verdict_check(disabled, False)
    # Execute the actual wrapper's encryption and decryption phases, mocking
    # only the role scripts so no services, identities or VM claims are invented.
    fixture = temp / 'repo'
    scripts = fixture / 'scripts'
    scripts.mkdir(parents=True)
    for name in ('real-e2e-hosted.sh', 'real-e2e-evidence.py', 'real-e2e-relay.py'):
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
cleanup)
  test "${CLEANUP_STATUS:-0}" = 0
  rm -rf "$RUNNER_TEMP/ocservia-real-e2e-node-$RUN_ID" "$RUNNER_TEMP/ocservia-real-e2e-target-$RUN_ID" ;;
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
    run('bash', wrapper, 'configure-relay', env={**env, **{
        key:value for key,value in config_env.items() if key.startswith('REAL_E2E_RELAY_')}})
    assert json.loads((temp / 'real-e2e-evidence-node/environment.json').read_text())['relay']['url'] == config_env['REAL_E2E_RELAY_URL']
    # Wrapper tests with controlled network-probe results. A curl timeout or
    # DNS error must never stand in for certificate rejection.
    probe = temp / 'ocservia-real-e2e-target-4242-1/release/ocservia-relay-network-probe'
    probe.parent.mkdir(parents=True)
    probe.write_text('#!/bin/bash\nexit "${PROBE_STATUS:-0}"\n')
    probe.chmod(0o755)
    curl_bin = temp / 'curl-bin'
    curl_bin.mkdir()
    (curl_bin / 'curl').write_text('''#!/bin/bash
for argument in "$@"; do
  if [[ "$argument" == */wrong-ca.pem ]]; then exit "${WRONG_CA_STATUS:-60}"; fi
done
exit 0
''')
    (curl_bin / 'curl').chmod(0o755)
    probe_env = {**env, 'PATH':str(curl_bin)+os.pathsep+os.environ['PATH']}
    relay_evidence = temp / 'real-e2e-evidence-node/relay.txt'
    for status in ('0', '6', '28'):
        run('bash', wrapper, 'relay-check', env={**probe_env, 'WRONG_CA_STATUS':status}, ok=False)
        assert not relay_evidence.exists()
    run('bash', wrapper, 'relay-check', env={**probe_env, 'PROBE_STATUS':'1'}, ok=False)
    assert not relay_evidence.exists()
    run('bash', wrapper, 'relay-check', env=probe_env)
    assert 'WRONG_TOKEN_REJECTED=PASS' in relay_evidence.read_text()
    # Restore matching synthetic peer metadata after configuration exercise.
    (temp / 'real-e2e-evidence-node/environment.json').write_text(json.dumps(peer))
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
    run('bash', wrapper, 'cleanup', env={**env, 'CLEANUP_STATUS':'1'}, ok=False)
    assert not (temp / 'real-e2e-evidence-node/cleanup.txt').exists()
    run('bash', wrapper, 'cleanup', env=env)
    assert not (temp / 'real-e2e-private-4242-1-node').exists()
    assert (temp / 'real-e2e-evidence-node/cleanup.txt').read_text() == 'CLEANUP=PASS\n'
    # Drive the real verifier against controlled SQL responses. A consumed
    # token mismatch must fail even when the node's Pending binding is correct.
    state = temp / 'ocservia-real-e2e-controller-4242-1/state'
    state.mkdir(parents=True)
    relay = state.parent / 'relay'
    relay.mkdir()
    (relay / 'url').write_text('https://relay.e2e.test')
    endpoint = 'a' * 64
    node = '019d0000-0000-7000-8000-000000000002'
    token = '019d0000-0000-7000-8000-000000000003'
    for name, value in [('controller-endpoint-id','b'*64), ('agent-endpoint-id',endpoint),
                        ('token-id',token), ('agent-runner-instance','peer-boot')]:
        (state / name).write_text(value + '\n')
    result = temp / 'result'
    result.mkdir()
    (result / 'node-id').write_text(node + '\n')
    (result / 'runner-instance').write_text('peer-boot\n')
    binary = temp / 'bin'
    binary.mkdir()
    (binary / 'docker').write_text('''#!/bin/bash
set -euo pipefail
case "${!#}" in
  "SELECT n.id"*) printf '%s\\n' "$DB_NODE" ;;
  "SELECT consumed_node_id"*) printf '%s\\n' "$DB_TOKEN" ;;
  *) exit 2 ;;
esac
''')
    (binary / 'docker').chmod(0o755)
    verified = temp / 'verification'
    for db_node, db_token, ok in [(node+':pending:'+endpoint,node+':'+endpoint,True),
                                  (node+':active:'+endpoint,node+':'+endpoint,False),
                                  (node+':pending:'+endpoint,'',False),
                                  (node+':pending:'+endpoint,node+':'+'c'*64,False)]:
        shutil.rmtree(verified, ignore_errors=True)
        verify_env = {**env, 'REAL_E2E_ROLE':'controller', 'COMPOSE_PROJECT':'real-e2e-test',
                      'ARTIFACT_DIR':str(verified), 'PATH':str(binary)+os.pathsep+os.environ['PATH'],
                      'DB_NODE':db_node, 'DB_TOKEN':db_token}
        run('bash', str(ROOT / 'scripts/real-e2e-controller.sh'), 'verify-enrollment', str(result),
            env=verify_env, ok=ok)
        assert (verified / 'result.txt').exists() == ok
print('hosted workflow, Relay configuration/negative controls, fail-closed verdict, peer rejection, OAEP, binding and cleanup checks passed (local contracts only)')

# Three-VM additions use the existing local harness, never an OIDC request.
spec = importlib.util.spec_from_file_location('tailnet', ROOT / 'scripts/real-e2e-tailnet.py')
tailnet = importlib.util.module_from_spec(spec)
spec.loader.exec_module(tailnet)
claims = dict(iss='https://token.actions.githubusercontent.com', aud='test-audience',
              sub=tailnet.SUBJECT, repository='GentleKingson/ocservia',
              repository_id='1319465232', repository_owner_id='72605370',
              ref='refs/heads/main', workflow_ref=tailnet.WORKFLOW, workflow_sha='a'*40,
              environment='cross-vm-test-relay', runner_environment='github-hosted')
tailnet.claims_valid(claims, 'test-audience', 'a'*40)
for key in claims:
    for value in ('wrong', None):
        try:
            tailnet.claims_valid({**claims, key:value}, 'test-audience', 'a'*40)
        except ValueError:
            pass
        else:
            raise AssertionError('accepted wrong OIDC '+key)
for key in ('job_workflow_ref', 'job_workflow_sha'):
    try:
        tailnet.claims_valid({**claims, key:'wrong'}, 'test-audience', 'a'*40)
    except ValueError:
        pass
    else:
        raise AssertionError('accepted untrusted callee')
for role in ('relay', 'controller', 'node'):
    job = workflow['jobs'][role]
    assert job['permissions'] == {'contents':'read', 'actions':'read', 'id-token':'write'}
    join = next(step for step in job['steps'] if step.get('id') == 'tailnet')
    assert join['uses'] == 'tailscale/github-action@780049a30b6ff5c378a9e7b389d15ece7a204888'
    assert not {'authkey', 'oauth-secret'} & join['with'].keys()
    assert join['with']['tags'] == 'tag:ocservia-ci-' + ('relay' if role == 'relay' else 'peer')
    assert job['needs'] == 'preflight'
    assert next(step for step in job['steps'] if step.get('name') == 'Logout local Tailnet identity')['if'] == 'always()'

with tempfile.TemporaryDirectory(prefix='real-e2e-tailnet-test-') as directory:
    import hashlib, json
    temp = Path(directory)
    run('openssl', 'req', '-x509', '-newkey', 'rsa:2048', '-nodes', '-days', '1',
        '-subj', '/CN=local-test-ca', '-keyout', str(temp/'ca.key'), '-out', str(temp/'ca.pem'))
    ca = (temp/'ca.pem').read_text()
    ready = dict(url=tailnet.URL, ip='100.64.0.1', ca_sha256=hashlib.sha256(ca.encode()).hexdigest(),
                 source_commit=local['source_commit'], run_id='4242', attempt='1', boot_id=str(uuid.uuid4()))
    tailnet.validate_ready(ready, ca, local)
    for key,value in [('url','https://public.example'), ('ip','127.0.0.1'), ('ip','::1'),
                      ('ca_sha256','0'*64), ('source_commit','b'*40), ('run_id','3'),
                      ('attempt','2'), ('boot_id',local['boot_id']), ('token','must-not-be-published')]:
        try:
            tailnet.validate_ready({**ready,key:value}, ca, local)
        except (ValueError, TypeError):
            pass
        else:
            raise AssertionError('accepted wrong ready '+key)
    evidence.validate({**local,'relay_vm':ready}, {**peer,'relay_vm':ready})
    for bad in ({**ready,'boot_id':peer['boot_id']}, {**ready,'source_commit':'b'*40}):
        try:
            evidence.validate({**local,'relay_vm':bad}, {**peer,'relay_vm':bad})
        except ValueError:
            pass
        else:
            raise AssertionError('accepted non-independent Relay')
    env = {**os.environ, 'REAL_E2E_RELAY_TOKEN':'t'*32, 'TS_POLICY_REVIEWED':'true'}
    for role in ('PEER','RELAY'):
        env['TS_'+role+'_CLIENT_ID'] = 'test-client-' + role
        env['TS_'+role+'_AUDIENCE'] = 'test-audience'
    helper = str(ROOT/'scripts/real-e2e-tailnet.py')
    run('python3',helper,'infra',env=env)
    run('python3',helper,'infra',env={**env,'TS_PEER_CLIENT_ID':env['TS_RELAY_CLIENT_ID']},ok=False)
    for key in ('TS_POLICY_REVIEWED','TS_PEER_CLIENT_ID','TS_RELAY_AUDIENCE','REAL_E2E_RELAY_TOKEN'):
        run('python3',helper,'infra',env={**env,key:''},ok=False)
    missing = temp/'missing-ready'
    missing.mkdir()
    local_file=temp/'environment.json'
    local_file.write_text(json.dumps(local))
    run('python3',helper,'consume',str(missing),str(local_file),ok=False)
    (missing/'ready.json').write_text(json.dumps(ready))
    (missing/'ca.pem').write_text(ca)
    run('python3',helper,'consume',str(missing),str(local_file))
    (missing/'secret').write_text('not-allowed')
    run('python3',helper,'consume',str(missing),str(local_file),ok=False)

    # Exercise the actual Relay lifetime/cleanup shell with bounded local shims.
    shim = temp/'bin'
    shim.mkdir()
    def executable(name, body):
        path=shim/name
        path.write_text('#!/bin/bash\nset -eu\n'+body)
        path.chmod(0o755)
    executable('docker', 'if [[ "$1" == inspect ]]; then echo "${RELAY_RUNNING:-true}"; else exit "${DOCKER_STATUS:-0}"; fi\n')
    executable('curl', 'cat "$JOBS_FIXTURE"\n')
    executable('sleep', 'cp "$COMPLETED_FIXTURE" "$JOBS_FIXTURE"\n')
    ready_dir=temp/'real-e2e-relay-4242-1/ready'
    ready_dir.mkdir(parents=True)
    (ready_dir/'ready.json').write_text(json.dumps(ready))
    jobs=temp/'jobs.json'
    completed=temp/'completed.json'
    success=[dict(name=n,status='completed',conclusion='success') for n in ('Cross-VM Controller','Cross-VM Agent')]
    completed.write_text(json.dumps({'jobs':success}))
    env={**os.environ,'PATH':str(shim)+os.pathsep+os.environ['PATH'],'RUNNER_TEMP':str(temp),
         'RUN_ID':'4242-1','GITHUB_RUN_ID':'4242','GITHUB_RUN_ATTEMPT':'1','GITHUB_TOKEN':'local-test',
         'GITHUB_API_URL':'https://api.github.com','GITHUB_REPOSITORY':'GentleKingson/ocservia',
         'JOBS_FIXTURE':str(jobs),'COMPLETED_FIXTURE':str(completed)}
    relay_host=str(ROOT/'scripts/real-e2e-relay-host.sh')
    jobs.write_text(json.dumps({'jobs':[success[0],{**success[1],'status':'in_progress','conclusion':None}]}))
    run('bash',relay_host,'hold',env=env)
    for changed in ([success[0],{**success[1],'conclusion':'failure'}], [success[0]], success+success):
        jobs.write_text(json.dumps({'jobs':changed}))
        run('bash',relay_host,'hold',env=env,ok=False)
    jobs.write_text(completed.read_text())
    run('bash',relay_host,'hold',env={**env,'RELAY_RUNNING':'false'},ok=False)
    run('bash',relay_host,'cleanup',env={**env,'DOCKER_STATUS':'1'},ok=False)
    assert not (temp/'real-e2e-evidence-relay/cleanup.txt').exists()
    run('bash',relay_host,'cleanup',env=env)
    assert not (temp/'real-e2e-relay-4242-1').exists()
print('three-VM OIDC, readiness, lifetime and cleanup negative contracts passed (no Tailnet credentials used)')
