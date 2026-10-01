#!/usr/bin/env python3
"""Focused checks for Business Smoke and manual integration."""
import importlib.util
import json
import os
from pathlib import Path
import tempfile
from types import SimpleNamespace
from unittest.mock import MagicMock, patch


def main():
    root = Path(__file__).resolve().parents[1]
    with tempfile.TemporaryDirectory() as directory:
        work = Path(directory)
        (work / 'private').mkdir()
        (work / 'evidence').mkdir()
        (work / 'config-reference.json').write_text(json.dumps({'id': 'tls-ref'}))
        (work / 'private/smoke-vpn-password').write_text('test-password')
        env = {'T07_WORK': str(work), 'ARTIFACT_DIR': str(work / 'evidence'),
               'T07_WORKSPACE': 'workspace', 'T07_NODE': 'node', 'BUSINESS_PROFILE': 'smoke'}
        spec = importlib.util.spec_from_file_location('release_business_api', root / 'scripts/release-business-api.py')
        with patch.dict(os.environ, env), patch('ssl.create_default_context'):
            business = importlib.util.module_from_spec(spec)
            spec.loader.exec_module(business)

        with patch.object(business, 'sql', side_effect=['f', 'f', 't']) as sql, \
                patch.object(business.time, 'sleep') as sleep, patch.object(business, 'record') as record:
            business.wait_for_relay_outage('0123-4567')
            assert sql.call_count == 3 and sleep.call_count == 2
            assert "node_id=decode('01234567','hex')" in sql.call_args.args[0]
            assert 'lease_until>clock_timestamp()' in sql.call_args.args[0]
            record.assert_called_once_with('single_relay_owner_lease_invalidated')

        with patch.object(business, 'sql', return_value='f'), \
                patch.object(business.time, 'monotonic', side_effect=[0, 0, 121]), \
                patch.object(business.time, 'sleep'), patch.object(business, 'record') as record:
            try:
                business.wait_for_relay_outage('0123-4567')
            except RuntimeError as error:
                assert 'owner lease invalidation' in str(error)
            else:
                raise AssertionError('live owner must prevent offline queueing')
            record.assert_not_called()

        # Active privd and intact receipts must not report R2 success while its
        # dependent Agent is still down and has not acquired a new session.
        recovery = {'service': '', 'seconds': 0}

        def restart_command(*args):
            if args[:3] == ('sudo', 'systemctl', 'restart'):
                recovery['service'] = args[3]
            return 'active'

        with patch.object(business, 'run', side_effect=restart_command), \
                patch.object(business, 'owner', return_value={'owner_epoch': 1}), \
                patch.object(business, 'fresh_owner', side_effect=lambda _before:
                             {'owner_epoch': 2} if recovery['service'] == 'ocservia-agent' else None), \
                patch.object(business, 'identity_digest', return_value='unchanged'), \
                patch.object(business, 'check_confirmed'), patch.object(business, 'record') as record, \
                patch.object(business.time, 'monotonic', side_effect=lambda: recovery['seconds']), \
                patch.object(business.time, 'sleep', side_effect=lambda delay:
                             recovery.update(seconds=recovery['seconds'] + delay)):
            try:
                business.agent_privd_recovery([], 'confirmed-config')
            except RuntimeError as error:
                assert 'fresh Agent session after ocservia-privd' in str(error)
            else:
                raise AssertionError('active privd must not hide unavailable Agent')
            record.assert_not_called()

        directives = business.smoke_directives(4)
        assert {item['name'] for item in directives} == {
            'auth', 'cookie-timeout', 'device', 'dns', 'ipv4-network', 'max-clients',
            'max-same-clients', 'route', 'socket-file', 'tcp-port', 'udp-port', 'server-cert', 'server-key'}
        assert next(item for item in directives if item['name'] == 'max-clients')['value'] == '4'
        assert all(item['secret_ref']['secret_ref_id'] == 'tls-ref' for item in directives if 'secret_ref' in item)

        requests = []

        def plan_api(path, body=None, **_kwargs):
            if path == 'nodes/node/config-plans':
                requests.append(body)
                return {'id': 'plan'}
            if path == 'config-plans/plan':
                return {'id': 'plan', 'state': 'succeeded', 'validation': 'valid'}
            raise AssertionError(path)

        with patch.dict(os.environ, env), patch.object(business, 'api', side_effect=plan_api):
            business.smoke_plan(directives, 0, 'smoke')
        assert requests[0]['expected_revision'] == 0
        assert requests[0]['template'] == {'name': 'smoke', 'directives': directives}

        def api(path, *_args, **_kwargs):
            if path.endswith('/apply') or path.endswith('/users'):
                return {'id': 'operation'}
            if path == 'operations/operation':
                return {'state': 'succeeded', 'config_apply_state': 'succeeded'}
            if path == 'nodes/node':
                return {'config_revision': 1}
            raise AssertionError(path)

        with patch.dict(os.environ, env), patch.object(business, 'smoke_plan', return_value={
            'id': 'plan', 'materialized_hash': 'newhash'}), patch.object(business, 'approval', return_value='approval'), \
                patch.object(business, 'api', side_effect=api), patch.object(business, 'record') as record, \
                patch.object(business, 'run', side_effect=['oldhash  /etc/ocserv/ocserv.conf',
                                                          'newhash  /etc/ocserv/ocserv.conf']):
            business.smoke_config_apply()
            record.assert_called_once()
        assert json.loads((work / 'smoke-applied.json').read_text())['materialized_hash'] == 'newhash'

        with patch.dict(os.environ, {**env, 'PRODUCTION_SIGNER_ACCEPTANCE': 'true'}), \
                patch.object(business, 'run', return_value='{"ciphertext":"sealed"}') as signer, \
                patch.object(business, 'api', side_effect=api) as requests, \
                patch.object(business, 'record'):
            business.smoke_user()
            assert signer.call_args.args[-1] == 'seal'
            assert requests.call_args_list[0].args[1]['sealed_password'] == {'ciphertext': 'sealed'}

        vpn = MagicMock()
        vpn.poll.return_value = None
        with patch.dict(os.environ, env), patch.object(business.subprocess, 'Popen', return_value=vpn) as popen, \
                patch.object(business.subprocess, 'run', return_value=SimpleNamespace(returncode=0)), \
                patch.object(business, 'api', return_value={'config_revision': 1}), \
                patch.object(business, 'record') as record:
            business.vpn_smoke('after_config_apply')
            business.vpn_smoke('after_rollback')
            assert popen.call_count == 2
            assert all('--user=t07-smoke' in call.args[0] for call in popen.call_args_list)
            assert [call.args[0] for call in record.call_args_list] == [
                'real_vpn_after_config_apply', 'real_vpn_after_rollback']


if __name__ == '__main__':
    main()
