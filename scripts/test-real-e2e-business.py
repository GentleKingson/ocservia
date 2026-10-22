#!/usr/bin/env python3
"""Small fail-closed check for cross-VM business evidence reconciliation."""
import importlib.util
import json
import os
from pathlib import Path
import subprocess
import tempfile

ROOT = Path(__file__).resolve().parents[1]
with tempfile.TemporaryDirectory(prefix='real-e2e-business-') as directory:
    work = Path(directory)
    os.environ.update(T07_WORK=str(work), T07_WORKSPACE='019d0000-0000-7000-8000-000000000001',
                      ARTIFACT_DIR=str(work))
    subprocess.run(['openssl', 'req', '-x509', '-newkey', 'rsa:2048', '-nodes', '-days', '1',
                    '-subj', '/CN=test-ca', '-keyout', str(work / 'ca.key'),
                    '-out', str(work / 'ca.crt')], capture_output=True, check=True)
    spec = importlib.util.spec_from_file_location('real_e2e_business',
                                                   ROOT / 'scripts/real-e2e-business.py')
    harness = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(harness)
    node = '019d0000-0000-7000-8000-000000000002'
    operation = '019d0000-0000-7000-8000-000000000003'
    command = '019d0000-0000-7000-8000-000000000004'
    expected = {'node_id': node, 'agent_endpoint_id': 'a' * 64,
                'config_hash': 'b' * 64, 'operations': [{'id': operation, 'command_id': command}]}
    done = work / 'outbox/business-controller-done'
    done.mkdir(parents=True)
    (done / 'done.json').write_text(json.dumps(expected))
    (work / 'state').mkdir()
    (work / 'state/node-approval-id').write_text('019d0000-0000-7000-8000-000000000005\n')
    result_dir = work / 'received'
    result_dir.mkdir()
    result = {**expected, 'native_services': ['ocserv', 'ocservia-agent', 'ocservia-privd'],
              'native_reload_events': 1,
              'vpn': {'connected': True, 'native_session': 'session-1', 'icmp': True},
              'operations': [{**expected['operations'][0], 'journal': '1|succeeded||1',
                              'privd': '1|applied|1'}]}
    def api(path):
        assert path == 'operations/' + operation
        return {'state': 'succeeded', 'command_id': command}
    def sql(query):
        if 'FROM operations' in query:
            return 'succeeded'
        if 'FROM outbox_events' in query:
            return '1'
        if 'FROM command_attempts' in query:
            return '1'
        raise AssertionError(query)
    harness.business.api = api
    harness.business.sql = sql
    (result_dir / 'result.json').write_text(json.dumps(result))
    harness.verify_result(result_dir)
    assert 'CROSS_VM_PRIVATE_BUSINESS=PASS' in (work / 'business.txt').read_text()
    assert json.loads((work / 'business.json').read_text())['operations'][0]['journal'] == '1|succeeded||1'
    for broken in ({**result, 'config_hash': 'c' * 64},
                   {**result, 'native_reload_events': 0},
                   {**result, 'vpn': {**result['vpn'], 'icmp': False}},
                   {**result, 'operations': [{**result['operations'][0], 'journal': '0'}]}):
        (work / 'business.txt').unlink(missing_ok=True)
        (work / 'business.json').unlink(missing_ok=True)
        (result_dir / 'result.json').write_text(json.dumps(broken))
        try:
            harness.verify_result(result_dir)
        except AssertionError:
            pass
        else:
            raise AssertionError('accepted unproven native business evidence')
        assert not (work / 'business.txt').exists()
        assert not (work / 'business.json').exists()
    for state in ('queued', 'unknown', 'succeeded', 'failed'):
        plan = {'operation_id': operation, 'state': state, 'validation': 'pending',
                'current_unchanged': False, 'staging_cleaned': True}
        def plan_api(path):
            return plan if path == 'config-plans/test-plan' else {'state': state}
        harness.business.api = plan_api
        try:
            actual = harness.plan_result('test-plan')
            assert (actual is plan) == (state == 'succeeded')
        except RuntimeError:
            assert state == 'failed'
        status = json.loads((work / 'config-plan-status.json').read_text())
        assert status == {'state':state, 'operation_state':state, 'validation':'pending',
                          'current_unchanged':False, 'staging_cleaned':True}
print('cross-VM business evidence reconciliation and negative controls passed')
