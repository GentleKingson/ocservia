#!/usr/bin/env python3
"""Reuse same-commit Basic CI and qualify the exact artifacts from Release Check."""

import hashlib
import io
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import time
from urllib.parse import urlencode
from zipfile import BadZipFile, ZipFile


PRODUCTS = ('agent_amd64', 'agent_arm64', 'controller_amd64', 'controller_arm64')
VERSION = re.compile(r'[0-9]+\.[0-9]+\.[0-9]+(?:-rc\.[1-9][0-9]*)?')
SHA = re.compile(r'[0-9a-f]{40}')
DIGEST = re.compile(r'[0-9a-f]{64}')
POLL_SECONDS = 10
WAIT_SECONDS = 225 * 60


def require(condition, message):
    if not condition:
        raise RuntimeError(message)


def positive(value):
    return type(value) is int and value > 0


def gh(repo, path, *args, data=None):
    return subprocess.run(['gh', 'api', f'repos/{repo}/{path}', *args],
                          input=data, stdout=subprocess.PIPE, check=True).stdout


def api(repo, path, *args, data=None):
    return json.loads(gh(repo, path, *args, data=data))


def list_items(repo, path, key, **params):
    query = urlencode({'per_page': 100, **params})
    data = gh(repo, f'{path}?{query}', '--paginate', '--jq', f'.{key}[]')
    return [json.loads(line) for line in data.splitlines()]


def matches(run, repo, sha, events, title=None):
    head_repository = run['head_repository']
    return (run['event'] in events and run['head_sha'] == sha and run['head_branch'] == 'main'
            and head_repository is not None and head_repository['full_name'].lower() == repo.lower()
            and (title is None or run['display_title'] == title))


def latest(repo, workflow, sha, predicate):
    runs = list_items(repo, f'actions/workflows/{workflow}/runs', 'workflow_runs',
                      branch='main', head_sha=sha)
    return max((run for run in runs if predicate(run)),
               key=lambda run: run['run_number'], default=None)


def dispatch(repo, workflow, sha, inputs):
    require(api(repo, 'git/ref/heads/main')['object']['sha'] == sha,
            f'main moved before {workflow} could be dispatched for {sha}')
    data = json.dumps({'ref': 'main', 'inputs': {'expected_sha': sha, **inputs}}).encode()
    run = api(repo, f'actions/workflows/{workflow}/dispatches', '--method', 'POST',
              '-H', 'X-GitHub-Api-Version: 2026-03-10', '--input', '-', data=data)
    run_id = run['workflow_run_id']
    require(positive(run_id), f'{workflow} dispatch did not return a run ID')
    print(f'Dispatched {workflow}: {run["html_url"]}', flush=True)
    return api(repo, f'actions/runs/{run_id}')


def wait(repo, run, predicate, deadline):
    while True:
        require(predicate(run), f'Upstream run is for a different candidate: {run["html_url"]}')
        if run['status'] == 'completed':
            return run
        require(time.monotonic() < deadline, f'Timed out waiting for {run["html_url"]}')
        time.sleep(POLL_SECONDS)
        run = api(repo, f'actions/runs/{run["id"]}')


def find_or_dispatch(repo, workflow, sha, predicate, inputs, deadline):
    run = latest(repo, workflow, sha, predicate)
    if run is None:
        run = dispatch(repo, workflow, sha, inputs)
    print(f'Using {workflow}: {run["html_url"]}', flush=True)
    return wait(repo, run, predicate, deadline)


def succeeded(run):
    require(run['status'] == 'completed' and run['conclusion'] == 'success',
            f'Expected a successful run: {run["html_url"]} ({run["status"]}/{run["conclusion"]})')


def jobs(repo, run):
    executions = list_items(repo, f'actions/runs/{run["id"]}/jobs', 'jobs', filter='all')
    return {job['name']: job for job in sorted(executions, key=lambda job: job['run_attempt'])}


def passed(jobs_by_name, name):
    return jobs_by_name.get(name, {}).get('conclusion') == 'success'


def ci(repo, sha, deadline):
    def quick(run):
        return (matches(run, repo, sha, ('push',))
                or matches(run, repo, sha, ('workflow_dispatch',), 'Basic CI quick'))

    run = find_or_dispatch(repo, 'ci.yml', sha, quick,
                           {'profile': 'quick', 'contract_base_ref': 'HEAD~1'}, deadline)
    quick_jobs = jobs(repo, run)
    require(run['conclusion'] in ('success', 'failure') and passed(quick_jobs, 'Basic CI Result'),
            f'Basic CI Result did not pass: {run["html_url"]}')
    return {'quick_url': run['html_url']}


def artifact_digest(data):
    return 'sha256:' + hashlib.sha256(data).hexdigest()


def artifacts(repo, run_id):
    return {item['id']: item for item in list_items(repo, f'actions/runs/{run_id}/artifacts', 'artifacts')}


def artifact(items, artifact_id, prefix):
    item = items.get(artifact_id)
    require(positive(artifact_id) and item is not None and not item['expired']
            and item['name'].startswith(prefix),
            f'Missing, expired or unexpected artifact {artifact_id} ({prefix}); rebuild and re-qualify if needed')
    return item


def coordinates(run, version):
    return {'sha': run['head_sha'], 'version': version, 'run_id': run['id'], 'attempt': run['run_attempt']}


def read_record(repo, items, run, version):
    name = f'release-check-{run["id"]}-{run["run_attempt"]}'
    matching = [item for item in items.values() if item['name'] == name]
    require(len(matching) == 1, f'Expected one {name} artifact: {run["html_url"]}')
    item = artifact(items, matching[0]['id'], name)
    data = gh(repo, f'actions/artifacts/{item["id"]}/zip')
    require(artifact_digest(data) == item['digest'], f'Artifact digest mismatch: {name}')
    filename = 'release-check.json'
    with ZipFile(io.BytesIO(data)) as archive:
        require(archive.namelist() == [filename], f'Unexpected files in {name}')
        record = json.loads(archive.read(filename))
    require(record['schema'] == 2 and record['candidate'] == coordinates(run, version),
            f'Candidate mismatch in {name}')
    return record, item['id']


def qualify(repo, sha, version):
    def gate(run):
        return matches(run, repo, sha, ('workflow_dispatch',), f'Release Check v{version}')

    run = latest(repo, 'release-check.yml', sha, gate)
    require(run is not None, f'No Release Check on main for {sha} / v{version}')
    succeeded(run)
    items = artifacts(repo, run['id'])
    record, receipt_id = read_record(repo, items, run, version)
    outputs = {'qualification_run_id': run['id'], 'qualification_id': receipt_id,
               'qualification_url': run['html_url']}
    for key in PRODUCTS:
        kind, arch = key.split('_')
        product = record['products'][key]
        prefix = 'agent-packages' if kind == 'agent' else 'controller-images'
        artifact(items, product['id'], f'{prefix}-{arch}-{run["id"]}-')
        require(DIGEST.fullmatch(product['manifest_sha256']), f'Invalid product manifest digest: {key}')
        outputs[f'{key}_id'] = product['id']
        outputs[f'{key}_sha256'] = product['manifest_sha256']
    asset = record['assets']
    item = artifact(items, asset['id'], f'release-assets-{run["id"]}-')
    require(item['digest'] == 'sha256:' + asset['sha256'], 'Release assets digest mismatch')
    outputs['assets_id'] = asset['id']
    return outputs


def write_outputs(values):
    with open(os.environ['GITHUB_OUTPUT'], 'a') as stream:
        for key, value in values.items():
            if isinstance(value, bool):
                value = str(value).lower()
            require('\n' not in str(value) and '\r' not in str(value), f'Invalid output: {key}')
            print(f'{key}={value}', file=stream)


def write_record(sha, version):
    env = os.environ
    run = {'head_sha': sha, 'id': int(env['GITHUB_RUN_ID']), 'run_attempt': int(env['GITHUB_RUN_ATTEMPT'])}
    record = {
        'schema': 2,
        'candidate': coordinates(run, version),
        'products': {
            key: {'id': int(env[f'{key.upper()}_ID']), 'manifest_sha256': env[f'{key.upper()}_SHA256']}
            for key in PRODUCTS},
        'assets': {'id': int(env['ASSETS_ID']), 'sha256': env['ASSETS_SHA256']},
        'checks': {'quick_url': env['QUICK_URL']},
    }
    (Path(env['RUNNER_TEMP']) / 'release-check.json').write_text(json.dumps(record) + '\n')


def main():
    phase = sys.argv[1]
    sha, version = os.environ['GITHUB_SHA'], os.environ['VERSION']
    require(SHA.fullmatch(sha) and VERSION.fullmatch(version), 'Invalid candidate SHA or version')
    if phase == 'record-check':
        write_record(sha, version)
        return
    repo = os.environ['GH_REPO']
    deadline = time.monotonic() + WAIT_SECONDS
    if phase == 'ci':
        write_outputs(ci(repo, sha, deadline))
    else:
        raise RuntimeError(f'Unknown evidence phase: {phase}')


if __name__ == '__main__':
    try:
        main()
    except (RuntimeError, OSError, ValueError, KeyError, TypeError, BadZipFile, subprocess.CalledProcessError) as error:
        sys.exit(f'Release evidence failed: {error}')
