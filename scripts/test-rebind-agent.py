#!/usr/bin/python3
"""Local lifecycle crash/retry tests; run in an isolated root container."""
import importlib.util
from contextlib import closing
import json
import os
from pathlib import Path
import sqlite3
import tempfile
import time
import types
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location('rebind', Path(__file__).with_name('rebind-agent.py'))
rebind = importlib.util.module_from_spec(spec)
spec.loader.exec_module(rebind)


class RebindTests(unittest.TestCase):
    def setUp(self):
        if os.geteuid() != 0:
            self.skipTest('requires an isolated root container')
        Path('/rebind-test-fixtures').mkdir(exist_ok=True, mode=0o700)
        self.temporary = tempfile.TemporaryDirectory(dir='/rebind-test-fixtures')
        self.root = Path(self.temporary.name)
        self.patches = []
        for name, leaf in [('CONF', 'conf'), ('STATE', 'state'), ('AGENT_STATE', 'agent'),
                           ('PRIVD_STATE', 'privd'), ('UPGRADE_STATE', 'upgrade')]:
            path = self.root / leaf
            path.mkdir(mode=0o700)
            replacement = patch.object(rebind, name, path)
            replacement.start()
            self.patches.append(replacement)
        replacement = patch.object(rebind, 'ACTIVE', rebind.CONF / 'active-binding')
        replacement.start()
        self.patches.append(replacement)
        self.account = types.SimpleNamespace(pw_uid=1000, pw_gid=1000)
        self.source = dict(node='018f0c2e-7b1a-7c3d-8e9f-0123456789ab', controller='11' * 32,
                           endpoint='22' * 32, key='33' * 32, blocked=False,
                           identity=str(self.root / 'old-identity'), journal=str(self.root / 'old.db'),
                           effects=str(self.root / 'old-effects.db'))
        Path(self.source['identity']).mkdir(mode=0o700)
        for name, content in [('endpoint.key', b'preserved-private-key'), ('controller.endpoint', b'old-pin')]:
            Path(self.source['identity'], name).write_bytes(content)
        with closing(sqlite3.connect(self.source['journal'])) as db, db:
            db.execute('CREATE TABLE command_journal(state TEXT)')
        self.operation = rebind.STATE / '00000000-0000-4000-8000-000000000001'
        self.operation.mkdir(mode=0o700)
        self.state = dict(id=self.operation.name, phase='enrolled', source=self.source,
                          node='018f0c2e-7b1a-7c3d-8e9f-0123456789ac', controller='44' * 32,
                          key='55' * 32, environment='production')
        stage = rebind.AGENT_STATE / 'rebind-staging' / self.state['id'] / 'identity'
        stage.mkdir(parents=True, mode=0o700)
        for name in ('endpoint.key', 'controller.endpoint'):
            (stage / name).write_bytes(Path(self.source['identity'], name).read_bytes())
        rebind.save(self.operation, self.state)

    def tearDown(self):
        for replacement in reversed(self.patches):
            replacement.stop()
        self.temporary.cleanup()

    def runner(self, args, **kwargs):
        if '--verify-rebind-session' in args:
            return types.SimpleNamespace(returncode=0, stdout=str(int(time.time())).encode())
        if 'start' in args:
            journal = rebind.AGENT_STATE / 'bindings' / self.state['node'] / 'agent.db'
            with closing(sqlite3.connect(journal)) as db, db:
                db.execute('CREATE TABLE IF NOT EXISTS agent_metadata(key TEXT, value BLOB)')
                db.execute("INSERT INTO agent_metadata VALUES('verified_session_grant', ?)", (b'signed-fixture',))
        return types.SimpleNamespace(returncode=0, stdout=b'')

    def commit(self, **options):
        with patch.object(rebind, 'current_binding', return_value=self.source), \
             patch.object(rebind, 'agent', return_value=self.source['endpoint']), \
             patch.object(rebind.subprocess, 'run', side_effect=options.get('runner', self.runner)):
            rebind.commit(self.operation, self.state, {}, self.account)

    def test_packaged_p12_environment_names_are_valid(self):
        rebind.atomic_write(rebind.CONF / 'agent.env', b'P12_PASSWORD_SEAL_KEY_ID=default-p12\nP12_PASSWORD_SEAL_PUBLIC_KEY_SHA256=abcd\n')
        self.assertEqual(rebind.environment()['P12_PASSWORD_SEAL_KEY_ID'], 'default-p12')
        rebind.atomic_write(rebind.CONF / 'agent.env', b'P12_PASSWORD_SEAL_KEY_ID=one\nP12_PASSWORD_SEAL_KEY_ID=two\n')
        with self.assertRaises(RuntimeError):
            rebind.environment()

    def test_relay_arguments_preserve_single_and_development_paths(self):
        self.assertEqual(rebind.relay_arguments(), [])
        for suffix in ('', 'RELAY_URL_B=\n', 'RELAY_URL_B=""\n'):
            with self.subTest(suffix=suffix):
                rebind.atomic_write(rebind.CONF / 'relays.env',
                                    ('RELAY_URL_A=https://relay.example.test\n' + suffix).encode())
                self.assertEqual(rebind.relay_arguments(), [
                    '--relay-mode', 'custom', '--relay-url', 'https://relay.example.test',
                    '--relay-token-file', str(rebind.CONF / 'relay-access-token')])
        rebind.atomic_write(rebind.CONF / 'relay-ca.pem', b'fixture')
        self.assertEqual(rebind.relay_arguments()[-2:],
                         ['--relay-ca-file', str(rebind.CONF / 'relay-ca.pem')])

    def assert_relay_preflight(self, arguments):
        rebind.atomic_write(rebind.CONF / 'relays.env',
                            b'RELAY_URL_A=https://relay.example.test\nRELAY_URL_B=https://second.example.test\n')
        rebind.atomic_write(rebind.ACTIVE, b'old-authority')
        rebind.STATE.chmod(0o750)
        os.chown(rebind.STATE, 0, self.account.pw_gid)
        before = {path: path.read_bytes() for path in self.root.rglob('*') if path.is_file()}
        operations = set(rebind.STATE.iterdir())
        env = {key: 'fixture' for key in ['USER_PASSWORD_SEAL_KEY_ID', 'USER_PASSWORD_SEAL_PUBLIC_KEY_SHA256',
                                         'P12_PASSWORD_SEAL_KEY_ID', 'P12_PASSWORD_SEAL_PUBLIC_KEY_SHA256']}
        def send(args, *rest):
            return self.state['node'] if '--enrollment-token-file' in args else self.source['endpoint']
        with patch.object(rebind.sys, 'argv', ['rebind'] + arguments), \
             patch.object(rebind.pwd, 'getpwnam', return_value=self.account), \
             patch.object(rebind, 'environment', return_value=env), \
             patch.object(rebind, 'managed_units'), \
             patch.object(rebind, 'current_binding', return_value=self.source), \
             patch.object(rebind, 'public_key', return_value=self.state['key']), \
             patch.object(rebind.time, 'monotonic', side_effect=[0, 61]), \
             patch.object(rebind, 'agent', side_effect=send) as agent, \
             patch.object(rebind.subprocess, 'run', return_value=types.SimpleNamespace(stdout=b'1')) as process:
            with self.assertRaisesRegex(RuntimeError, 'only one dedicated Relay'):
                rebind.main()
            agent.assert_not_called()
            process.assert_not_called()
        self.assertEqual(set(rebind.STATE.iterdir()), operations)
        for path, content in before.items():
            self.assertEqual(path.read_bytes(), content, str(path))

    def test_prepare_rejects_second_relay_before_operation_and_identity_work(self):
        self.state['phase'] = 'verified'
        rebind.save(self.operation, self.state)
        token = self.root / 'token'
        rebind.atomic_write(token, b'obt1_' + b'A' * 43)
        arguments = ['prepare', '--controller', self.state['controller'], '--command-key-file', 'fixture',
                     '--token-file', str(token), '--environment', 'production', '--reason', 'fixture',
                     '--source-disposition', 'revoked']
        for suffix in (['--dry-run'], []):
            with self.subTest(suffix=suffix):
                self.assert_relay_preflight(arguments + suffix)

    def test_resume_rejects_second_relay_before_staging_or_enrollment(self):
        token = b'obt1_' + b'A' * 43
        self.state['token_hash'] = rebind.hashlib.sha256(token).hexdigest()
        rebind.atomic_write(self.operation / 'token', token, 0o640)
        for path in [rebind.AGENT_STATE / 'rebind-staging', rebind.AGENT_STATE / 'rebind-staging' / self.state['id']]:
            os.chown(path, self.account.pw_uid, self.account.pw_gid)
            path.chmod(0o700)
        for phase in ('prepared', 'enrolling'):
            with self.subTest(phase=phase):
                self.state['phase'] = phase
                rebind.save(self.operation, self.state)
                self.assert_relay_preflight(['resume', self.state['id']])

    def test_uncommitted_operation_rejects_second_relay_before_stopping_services(self):
        for phase in ('enrolled', 'committing'):
            with self.subTest(phase=phase):
                self.state['phase'] = phase
                rebind.save(self.operation, self.state)
                self.assert_relay_preflight(['commit', self.state['id']])

    def test_status_remains_available_with_second_relay(self):
        rebind.atomic_write(rebind.CONF / 'relays.env', b'RELAY_URL_B=https://second.example.test\n')
        rebind.STATE.chmod(0o750)
        os.chown(rebind.STATE, 0, self.account.pw_gid)
        with patch.object(rebind.sys, 'argv', ['rebind', 'status', self.state['id']]), \
             patch.object(rebind.pwd, 'getpwnam', return_value=self.account), \
             patch.object(rebind, 'environment', return_value={}), \
             patch.object(rebind, 'relay_arguments', side_effect=AssertionError('diagnosis must remain available')):
            rebind.main()

    def test_commit_preserves_source_and_publishes_one_complete_authority(self):
        self.commit()
        active = rebind.decode_binding(rebind.secure_read(rebind.ACTIVE))
        self.assertEqual(active['node'], self.state['node'])
        self.assertEqual(active['endpoint'], self.source['endpoint'])
        self.assertNotEqual(active['controller'], self.source['controller'])
        self.assertEqual(Path(self.source['identity'], 'endpoint.key').read_bytes(), b'preserved-private-key')
        self.assertTrue(Path(self.source['journal']).exists())
        self.assertEqual(self.state['phase'], 'verified')

    def test_old_unknown_survives_and_quarantines_new_mutations(self):
        with closing(sqlite3.connect(self.source['journal'])) as db, db:
            db.execute("INSERT INTO command_journal VALUES('unknown')")
        self.commit()
        self.assertTrue(rebind.decode_binding(rebind.secure_read(rebind.ACTIVE))['blocked'])
        with closing(sqlite3.connect(self.source['journal'])) as db, db:
            self.assertEqual(db.execute('SELECT state FROM command_journal').fetchone()[0], 'unknown')

    def test_unresolved_root_effect_quarantines_even_with_empty_agent_journal(self):
        with closing(sqlite3.connect(self.source['effects'])) as db, db:
            db.execute('CREATE TABLE authorized_effects(state TEXT)')
            db.execute('CREATE TABLE desired_effects(state TEXT)')
            db.execute("INSERT INTO authorized_effects VALUES('prepared')")
        self.commit()
        self.assertTrue(self.state['mutations_blocked'])

    def test_publication_failure_preserves_source_and_can_resume(self):
        original = rebind.atomic_write
        def fail(path, *args, **kwargs):
            if path == rebind.ACTIVE:
                raise OSError('injected publication failure')
            return original(path, *args, **kwargs)
        with patch.object(rebind, 'atomic_write', side_effect=fail):
            with self.assertRaises(OSError):
                self.commit()
        self.assertFalse(rebind.ACTIVE.exists())
        self.assertEqual(Path(self.source['identity'], 'endpoint.key').read_bytes(), b'preserved-private-key')
        self.state = json.loads((self.operation / 'state.json').read_text())
        self.commit()
        self.assertEqual(self.state['phase'], 'verified')

    def test_restart_failure_keeps_target_committed_and_source_evidence(self):
        def fail(args, **kwargs):
            if 'start' in args:
                raise rebind.subprocess.CalledProcessError(1, args)
            return self.runner(args, **kwargs)
        with self.assertRaises(rebind.subprocess.CalledProcessError):
            self.commit(runner=fail)
        self.assertEqual(self.state['phase'], 'committed')
        self.assertEqual(rebind.decode_binding(rebind.secure_read(rebind.ACTIVE))['node'], self.state['node'])
        self.assertTrue(Path(self.source['journal']).exists())
        rebind.atomic_write(rebind.CONF / 'relays.env', b'RELAY_URL_B=https://second.example.test\n')
        self.commit()
        self.assertEqual(self.state['phase'], 'verified')
        # A crash after publication can leave the durable phase at committing.
        self.state['phase'] = 'committing'
        self.commit()
        self.assertEqual(self.state['phase'], 'verified')
        self.commit()

    def test_stale_operation_cannot_start_another_binding(self):
        self.commit()
        rebind.atomic_write(rebind.ACTIVE, rebind.secure_read(rebind.ACTIVE).replace(b'44' * 32, b'66' * 32), 0o640)
        with self.assertRaisesRegex(RuntimeError, 'active binding changed'):
            self.commit()

    def test_enrollment_response_loss_reuses_same_token_and_does_not_commit(self):
        self.state['phase'] = 'prepared'
        token = b'obt1_' + b'A' * 43
        self.state['token_hash'] = rebind.hashlib.sha256(token).hexdigest()
        rebind.atomic_write(self.operation / 'token', token, 0o640)
        rebind.atomic_write(rebind.CONF / 'relays.env', b'RELAY_URL_A=https://relay.example.test\nRELAY_URL_B=\n')
        env = {key: 'fixture' for key in ['USER_PASSWORD_SEAL_KEY_ID', 'USER_PASSWORD_SEAL_PUBLIC_KEY_SHA256',
                                         'P12_PASSWORD_SEAL_KEY_ID', 'P12_PASSWORD_SEAL_PUBLIC_KEY_SHA256']}
        calls = []
        def send(args, *rest):
            if '--enrollment-token-file' in args:
                self.assertEqual(args.count('--relay-url'), 1)
                self.assertEqual(args[args.index('--relay-url') + 1], 'https://relay.example.test')
                self.assertEqual(Path(args[args.index('--identity-dir') + 1], 'endpoint.key').read_bytes(),
                                 Path(self.source['identity'], 'endpoint.key').read_bytes())
                calls.append(Path(args[args.index('--enrollment-token-file') + 1]).read_bytes())
                if len(calls) == 1:
                    raise rebind.subprocess.TimeoutExpired('enroll', 90)
                return self.state['node']
            return self.source['endpoint']
        # Restore ownership expected by the real stage-directory validation.
        for path in [rebind.AGENT_STATE / 'rebind-staging', rebind.AGENT_STATE / 'rebind-staging' / self.state['id']]:
            os.chown(path, 1000, 1000)
            path.chmod(0o700)
        with patch.object(rebind, 'current_binding', return_value=self.source), \
             patch.object(rebind, 'agent', side_effect=send):
            with self.assertRaises(rebind.subprocess.TimeoutExpired):
                rebind.enroll(self.operation, self.state, env, self.account)
            self.assertEqual(self.state['phase'], 'enrolling')
            self.assertFalse(rebind.ACTIVE.exists())
            rebind.enroll(self.operation, self.state, env, self.account)
        self.assertEqual(calls, [token, token])
        self.assertEqual(self.state['phase'], 'enrolled')
        self.assertFalse(rebind.ACTIVE.exists())

    def test_trusted_files_reject_links_and_writable_ancestry(self):
        path = self.root / 'trusted'
        rebind.atomic_write(path, b'public', 0o640)
        self.assertEqual(rebind.secure_read(path), b'public')
        os.link(path, self.root / 'alias')
        with self.assertRaises(RuntimeError):
            rebind.secure_read(path)
        (self.root / 'alias').unlink()
        path.chmod(0o666)
        with self.assertRaises(RuntimeError):
            rebind.secure_read(path)

    def test_publication_uses_pinned_directories_under_parent_substitution(self):
        stage = rebind.AGENT_STATE / 'rebind-staging' / self.state['id'] / 'identity'
        target = self.root / 'target'
        target.mkdir(mode=0o700)
        protected = self.root / 'protected'
        protected.mkdir(mode=0o700)
        retained = self.root / 'retained'
        original = os.rename
        def substitute(source, destination, **kwargs):
            original(target, retained)
            target.symlink_to(protected, target_is_directory=True)
            return original(source, destination, **kwargs)
        with patch.object(rebind.os, 'rename', side_effect=substitute):
            rebind.publish_identity(stage, target, self.account)
        self.assertFalse((protected / 'identity').exists())
        self.assertTrue((retained / 'identity' / 'endpoint.key').is_file())

    def test_custom_service_state_paths_are_refused(self):
        def show(args, **kwargs):
            name = 'agent' if 'ocservia-agent.service' in args else 'privd'
            executable = str(rebind.BIN / ('ocservia-' + name))
            return types.SimpleNamespace(stdout=('{ path=' + executable + ' ; argv[]=' + executable + ' --node-id fixture ; }').encode())
        with patch.object(rebind.subprocess, 'run', side_effect=show):
            rebind.managed_units()
        unsafe = types.SimpleNamespace(stdout=b'{ path=/usr/libexec/ocservia/ocservia-agent ; argv[]=ocservia-agent --journal /other.db ; }')
        with patch.object(rebind.subprocess, 'run', return_value=unsafe):
            with self.assertRaisesRegex(RuntimeError, 'custom service arguments'):
                rebind.managed_units()

    def test_public_files_keep_required_permissions_under_restrictive_umask(self):
        old = os.umask(0o077)
        try:
            path = self.root / 'public-binding'
            rebind.atomic_write(path, b'public', 0o640, 1000)
            self.assertEqual(path.stat().st_mode & 0o777, 0o640)
            rebind.managed_directory(self.root / 'traversable', 0, 1000, 0o750)
            self.assertEqual((self.root / 'traversable').stat().st_mode & 0o777, 0o750)
        finally:
            os.umask(old)


if __name__ == '__main__':
    unittest.main()
