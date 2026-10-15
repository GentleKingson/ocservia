#!/usr/bin/env python3
"""Focused checks for Business Smoke and manual integration."""
import hashlib
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
            {'name': 'browser_csp', 'flow_violations': 0, 'flow_console_reports': 0, 'flow_page_errors': 0,
             'control': ['script-src-elem/enforce', 'img-src/enforce']},
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
        # A flow violation, a collector that missed the control, or a control
        # that was only reported rather than enforced fails.
        for change in ({'flow_violations': 1}, {'flow_console_reports': 1}, {'control': ['img-src/enforce']},
                       {'control': ['script-src-elem/enforce', 'img-src/enforce', 'img-src/enforce']},
                       {'control': ['script-src-elem/report', 'img-src/report']}):
            invalid_entries += [[{**entry, **change} if entry['name'] == 'browser_csp' else entry
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

        # The rollout counts only when the canary succeeded under the bound
        # approval and the node and installed binaries report the target.
        rollout_env = {**env, 'BUSINESS_PROFILE': 'extended', 'VERSION': '0.0.0', 'ROLLOUT_VERSION': '0.0.1'}
        rollout_file = work / 'evidence/browser-rollout-checkpoints.json'
        rollout_entries = [
            {'name': 'browser_bound_approval', 'action': 'agent.rollout'},
            {'name': 'browser_agent_rollout', 'rollout': {'id': 'rollout', 'approval_id': 'approval', 'target_version': '0.0.1'}},
            {'name': 'browser_csp', 'flow_violations': 0, 'flow_console_reports': 0, 'flow_page_errors': 0,
             'control': ['script-src-elem/enforce', 'img-src/enforce']},
        ]
        rollout_state = {'agent-rollouts/rollout': {
                             'id': 'rollout', 'state': 'succeeded', 'target_version': '0.0.1', 'stop_on_failure': True,
                             'approval_id': 'approval',
                             'nodes': [{'node_id': 'node', 'batch': 0, 'state': 'succeeded', 'operation_id': 'upgrade'}]},
                         'approval-requests/approval': {'status': 'consumed'},
                         'nodes/node': {'connection_state': 'online', 'agent_version': '0.0.1'}}
        binaries = lambda path, _flag: f'{path.rsplit("/", 1)[1]} 0.0.1\n'
        rollout_file.write_text(json.dumps(rollout_entries))
        with patch.dict(os.environ, rollout_env), patch.object(business, 'api', side_effect=rollout_state.__getitem__), \
                patch.object(business, 'run', side_effect=binaries), patch.object(business, 'record') as record:
            business.rollout_verify()
            assert record.call_args.args[0] == 'agent_rollout_canary_succeeded'
        bad_rollouts = [
            ('approval', [{**rollout_entries[0], 'action': 'service.reload'}, *rollout_entries[1:]], {}),
            ('csp', [*rollout_entries[:2], {**rollout_entries[2], 'flow_violations': 1}], {}),
            ('csp report-only', [*rollout_entries[:2], {**rollout_entries[2], 'control': ['script-src-elem/report', 'img-src/report']}], {}),
            ('paused', rollout_entries, {'agent-rollouts/rollout': {**rollout_state['agent-rollouts/rollout'], 'state': 'paused'}}),
            ('approval not consumed', rollout_entries, {'approval-requests/approval': {'status': 'approved'}}),
        ]
        for label, entries, overrides in bad_rollouts:
            rollout_file.write_text(json.dumps(entries))
            with patch.dict(os.environ, rollout_env), \
                    patch.object(business, 'api', side_effect={**rollout_state, **overrides}.__getitem__), \
                    patch.object(business, 'run', side_effect=binaries), patch.object(business, 'record') as record:
                try:
                    business.rollout_verify()
                except AssertionError:
                    pass
                else:
                    raise AssertionError('rollout accepted: ' + label)
                record.assert_not_called()
        rollout_file.write_text(json.dumps(rollout_entries))
        with patch.dict(os.environ, rollout_env), patch.object(business, 'api', side_effect=rollout_state.__getitem__), \
                patch.object(business, 'run', side_effect=lambda path, _flag: f'{path.rsplit("/", 1)[1]} 0.0.0\n'), \
                patch.object(business, 'record') as record:
            try:
                business.rollout_verify()
            except AssertionError:
                pass
            else:
                raise AssertionError('rollout accepted with old installed binaries')
            record.assert_not_called()

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

        # The bootstrap source is protected and consumed by the installer; the
        # private copy only feeds redaction. Binding evidence must be exact.
        token = 'obt1_' + 'A' * 43
        with patch.dict(os.environ, env), \
                patch.object(business, 'api', return_value={'id': 'token-id', 'token': token}) as requests, \
                patch.object(business, 'sql', return_value='t'), patch.object(business, 'record') as record:
            business.bootstrap_token()
            assert 'expected_endpoint_id' not in requests.call_args.args[1]
            source = work / 'bootstrap/node-bootstrap-token'
            assert source.read_text() == token and source.stat().st_mode & 0o777 == 0o600
            assert (work / 'private/bootstrap-token').read_text() == token
            record.assert_called_once_with('bootstrap_token_created_unbound', token_id='token-id')
        binding = {'token_hash': hashlib.sha256(token.encode()).hexdigest(), 'bound_endpoint': 'e' * 64,
                   'unbound_at_creation': True, 'consumed_node_id': 'node', 'consumed': True}
        verify_env = {**env, 'T07_ENDPOINT': 'e' * 64}
        for label, row, status in [('accepted', binding, 'pending'),
                                   ('other endpoint', {**binding, 'bound_endpoint': 'f' * 64}, 'pending'),
                                   ('unconsumed', {**binding, 'consumed': False}, 'pending'),
                                   ('created bound', {**binding, 'unbound_at_creation': False}, 'pending'),
                                   ('active', binding, 'active')]:
            with patch.dict(os.environ, verify_env), \
                    patch.object(business, 'sql', side_effect=[json.dumps(row), status, 'e' * 64]), \
                    patch.object(business, 'record') as record:
                try:
                    business.bootstrap_verify()
                except AssertionError:
                    assert label != 'accepted', label
                    record.assert_not_called()
                else:
                    assert label == 'accepted', 'bootstrap evidence accepted: ' + label
                    assert record.call_args.args[0] == 'bootstrap_token_consumed_and_bound_pending'

        # Revocation passes only on synchronously applied trust, a closed and not readmitted
        # session, and old authority that leaves privd untouched; a 503 or a
        # timeout never stands in for that evidence.
        def revocation_case(synced=True, queued=True, readmitted='0', effects_change=False):
            state = {'revoked': False, 'effects': 0}
            def fake_api(path, body=None, *, role='requester', status=200, headers=None, **_kwargs):
                headers = headers or {}
                if path == 'nodes/node':
                    return {'connection_state': 'offline' if state['revoked'] else 'online',
                            'trust_status': 'revoked' if state['revoked'] else 'active', 'version': 7}
                if path == 'approval-requests':
                    return {'id': 'pending'}
                if path.startswith('approval-requests/'):
                    return {'status': 'consumed' if state['revoked'] and path.endswith('revoke') else 'approved'}
                if path == 'nodes/node/revocation':
                    if role == 'requester' and headers.get('X-Approval-ID') == 'revoke' and 'Origin' not in headers:
                        assert status == 200
                        state['revoked'] = True
                        if not synced:
                            raise RuntimeError('POST nodes/node/revocation: HTTP 503, expected 200')
                        return None
                    assert status in (400, 401, 403, 409)
                    return None
                if path == 'nodes/node/service:reload':
                    assert status == 404 and state['revoked']
                    return {'type': 'https://ocservia.dev/problems/not-found'}
                raise AssertionError(path)
            def fake_sql(query):
                if 'FROM node_trust_convergence' in query:
                    return 't' if queued else 'f'
                if 'owner_epoch>' in query:
                    return readmitted
                if 'lease_until>clock_timestamp()' in query:
                    return 't'
                if 'status||' in query:
                    return 'revoked|3'
                if 'FROM node_endpoint_keys' in query:
                    return 'revoked'
                if 'SELECT status FROM nodes' in query:
                    return 'active'
                if 'FROM operations' in query:
                    return '0'
                raise AssertionError(query)
            def fake_run(*args):
                if args[:2] == ('systemctl', 'show'):
                    return '42' if 'MainPID' in args else 'active'
                if 'journalctl' in args and 'ocservia-agent' in args:
                    return 'controller connection ended\n'
                if 'sqlite3' in args:
                    if state['revoked'] and effects_change:
                        state['effects'] += 1
                    return str(state['effects'])
                return ''
            clock = {'now': 0}
            with patch.dict(os.environ, env), patch.object(business, 'api', side_effect=fake_api), \
                    patch.object(business, 'sql', side_effect=fake_sql), patch.object(business, 'run', side_effect=fake_run), \
                    patch.object(business, 'owner', return_value={'owner_epoch': 5}), \
                    patch.object(business, 'approval', side_effect=lambda action, *_: 'revoke' if action == 'node.revoke' else 'stale'), \
                    patch.object(business.time, 'monotonic', side_effect=lambda: clock['now']), \
                    patch.object(business.time, 'sleep', side_effect=lambda delay: clock.update(now=clock['now'] + delay)), \
                    patch.object(business, 'record') as record:
                try:
                    business.revocation()
                except (AssertionError, RuntimeError):
                    return [c.args[0] for c in record.call_args_list], False
                return [c.args[0] for c in record.call_args_list], True
        names, passed = revocation_case()
        assert passed and names == ['revocation_unauthorized_and_unapproved_rejected',
                                    'node_revoked_trust_converged_and_disconnected',
                                    'revoked_node_old_authority_without_privd_effect']
        for label, case in [('trust sync pending', {'synced': False}), ('intent not queued', {'queued': False}), ('session readmitted', {'readmitted': '1'}),
                            ('privd effect', {'effects_change': True})]:
            names, passed = revocation_case(**case)
            assert not passed, 'revocation accepted: ' + label
            assert 'revoked_node_old_authority_without_privd_effect' not in names, label


        # Drive the actual terminal phase. No failed observation may emit its
        # final checkpoint; API errors are failures, never evidence of offline.
        for fault in (None, 'remove', 'package installed', 'process', 'timer', 'payload', 'identity', 'endpoint changed', 'online',
                      'api timeout', 'revoked', 'owner', 'readmitted', 'effect', 'ocserv'):
            state = {'removed': False}
            def native():
                active = not state['removed']
                return {'processes': ['42'] if active or fault == 'process' else [],
                        'units': [{'unit': unit, 'active': 'active' if active or fault == 'timer' else 'inactive'}
                                  for unit in ('ocservia-agent.service', 'ocservia-privd.service', 'ocservia-agent-retention.timer')]}
            def terminal_run(*args):
                if args[:3] == ('sudo', 'apt-get', 'remove'):
                    if fault == 'remove':
                        raise RuntimeError('apt remove failed')
                    state['removed'] = True
                    return 'Removing ocservia-agent'
                if args[0] == 'dpkg-query':
                    return '/usr/share/ocservia-agent/package' if '-L' in args else 'install ok installed'
                if 'find' in args:
                    return 'file'
                if 'test' in args and fault == 'payload':
                    raise RuntimeError('payload remains')
                if args[:2] == ('systemctl', 'show'):
                    return 'ActiveState=active\nMainPID=' + ('99' if state['removed'] and fault == 'ocserv' else '42') + '\n'
                return ''
            def terminal_api(*_args, **_kwargs):
                if state['removed'] and fault == 'api timeout':
                    raise TimeoutError('API timed out')
                return {'trust_status': 'revoked' if state['removed'] and fault == 'revoked' else 'active',
                        'connection_state': 'offline' if state['removed'] and fault != 'online' else 'online'}
            def terminal_sql(query):
                if 'owner_epoch>' in query:
                    return '1' if fault == 'readmitted' else '0'
                if 'NOT EXISTS' in query:
                    return 'f' if not state['removed'] or fault == 'owner' else 't'
                return 'active|3' if 'authorization_revision' in query else ('changed|active' if state['removed'] and fault == 'endpoint changed' else 'endpoint|active')
            def once(description, fn, seconds=120):
                value = fn()
                if not value:
                    raise RuntimeError('timeout: ' + description)
                return value
            with patch.dict(os.environ, {**env, 'T07_ENDPOINT': 'endpoint'}), \
                    patch.object(business, 'api', side_effect=terminal_api), \
                    patch.object(business, 'sql', side_effect=terminal_sql), \
                    patch.object(business, 'run', side_effect=terminal_run), \
                    patch.object(business, 'owner', return_value={'owner_epoch': 5}), \
                    patch.object(business, 'uninstall_native', side_effect=native), \
                    patch.object(business, 'uninstall_identity', side_effect=lambda: ['x /identity/endpoint.key', 'y /identity/controller.endpoint'] + (['changed'] if state['removed'] and fault == 'identity' else [])), \
                    patch.object(business, 'node_side_effects', side_effect=lambda: {'effects': int(state['removed'] and fault == 'effect')}), \
                    patch.object(business.subprocess, 'run', return_value=SimpleNamespace(returncode=0, stdout=b'install ok installed' if fault == 'package installed' else b'')), \
                    patch.object(business, 'wait_for', side_effect=once), patch.object(business.time, 'sleep'), \
                    patch.object(business, 'vpn_smoke') as vpn, patch.object(business, 'record') as record:
                passed = False
                try:
                    business.uninstall()
                    passed = True
                except (AssertionError, RuntimeError, TimeoutError):
                    pass
                assert passed == (fault is None), fault
                names = [call.args[0] for call in record.call_args_list]
                assert ('ocserv_unaffected' in names) == passed, fault
                assert vpn.call_count == int(passed), fault

        # Session and IP-ban operations pass only on the real occtl effect; an
        # operation that succeeds without changing ocserv must not pass. Ocserv
        # 1.2.4 has no terminate; from 1.4.2 terminate must revoke a cookie
        # that a replay proved still valid after the client died.
        cases = [((1, 2, 4), fault) for fault in (None, 'disconnect ignored', 'ban kept')]
        cases += [((1, 5, 0), fault) for fault in (None, 'terminate ignored', 'cookie kept', 'kill revokes cookie')]
        for version, fault in cases:
            native = {'sessions': ['7'], 'bans': set(), 'score': 0, 'cookies': {}, 'next': 8, 'clients': []}

            def new_session(native=native):
                native['next'] += 1
                native['sessions'].append(str(native['next']))
                return str(native['next'])

            class Client:
                def __init__(self, cookie, _log, native=native, new_session=new_session):
                    self.cookie, self.code, self.session = cookie, None, None
                    if native['cookies'].get(cookie):
                        self.session = new_session()
                        native['clients'].append(self)
                    else:
                        self.code = 1

                def poll(self):
                    return self.code

                def wait(self, timeout):
                    if self.code is None:
                        raise subprocess.TimeoutExpired('openconnect', timeout)
                    return self.code

            def mutation(path, body, revision, native=native, new_session=new_session):
                assert revision == 3 and body == ({} if 'ip-bans' in path else {'boot_id': 'boot'})
                target, action = path.split('/', 1)[1].split(':')
                client = next((item for item in native['clients'] if item.code is None and item.session == target), None)
                if action == 'remove' and fault != 'ban kept':
                    native['bans'].discard(target)
                elif action == 'disconnect' and fault != 'disconnect ignored':
                    native['sessions'].remove(target)
                elif action == 'terminate' and fault != 'terminate ignored':
                    native['sessions'].remove(target)
                    if fault == 'cookie kept':
                        client.session = new_session()
                    else:
                        native['cookies'][client.cookie], client.code = False, 1
                return {'id': action}, {}

            def attempt(password, native=native):
                if '10.207.0.2' in native['bans']:
                    return SimpleNamespace(returncode=1, stdout=b'')
                if password == 'valid':
                    cookie = f"c{len(native['cookies'])}"
                    native['cookies'][cookie] = True
                    return SimpleNamespace(returncode=0, stdout=f"COOKIE='{cookie}'\n".encode())
                native['score'] += 10
                if native['score'] >= 80:
                    native['bans'].add('10.207.0.2')
                return SimpleNamespace(returncode=1, stdout=b'')

            def control_api(path, *_args, native=native, **_kwargs):
                if path == 'nodes/node':
                    return {'boot_id': 'boot', 'version': 3}
                if path.endswith('/sessions'):
                    return {'items': [{'id': item} for item in native['sessions']]}
                return {'items': [{'ip': item} for item in native['bans']]}

            def stopped(argv, *_args, native=native, **_kwargs):
                # SIGINT logs every client off, invalidating its cookie; SIGKILL
                # ends the sessions without a logoff, so cookies stay valid.
                killed = '-KILL' in argv
                native['sessions'].clear()
                for client in native['clients']:
                    if client.code is None:
                        client.code = -9 if killed else 0
                        if not killed or fault == 'kill revokes cookie':
                            native['cookies'][client.cookie] = False

            def bounded_wait(description, fn, seconds=120):
                result = fn()
                if not result:
                    raise RuntimeError(description)
                return result

            with patch.dict(os.environ, env), patch.object(business, 'api', side_effect=control_api), \
                    patch.object(business, 'native_bans', side_effect=lambda native=native: sorted(native['bans'])), \
                    patch.object(business, 'wait_for', side_effect=bounded_wait), \
                    patch.object(business, 'ocserv_version', return_value=version), \
                    patch.object(business, 'cookie_client', side_effect=Client), \
                    patch.object(business.subprocess, 'run', side_effect=stopped), \
                    patch.object(business, 'record') as record:
                try:
                    business.session_and_ban_controls(mutation, lambda native=native: list(native['sessions']),
                                                      attempt, 'valid')
                except (AssertionError, RuntimeError):
                    assert fault, 'real session and ban controls rejected'
                else:
                    assert fault is None, 'missing native effect accepted: ' + fault
                names = [c.args[0] for c in record.call_args_list]
                assert ('ip_ban_remove_native_effect' in names) == (fault is None), fault
                assert ('session_disconnect_native_effect' in names) == (fault != 'disconnect ignored'), fault
                assert ('session_terminate_native_effect' in names) == (fault is None and version >= (1, 4, 2)), fault
                assert ('session_cookie_replay_control' in names) == (version >= (1, 4, 2) and fault in (None, 'terminate ignored', 'cookie kept')), fault

if __name__ == '__main__':
    main()
