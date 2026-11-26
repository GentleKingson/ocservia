"""Shared runtime for the release business acceptance drivers."""
from contextlib import contextmanager
from functools import cache, cached_property
import http.cookiejar
import json
import os
from pathlib import Path
import ssl
import subprocess
import time
import urllib.error
import urllib.request

ROOT = Path(__file__).resolve().parents[1]


class BusinessEnvironment:
    def __init__(self):
        self.work = Path(os.environ['OCSERVIA_BUSINESS_TEST_WORK_DIR'])
        self.evidence = Path(os.environ['ARTIFACT_DIR'])
        self.origin = os.environ.get('OCSERV_CONTROLLER_PUBLIC_URL', 'https://localhost')
        self.clients = {}

    @cached_property
    def workspace(self):
        return os.environ['OCSERVIA_BUSINESS_TEST_WORKSPACE_ID']

    @cached_property
    def tls_context(self):
        context = ssl.create_default_context(cafile=str(self.work / 'ca.crt'))
        context.minimum_version = ssl.TLSVersion.TLSv1_2
        return context


@cache
def environment():
    return BusinessEnvironment()


def run(*args, data=None):
    result = subprocess.run(args, input=data, capture_output=True, check=False)
    if result.returncode:
        raise RuntimeError(f'{args[0]} failed with exit {result.returncode}')
    return result.stdout.decode()


def sql(query):
    return run(str(ROOT / 'deploy/production/compose.sh'), 'exec', '-T', 'postgres',
               'psql', '-XAt', '-v', 'ON_ERROR_STOP=1', '-U', 'ocservia_owner', '-d', 'ocservia',
               data=query.encode()).strip()


def record(name, **details):
    with (environment().evidence / 'api-checkpoints.jsonl').open('a') as output:
        output.write(json.dumps({'name': name, 'status': 'PASS', 'time': time.time(), **details}) + '\n')


def client(role):
    env = environment()
    if role not in env.clients:
        jar = http.cookiejar.LWPCookieJar(str(env.work / 'private' / f'{role}.cookies'))
        if Path(jar.filename).exists():
            jar.load(ignore_discard=True)
        opener = urllib.request.build_opener(urllib.request.HTTPSHandler(context=env.tls_context),
                                             urllib.request.HTTPCookieProcessor(jar))
        env.clients[role] = opener, jar
    return env.clients[role]


def api(path, body=None, *, role='requester', status=200, headers=None, method=None, raw=False):
    opener, jar = client(role)
    env = environment()
    effective = {'Origin': env.origin, 'X-Workspace-ID': env.workspace, 'Content-Type': 'application/json'}
    effective.update(headers or {})
    request = urllib.request.Request(env.origin + '/api/v1/' + path,
                                    data=None if body is None else json.dumps(body).encode(),
                                    headers=effective, method=method)
    try:
        response = opener.open(request, timeout=15)
    except urllib.error.HTTPError as error:
        response = error
    payload = response.read()
    expected = (status,) if isinstance(status, int) else status
    if response.status not in expected:
        # Never include a response body: successful token/download endpoints
        # and auth errors must not accidentally export credential material.
        raise RuntimeError(f'{request.get_method()} {path}: HTTP {response.status}, expected {status}')
    jar.save(ignore_discard=True)
    return payload if raw else (json.loads(payload) if payload else None)


@contextmanager
def timed(name):
    started = time.monotonic_ns()
    status = 'FAIL'
    try:
        yield
        status = 'PASS'
    finally:
        elapsed_ms = (time.monotonic_ns() - started) // 1_000_000
        with (environment().evidence / 'scenario-timings.jsonl').open('a') as output:
            output.write(json.dumps({'name': name, 'elapsed_ms': elapsed_ms, 'status': status}) + '\n')


def wait_for(description, fn, seconds=120):
    deadline = time.monotonic() + seconds
    while time.monotonic() < deadline:
        value = fn()
        if value:
            return value
        time.sleep(2)
    raise RuntimeError(f'timeout: {description}')
