#!/usr/bin/env python3
"""Focused checks for Business Smoke and manual integration."""
import importlib.util
import json
import os
import subprocess
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

        # Exercise the driver's real profile branch with external commands
        # stubbed: no native installation, browser, or business mutation here.
        driver = (root / 'scripts/release-business-probe.sh').read_text()
        browser_stage = driver.split('record native_node_services\n', 1)[1].split('next_stage vpn_client_setup', 1)[0]
        shell = r"""
set -euo pipefail
ROOT="$1"
work="$2"
BUSINESS_PROFILE="$3"
next_stage() { :; }
record() { echo "checkpoint:$1"; }
python3() { echo "api:$2"; if [[ "$2" == browser_verify ]]; then return "${VERIFY_EXIT}"; fi; }
node() { echo browser; return "${BROWSER_EXIT}"; }
npm() { :; }
npx() { :; }
cd() { :; }
sudo() { :; }
mkdir() { :; }
certutil() { :; }
"""
        for profile in ('smoke', 'extended'):
            result = subprocess.run(['bash', '-c', shell + browser_stage, 'browser-stage', str(root), str(work), profile],
                                    env={**os.environ, 'BROWSER_EXIT': '0', 'VERIFY_EXIT': '0'}, capture_output=True, text=True, check=True)
            expected = (['api:certificate', 'checkpoint:real_certificate_lifecycle'] if profile == 'extended' else [])
            expected += ['api:browser_prepare', 'browser', 'api:browser_verify', 'checkpoint:real_browser_subset']
            if profile == 'smoke':
                expected += ['api:smoke_user', 'checkpoint:approved_config_apply_and_vpn_user']
            assert result.stdout.splitlines() == expected
            for failure in ('BROWSER_EXIT', 'VERIFY_EXIT'):
                failed = subprocess.run(['bash', '-c', shell + browser_stage, 'browser-stage', str(root), str(work), profile],
                                        env={**os.environ, 'BROWSER_EXIT': '0', 'VERIFY_EXIT': '0', failure: '9'},
                                        capture_output=True, text=True)
                assert failed.returncode == 9
                assert 'checkpoint:real_browser_subset' not in failed.stdout


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

        # One restart must cover both processes, and success requires a fresh
        # session and unchanged identity, not only active systemd units.
        with patch.dict(os.environ, env), patch.object(business, 'run', return_value='active') as command, \
                patch.object(business, 'owner', return_value={'owner_epoch': 1}), \
                patch.object(business, 'fresh_owner', return_value={'owner_epoch': 2}), \
                patch.object(business, 'identity_digest', return_value='same'), \
                patch.object(business, 'record') as record:
            business.agent_stack_recovery()
            restarts = [c.args for c in command.call_args_list if 'restart' in c.args]
            assert restarts == [('sudo', 'systemctl', 'restart', 'ocservia-privd', 'ocservia-agent')]
            record.assert_called_once_with('resilience_agent')

        with patch.dict(os.environ, env), patch.object(business, 'run', return_value='active'), \
                patch.object(business, 'owner', return_value={'owner_epoch': 1}), \
                patch.object(business, 'fresh_owner', return_value=None), \
                patch.object(business, 'identity_digest', return_value='same'), \
                patch.object(business.time, 'monotonic', side_effect=[0, 0, 0, 0, 0, 0, 121]), \
                patch.object(business.time, 'sleep'), patch.object(business, 'record') as record:
            try:
                business.agent_stack_recovery()
            except RuntimeError as error:
                assert 'fresh Agent stack session' in str(error)
            else:
                raise AssertionError('stale session accepted after stack restart')
            record.assert_not_called()

        with patch.dict(os.environ, env), patch.object(business, 'approval', return_value='approval'), \
                patch.object(business, 'api', side_effect=[{'version': 1}, {'id': 'operation'}]) as api_call, \
                patch.object(business, 'completed', return_value=True), patch.object(business, 'run') as native, \
                patch.object(business, 'verify_completed_operations') as verify:
            assert business.authorized_reload('reconnected')['id'] == 'operation'
            assert api_call.call_count == 2  # node revision and one mutation; no replay
            native.assert_not_called()
            verify.assert_not_called()

        events = []
        def recovery_command(*args):
            events.append(args)
            return ''
        def recovery_api(path, **kwargs):
            if path == 'readyz':
                assert kwargs['status'] == 503
                return {'type': 'https://ocservia.dev/problems/database-unavailable'}
            return {'connection_state': 'online'}
        with patch.dict(os.environ, env), patch.object(business, 'run', side_effect=recovery_command), \
                patch.object(business, 'api', side_effect=recovery_api), \
                patch.object(business, 'owner'), patch.object(business, 'fresh_owner', return_value=True), \
                patch.object(business, 'ready', return_value=True), patch.object(business, 'transport_ready'), \
                patch.object(business, 'agent_stack_recovery'), patch.object(business, 'record') as record, \
                patch.object(business, 'authorized_reload', side_effect=lambda name: events.append(('operation', name))), \
                patch.object(business, 'vpn_smoke') as vpn, patch.object(business, 'cross_check') as cross:
            business.resilience()
            assert events[0][-3:] == ('restart', 'control-plane', 'transportd')
            stop = next(i for i, event in enumerate(events) if event[-2:] == ('stop', 'postgres'))
            start = next(i for i, event in enumerate(events) if event[-2:] == ('start', 'postgres'))
            assert start == stop + 1  # no outage mutation
            assert events[-1] == ('operation', 'database reconnect business')
            assert [c.args[0] for c in record.call_args_list] == ['resilience_controller', 'resilience_database']
            vpn.assert_called_once_with('relay_recovery', relay_recovery=True)
            cross.assert_not_called()

        for expired in (True, False):
            events.clear()
            def relay_wait(description, fn, seconds=120):
                events.append(('wait', description))
                result = fn()
                if not result:
                    raise RuntimeError(description)
                return result
            with patch.dict(os.environ, {**env, 'T07_RELAY_CONTAINER': 'relay'}), \
                    patch.object(business, 'single_relay_argv'), patch.object(business, 'owner', return_value='old'), \
                    patch.object(business, 'run', side_effect=recovery_command), \
                    patch.object(business, 'api', side_effect=AssertionError('must not poll stale telemetry')), \
                    patch.object(business, 'sql', return_value='t' if expired else 'f') as sql, \
                    patch.object(business, 'fresh_owner', side_effect=lambda old: events.append(('fresh', old)) or True), \
                    patch.object(business, 'wait_for', side_effect=relay_wait), \
                    patch.object(business, 'authorized_reload', side_effect=lambda name: events.append(('reload', name))), \
                    patch.object(business, 'record') as record:
                try:
                    business.smoke_relay_recovery(lambda: events.append(('vpn',)) or True)
                except RuntimeError as error:
                    assert not expired and 'owner lease invalidation' in str(error)
                else:
                    assert expired, 'live lease must prevent recovery success'
                sql.assert_called_once()
                assert "node_id=decode('node','hex')" in sql.call_args.args[0]
                assert 'lease_until>clock_timestamp()' in sql.call_args.args[0]
                expected = [('docker', 'stop', 'relay'),
                            ('wait', 'single Relay owner lease invalidation'),
                            ('docker', 'start', 'relay')]
                if expired:
                    expected += [('wait', 'fresh Agent session after Relay recovery'), ('fresh', 'old'),
                                 ('reload', 'Relay reconnect business'),
                                 ('wait', 'VPN after Relay recovery'), ('vpn',)]
                    assert [c.args[0] for c in record.call_args_list] == [
                        'single_relay_owner_lease_invalidated', 'resilience_relay']
                else:
                    record.assert_not_called()
                assert events == expected

        def api(path, *_args, **_kwargs):
            if path.endswith('/users'):
                return {'id': 'operation'}
            if path == 'operations/operation':
                return {'id': 'operation', 'state': 'succeeded', 'config_apply_state': 'succeeded'}
            if path == 'nodes/node':
                return {'config_revision': 1}
            if path == 'config-plans/plan':
                return {'materialized_hash': 'newhash'}
            raise AssertionError(path)

        browser_entries = [
            {'name': 'browser_user_mutation', 'operation': {'id': 'operation'}},
            {'name': 'browser_approved_reload', 'operation': {'id': 'operation'}},
            {'name': 'browser_consumed_approval_cannot_authorize_new_operation', 'operation_id': 'operation'},
            {'name': 'browser_complete_config_apply', 'operation': {'id': 'operation'},
             'plan_id': 'plan', 'materialized_hash': 'newhash'},
            {'name': 'browser_login_workspace_forbidden_refresh', 'status': 'PASS'},
            {'name': 'browser_bound_approval', 'action': 'service.reload'},
            {'name': 'browser_bound_approval', 'action': 'config.apply'},
        ]
        certificate_entries = [
            {'name': name, 'operation_id': 'operation'} for name in
            ('browser_certificate_csr', 'browser_certificate_issue_export_one_use', 'browser_certificate_revoke')]
        snapshot = {'database_state': 'succeeded', 'journal_count_state_error_receipt': '1|succeeded||1',
                    'root_count_state_response': '1|applied|1'}
        browser_file = work / 'evidence/browser-checkpoints.json'
        applied_file = work / 'smoke-applied.json'
        for profile in ('smoke', 'extended'):
            entries = browser_entries + (certificate_entries if profile == 'extended' else [])
            browser_file.write_text(json.dumps(entries))
            with patch.dict(os.environ, {**env, 'BUSINESS_PROFILE': profile}), \
                    patch.object(business, 'api', side_effect=api), \
                    patch.object(business, 'cross_check', return_value=snapshot), \
                    patch.object(business, 'run', return_value='newhash  /etc/ocserv/ocserv.conf'), \
                    patch.object(business, 'record') as record:
                business.browser_verify()
                record.assert_called_once_with('browser_operations_durable_cross_check')
                if profile == 'smoke':
                    assert json.loads(applied_file.read_text()) == {'operation_id': 'operation', 'materialized_hash': 'newhash'}
                    assert business.confirmed_config()[1] == 'newhash'
                    applied_file.unlink()
                else:
                    assert not applied_file.exists()

        # Missing/duplicate checkpoints and incomplete operations cannot grant
        # acceptance or create the evidence consumed by smoke recovery.
        invalid_entries = [[], browser_entries + [browser_entries[0]]]
        invalid_entries += [browser_entries[:i] + browser_entries[i + 1:] for i in range(len(browser_entries))]
        invalid_entries += [[{**entry, 'operation': {}} if entry['name'] == 'browser_user_mutation' else entry
                             for entry in browser_entries]]
        for entries in invalid_entries:
            browser_file.write_text(json.dumps(entries))
            with patch.dict(os.environ, env), patch.object(business, 'api', side_effect=api), \
                    patch.object(business, 'cross_check', return_value=snapshot), \
                    patch.object(business, 'record') as record:
                try:
                    business.browser_verify()
                except AssertionError:
                    pass
                else:
                    raise AssertionError('incomplete browser evidence accepted')
                record.assert_not_called()
                assert not applied_file.exists()

        browser_file.write_text(json.dumps(browser_entries))
        with patch.dict(os.environ, {**env, 'BUSINESS_PROFILE': 'extended'}), patch.object(business, 'record') as record:
            try:
                business.browser_verify()
            except AssertionError:
                pass
            else:
                raise AssertionError('extended profile accepted without certificate checks')
            record.assert_not_called()
        for path, bad_value in [('nodes/node', {'config_revision': 2}),
                                ('config-plans/plan', {'materialized_hash': 'wrong'}),
                                ('operations/operation', {'state': 'succeeded', 'config_apply_state': 'failed'})]:
            with patch.dict(os.environ, env), \
                    patch.object(business, 'api', side_effect=lambda requested, p=path, bad=bad_value: bad if requested == p else api(requested)), \
                    patch.object(business, 'cross_check', return_value=snapshot), \
                    patch.object(business, 'record') as record:
                try:
                    business.browser_verify()
                except AssertionError:
                    pass
                else:
                    raise AssertionError('unconfirmed configuration accepted')
                record.assert_not_called()
                assert not applied_file.exists()
        with patch.dict(os.environ, env), patch.object(business, 'api', side_effect=api), \
                patch.object(business, 'cross_check', return_value=snapshot), \
                patch.object(business, 'run', return_value='wrong  /etc/ocserv/ocserv.conf'), \
                patch.object(business, 'record') as record:
            try:
                business.browser_verify()
            except AssertionError:
                pass
            else:
                raise AssertionError('wrong native configuration accepted')
            record.assert_not_called()
            assert not applied_file.exists()

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
