#!/usr/bin/env python3
"""Exercise real config publication with simulated systemd and TLS endpoints."""
import importlib.util
import json
import os
import ssl
import subprocess
from pathlib import Path
import tempfile
import unittest
from types import SimpleNamespace
from unittest.mock import patch

path = Path(__file__).resolve().parents[1] / 'deploy/managed-node/rotate-config-tls.py'
spec = importlib.util.spec_from_file_location('rotate', path)
rotate = importlib.util.module_from_spec(spec)
spec.loader.exec_module(rotate)


class KeyValidationTest(unittest.TestCase):
    def test_tls_policy_for_activation_and_rollback(self):
        for rollback in (False, True):
            context = rotate.probe_context(rollback)
            self.assertGreaterEqual(context.minimum_version, ssl.TLSVersion.TLSv1_2)
            self.assertEqual(context.check_hostname, not rollback)
            self.assertEqual(context.verify_mode, ssl.CERT_NONE if rollback else ssl.CERT_REQUIRED)

    def test_both_probes_reject_a_different_leaf(self):
        for rollback in (False, True):
            with self.subTest(rollback=rollback), patch.object(rotate, 'probe_context') as factory, \
                    patch.object(rotate.socket, 'create_connection'), \
                    patch.object(rotate.time, 'monotonic', side_effect=[0, 21]):
                tls = factory.return_value.wrap_socket.return_value.__enter__.return_value
                tls.getpeercert.return_value = b'unexpected leaf'
                with self.assertRaisesRegex(ValueError, 'different leaf'):
                    rotate.handshake('vpn.example.test', 443, '0' * 64, rollback=rollback)

    def test_leaf_must_match_renewed_private_key(self):
        with tempfile.TemporaryDirectory() as tmp:
            cert, key, other = (Path(tmp) / f for f in ('leaf.pem', 'key.pem', 'other.pem'))
            subprocess.run(['openssl', 'req', '-x509', '-newkey', 'ec', '-pkeyopt',
                            'ec_paramgen_curve:P-256', '-nodes', '-days', '1', '-subj',
                            '/CN=vpn.example.test', '-out', cert, '-keyout', key],
                           check=True, capture_output=True)
            subprocess.run(['openssl', 'genpkey', '-algorithm', 'EC', '-pkeyopt',
                            'ec_paramgen_curve:P-256', '-out', other], check=True)
            self.assertRegex(rotate.checked_spki(cert, key), r'^[0-9a-f]{64}$')
            with self.assertRaisesRegex(ValueError, 'mismatch'):
                rotate.checked_spki(cert, other)


class RotationTest(unittest.TestCase):
    def setUp(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        root = Path(tmp.name)
        self.config = root / 'ocserv.conf'
        self.args = SimpleNamespace(node='01234567-0000-7000-8000-000000000001',
                                    reference='01234567-0000-7000-8000-000000000002',
                                    hostname='vpn.example.test', port=443, client_ca=None)
        self.old, self.new = 'a' * 64, 'b' * 64
        bundles = root / 'bundles'
        self.target = bundles / self.args.reference / self.new
        original = bundles / '01234567-0000-7000-8000-000000000003' / self.old
        for bundle in (original, self.target):
            bundle.mkdir(parents=True)
            (bundle / 'manifest.json').write_text(json.dumps(dict(node_id=self.args.node,
                secret_ref_id=bundle.parent.name, certificate_sha256=bundle.name,
                version=bundle.name, ca_sha256='', spki_sha256='c' * 64)))
        for filename in ('fullchain.pem', 'privkey.pem', 'chain.pem'):
            (root / filename).touch()
        self.original = f'server-cert = {original}/server-cert.pem\nserver-key = {original}/server-key.pem\n'
        self.config.write_text(self.original)
        self.config.chmod(0o600)
        self.enterContext(patch.dict(os.environ, RENEWED_LINEAGE=str(root)))
        self.enterContext(patch.multiple(rotate, ROOT=bundles, CONFIG=self.config))
        self.enterContext(patch.object(rotate, 'protected'))
        self.enterContext(patch.object(rotate.os, 'geteuid', return_value=0))
        self.enterContext(patch.object(rotate, 'fingerprint', return_value=self.new))
        self.spki = self.enterContext(patch.object(rotate, 'checked_spki', return_value='c' * 64))
        self.original_run = subprocess.run
        self.run_command = self.enterContext(patch.object(rotate.subprocess, 'run'))
        self.probe = self.enterContext(patch.object(rotate, 'handshake'))

    def test_rotation_and_duplicate_hook(self):
        rotate.activate(self.args)
        self.assertEqual(rotate.config_binding(self.config.read_text())[0], self.target)
        rotate.activate(self.args)
        self.assertEqual(sum(c.args[0][0] == 'systemctl' for c in self.run_command.call_args_list), 1)
        self.assertEqual(self.config.stat().st_mode & 0o777, 0o600)
        self.assertFalse(self.config.with_name('.ocservia-renewal.pending').exists())

    def test_bad_handshake_restores_exact_configuration(self):
        self.probe.side_effect = [ValueError('wrong leaf'), None]
        with self.assertRaisesRegex(ValueError, 'wrong leaf'):
            rotate.activate(self.args)
        self.assertEqual(self.config.read_text(), self.original)
        self.probe.assert_called_with(self.args.hostname, 443, self.old, rollback=True)
        self.assertEqual(sum(c.args[0][0] == 'systemctl' for c in self.run_command.call_args_list), 2)

    def test_registration_is_cached_across_hook_retries(self):
        root = self.config.parent
        registrar = root / 'register'
        registrar.write_text("""#!/usr/bin/env python3
import json, sys
from pathlib import Path
marker = Path(__file__).with_name('called')
marker.touch(exist_ok=False)
request = json.load(sys.stdin)
assert set(request) == {'provider', 'key_path', 'version', 'reason'}
print(json.dumps(dict(request, id='01234567-0000-7000-8000-000000000002', state='active')))
""")
        registrar.chmod(0o700)
        reference = self.args.reference
        self.args.reference = None
        self.args.registrar = registrar
        # The registrar is a real subprocess; restore run because check_output uses it.
        self.run_command.side_effect = self.original_run
        for _ in range(2):
            self.assertEqual(rotate.registered_reference(self.args, self.new, 'c' * 64, ''), reference)

    def test_retry_after_rollback_restart_failure(self):
        self.probe.side_effect = [ValueError('wrong leaf'), ValueError('rollback probe failed')]
        with self.assertRaisesRegex(ValueError, 'rollback probe failed'):
            rotate.activate(self.args)
        self.assertEqual(self.config.read_text(), self.original)
        self.assertTrue(self.config.with_name('.ocservia-renewal.pending').exists())
        self.probe.side_effect = None
        rotate.activate(self.args)
        self.assertEqual(rotate.config_binding(self.config.read_text())[0], self.target)
        self.assertFalse(self.config.with_name('.ocservia-renewal.pending').exists())

    def test_failed_provisioning_never_restarts(self):
        self.spki.side_effect = ValueError('certificate/private key mismatch')
        with self.assertRaisesRegex(ValueError, 'mismatch'):
            rotate.activate(self.args)
        self.assertEqual(self.config.read_text(), self.original)
        self.probe.assert_not_called()


if __name__ == '__main__':
    unittest.main()
