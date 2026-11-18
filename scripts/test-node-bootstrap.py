#!/usr/bin/env python3
"""Exercise token provisioning parsing and recovery without privileged fixtures.

UID ownership is simulated as root; file types, modes, link counts, filesystem
writes and response validation are real. The systemd/native integration remains
covered by the disposable host suite.
"""
import base64
import contextlib
import io
import json
import os
import pty
from pathlib import Path
import shutil
import stat
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch

ROOT = Path(__file__).resolve().parents[1]
helper = (ROOT / 'deploy/lib/node-bootstrap.sh').read_text()
source = helper.split("<<'PY'\n", 1)[1].split('\nPY\n', 1)[0]
program = compile(source, 'node-bootstrap-helper', 'exec')
real_lstat, real_fstat, real_stat = os.lstat, os.fstat, os.stat


def root_stat(value):
    fields = list(value)
    fields[4] = fields[5] = 0
    return os.stat_result(fields)


class BootstrapTest(unittest.TestCase):
    def setUp(self):
        # Unlike /tmp, repository ancestors have no group/world writable bits.
        self.work = Path(tempfile.mkdtemp(prefix='.bootstrap-test-', dir=ROOT))
        self.state = self.work / 'state'
        origin = base64.urlsafe_b64encode(b'https://controller.example.test:8443').decode().rstrip('=')
        self.token = f'oit1_{origin}.' + base64.urlsafe_b64encode(b't' * 32).decode().rstrip('=')
        self.token_file = self.work / 'input'
        self.token_file.write_text(self.token)
        self.token_file.chmod(0o600)
        self.config = dict(controller_endpoint_id='1' * 64, environment='production',
            enrollment_token='obt2_' + 'A' * 43, relay_url='https://relay.example.test',
            relay_access_token='r' * 64, command_verification_key_hex='2' * 64,
            expires_at='2030-01-01T00:00:00Z')

    def tearDown(self):
        shutil.rmtree(self.work)

    def run_action(self, *args, ok=True):
        out, err = io.StringIO(), io.StringIO()
        status = 0
        with patch('sys.argv', ['helper', args[0], str(self.state), *map(str, args[1:])]), \
             patch('os.geteuid', return_value=0), \
             patch('os.lstat', side_effect=lambda *a, **kw: root_stat(real_lstat(*a, **kw))), \
             patch('os.stat', side_effect=lambda *a, **kw: root_stat(real_stat(*a, **kw))), \
             patch('os.fstat', side_effect=lambda *a, **kw: root_stat(real_fstat(*a, **kw))), \
             contextlib.redirect_stdout(out), contextlib.redirect_stderr(err):
            try:
                exec(program, {})
            except SystemExit as e:
                status = e.code
        self.assertEqual(status, 0 if ok else 1, err.getvalue())
        self.assertNotIn(self.token, out.getvalue() + err.getvalue())
        return out.getvalue()

    def response(self, value):
        p = self.state / 'response.json'
        p.write_text(json.dumps(value))
        p.chmod(0o600)

    @unittest.skipIf(os.geteuid() == 0, 'dispatch probe runs before actual sudo')
    def test_installer_token_sources_share_root_dispatch(self):
        bin_dir = self.work / 'bin'
        bin_dir.mkdir()
        sudo = bin_dir / 'sudo'
        sudo.write_text('#!/bin/sh\nprintf "%s\\n" "$@" >"$TEST_DISPATCH"\n')
        sudo.chmod(0o700)
        log = self.work / 'dispatch'
        env = dict(os.environ, PATH=str(bin_dir) + ':' + os.environ['PATH'],
                   TEST_DISPATCH=str(log))
        # A stale manual install.env must never select the standard flow.
        (self.work / 'install.env').write_text('INVALID=must-not-be-read\n')
        installer = str(ROOT / 'deploy/managed-node/install.sh')

        def dispatch(args):
            master, slave = pty.openpty()
            try:
                result = subprocess.run(['bash', installer, '--version', 'v9.8.7', *args],
                                        cwd=self.work, env=env, stdin=slave,
                                        stdout=slave, stderr=slave, timeout=10)
                self.assertEqual(result.returncode, 0)
                arguments = log.read_text().splitlines()
                self.assertNotIn(self.token, log.read_text())
                return arguments
            finally:
                os.close(slave)
                os.close(master)
        default = dispatch([])
        self.assertEqual(default, dispatch(['--join-token']))
        self.assertEqual(default, dispatch(['--interactive']))
        self.assertIn('--root-lifecycle', default)
        self.assertIn('--join-token', default)
        for source in (['--join-token-file', str(self.token_file)], ['--resume-install']):
            arguments = dispatch(source)
            self.assertIn('--root-lifecycle', arguments)
            self.assertIn(source[0], arguments)
            self.assertNotIn('--join-token', arguments)
        result = subprocess.run(['bash', installer, '--non-interactive'], cwd=self.work,
                                env=env, capture_output=True, text=True)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('unattended token install requires', result.stderr)

    def test_config_request_and_recovery(self):
        self.run_action('init', self.token_file)
        self.assertEqual(self.run_action('origin').strip(), 'https://controller.example.test:8443')
        self.run_action('request')
        self.assertEqual(json.loads((self.state / 'request.json').read_text()), {'token': self.token})
        self.response(self.config)
        self.run_action('config')
        self.assertEqual((self.state / 'enrollment-token').read_text(), self.config['enrollment_token'])
        self.assertNotIn(self.token, (self.state / 'enrollment-token').read_text())
        subprocess.run(['openssl', 'pkey', '-pubin', '-in', str(self.state / 'command-key.pem'), '-noout'], check=True, capture_output=True)
        (self.state / 'enrollment-token').unlink()
        self.run_action('init', 'resume')
        self.run_action('cached-config')
        self.assertEqual((self.state / 'enrollment-token').read_text(), self.config['enrollment_token'])
        for p in self.state.iterdir():
            self.assertEqual(stat.S_IMODE(p.stat().st_mode), 0o600)
        self.config['controller_endpoint_id'] = '3' * 64
        self.response(self.config)
        self.run_action('config', ok=False)
        self.assertEqual(json.loads((self.state / 'config.json').read_text())['controller_endpoint_id'], '1' * 64)

    def test_tokens_permissions_and_key_isolation(self):
        self.token_file.chmod(0o644)
        self.run_action('init', self.token_file, ok=False)
        self.token_file.chmod(0o600)
        link = self.work / 'link'
        link.symlink_to(self.token_file)
        self.run_action('init', link, ok=False)
        link.unlink()
        os.link(self.token_file, link)
        self.run_action('init', self.token_file, ok=False)
        link.unlink()
        self.token_file.write_text('obt2_' + 'A' * 43)
        self.run_action('init', self.token_file, ok=False)
        self.token_file.write_text(self.token)
        self.run_action('init', self.token_file)
        self.token_file.write_text(self.token[:-1] + 'B')
        self.run_action('init', self.token_file, ok=False)
        self.config['relay_url'] = 'https://user:password@relay.example.test'
        self.response(self.config)
        self.run_action('config', ok=False)
        self.assertFalse((self.state / 'enrollment-token').exists())

    def test_shell_registration_and_start_order(self):
        self.run_action('init', self.token_file)
        bin_dir = self.work / 'bin'
        bin_dir.mkdir()
        wrapper = bin_dir / 'python3'
        wrapper.write_text(f"#!{sys.executable}\n" + r'''import os, sys
from unittest.mock import patch
source=sys.stdin.read()
sys.argv.pop(1)  # emulate python3 - argv handling
a, b, c = os.stat, os.lstat, os.fstat
def root(v):
    f=list(v); f[4]=f[5]=0
    return os.stat_result(f)
with patch('os.geteuid', return_value=0), patch('os.stat', side_effect=lambda *x, **k:root(a(*x,**k))), patch('os.lstat', side_effect=lambda *x, **k:root(b(*x,**k))), patch('os.fstat', side_effect=lambda *x, **k:root(c(*x,**k))):
    exec(compile(source, 'bootstrap', 'exec'), {})
''')
        wrapper.chmod(0o700)
        curl = bin_dir / 'curl'
        curl.write_text(f"#!{sys.executable}\n" + r'''import json, os, pathlib, sys
args=sys.argv[1:]
assert '--proto' in args and args[args.index('--proto')+1] == '=https'
assert '--location' not in args and '-k' not in args
body=json.loads(pathlib.Path(args[args.index('--data-binary')+1][1:]).read_text())
assert body['token'].startswith('oit1_')
state=pathlib.Path(os.environ['TEST_STATE'])
operation=args[-1].rsplit('/',1)[1]
with (state/'calls').open('a') as f: f.write(operation+'\n')
if operation == 'attestation':
    assert len(body['endpoint_id']) == 64
    reply=dict(controller_nonce_hex='4'*64,credential_context_sha256_hex='5'*64,registered=bool(os.environ.get('REGISTERED')))
elif operation == 'register':
    assert body['registration']['key_id'] == 'root-key'
    assert body['user_password_seal_public_key_sha256'] == '7'*64
    assert body['p12_password_seal_public_key_sha256'] == '8'*64
    reply=dict(key_id='wrong' if os.environ.get('BAD_KEY') else 'root-key',state='active')
elif operation == 'status':
    assert (state/'started').exists()
    reply=dict(trust_status='active',connection_state='online',freshness='fresh',attestation_registered=True)
else:
    raise AssertionError(operation)
pathlib.Path(args[args.index('--output')+1]).write_text(json.dumps(reply))
''')
        curl.chmod(0o700)
        privd = bin_dir / 'privd'
        privd.write_text("#!/bin/sh\n[ \"$1\" = attestation-registration ] || exit 2\ntouch \"$TEST_STATE/privd-called\"\nprintf '%s\\n' '{\"key_id\":\"root-key\"}'\n")
        privd.chmod(0o700)
        systemctl = bin_dir / 'systemctl'
        systemctl.write_text("#!/bin/sh\n[ -f \"$TEST_STATE/registered.json\" ] || exit 2\nprintf '%s\\n' \"$*\" >\"$TEST_STATE/started\"\n")
        systemctl.chmod(0o700)
        script = f'''set -euo pipefail
umask 077
source "{ROOT}/deploy/lib/node-bootstrap.sh"
fail() {{ echo "$1" >&2; exit 1; }}
JOIN_STATE_DIR="$TEST_STATE"
JOIN_ORIGIN=https://controller.example.test:8443
SYSROOT="$TEST_STATE"
PRIVD_BINARY="{privd}"
ENDPOINT_ID={'6' * 64}
USER_PASSWORD_SEAL_KEY_ID=user-v1
P12_PASSWORD_SEAL_KEY_ID=p12-v1
USER_SEAL_DESCRIPTOR={'7' * 64}
P12_SEAL_DESCRIPTOR={'8' * 64}
node_bootstrap_complete 01900000-0000-7000-8000-000000000001
'''
        env = dict(os.environ, PATH=str(bin_dir) + ':' + os.environ['PATH'], TEST_STATE=str(self.state))
        result = subprocess.run(['bash', '-c', script], env=env, text=True, capture_output=True)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn('ONLINE\n', result.stdout)
        self.assertEqual((self.state / 'calls').read_text().splitlines(), ['attestation', 'register', 'status'])
        self.assertEqual((self.state / 'started').read_text().strip(), 'enable --now ocservia-privd.service ocservia-agent.service')
        (self.state / 'started').unlink()
        env['BAD_KEY'] = '1'
        result = subprocess.run(['bash', '-c', script], env=env, text=True, capture_output=True)
        self.assertNotEqual(result.returncode, 0)
        self.assertFalse((self.state / 'started').exists())
        self.assertNotIn('ONLINE', result.stdout)
        (self.state / 'privd-called').unlink()
        env.pop('BAD_KEY')
        env['REGISTERED'] = '1'
        result = subprocess.run(['bash', '-c', script], env=env, text=True, capture_output=True)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('restore the original root key', result.stderr)
        self.assertFalse((self.state / 'privd-called').exists())
        self.assertFalse((self.state / 'started').exists())
        key = self.state / 'var/lib/ocservia-privd/attestation.key'
        key.parent.mkdir(parents=True)
        key.write_bytes(b'original-root-key')
        result = subprocess.run(['bash', '-c', script], env=env, text=True, capture_output=True)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(key.read_bytes(), b'original-root-key')

    def test_attestation_and_observed_online(self):
        self.run_action('init', self.token_file)
        node = '01900000-0000-7000-8000-000000000001'
        self.response(dict(controller_nonce_hex='4' * 64, credential_context_sha256_hex='5' * 64, registered=False))
        self.assertEqual(self.run_action('challenge').splitlines(), ['4' * 64, '5' * 64, 'false'])
        proof = self.state / 'registration.json'
        proof.write_text('{"key_id":"root-key"}')
        proof.chmod(0o600)
        self.run_action('request', node, '6' * 64, 'registration', 'user-v1', '7' * 64, 'p12-v1', '8' * 64)
        request = json.loads((self.state / 'request.json').read_text())
        self.assertEqual(request['registration'], {'key_id': 'root-key'})
        self.response(dict(key_id='other-key', state='active'))
        self.run_action('registered', ok=False)
        self.response(dict(key_id='root-key', state='active'))
        self.run_action('registered')
        status = dict(trust_status='active', connection_state='online', freshness='stale', attestation_registered=True)
        self.response(status)
        self.run_action('online', ok=False)
        self.assertFalse((self.state / 'online.json').exists())
        status['freshness'] = 'fresh'
        self.response(status)
        self.run_action('online')


if __name__ == '__main__':
    unittest.main()
