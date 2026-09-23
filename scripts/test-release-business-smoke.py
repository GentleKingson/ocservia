#!/usr/bin/env python3
"""Focused contract checks for the two real-VPN smoke phases."""
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

        def rollback_api(path, *_args, **_kwargs):
            if path.endswith('/apply'):
                return {'id': 'operation'}
            if path == 'operations/operation':
                return {'config_apply_state': 'rolled_back'}
            if path == 'nodes/node':
                return {'config_revision': 1}
            raise AssertionError(path)

        def native_command(*args):
            return 'newhash  /etc/ocserv/ocserv.conf' if args[1] == 'sha256sum' else ''

        with patch.dict(os.environ, env), patch.object(business, 'smoke_plan', return_value={
            'id': 'plan', 'materialized_hash': 'failedhash'}), patch.object(business, 'approval', return_value='approval'), \
                patch.object(business, 'api', side_effect=rollback_api), patch.object(business, 'run', side_effect=native_command), \
                patch.object(business, 'record') as record:
            business.smoke_rollback()
            record.assert_called_once_with('smoke_config_plan_rolled_back', operation_id='operation', state='rolled_back')

        vpn = MagicMock()
        vpn.poll.return_value = None
        with patch.dict(os.environ, env), patch.object(business.subprocess, 'Popen', return_value=vpn) as popen, \
                patch.object(business.subprocess, 'run', return_value=SimpleNamespace(returncode=0)), \
                patch.object(business, 'api', return_value={'config_revision': 1}), \
                patch.object(business, 'record') as record:
            business.vpn_smoke('before_rollback')
            business.vpn_smoke('after_rollback')
            assert popen.call_count == 2
            assert all('--user=t07-smoke' in call.args[0] for call in popen.call_args_list)
            assert [call.args[0] for call in record.call_args_list] == [
                'real_vpn_before_rollback', 'real_vpn_after_rollback']


if __name__ == '__main__':
    main()
