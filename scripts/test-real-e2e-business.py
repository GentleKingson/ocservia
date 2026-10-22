#!/usr/bin/env python3
"""Small fail-closed check for cross-VM business evidence reconciliation."""
import importlib.util
import copy
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
    (work / 'state/node-id').write_text(node + '\n')
    (work / 'state/agent-endpoint-id').write_text('a' * 64 + '\n')
    (work / 'private').mkdir()
    (work / 'private/privd-credential').write_text('private-test-credential')
    (result_dir / 'ready.json').write_text(json.dumps({'node_id':node, 'agent_endpoint_id':'a'*64}))
    (result_dir / 'proof.json').write_text(json.dumps({'signature':'public-test-proof'}))
    confirmation = work / 'outbox/business-authority-registered/ready.json'
    def register_api(path, proof, **kwargs):
        assert path == f'nodes/{node}/privd-attestation-keys:register'
        assert proof == {'signature':'public-test-proof', 'credential':'private-test-credential'}
        assert kwargs == {'role':'anonymous', 'status':201}
        raise RuntimeError('registration rejected')
    harness.business.api = register_api
    try:
        harness.register_authority(result_dir)
    except RuntimeError:
        pass
    else:
        raise AssertionError('accepted failed root registration')
    assert not confirmation.exists()
    harness.business.api = lambda *args, **kwargs: None
    harness.register_authority(result_dir)
    assert json.loads(confirmation.read_text()) == {'node_id':node, 'agent_endpoint_id':'a'*64}
    assert 'private-test-credential' not in confirmation.read_text()
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
    harness.business.api = api
    baseline = {'node_id':node, 'agent_endpoint_id':'a'*64, 'path_mode':'relay',
                'single_controller_relay':True, 'owner':{'connection_id':'1'*32, 'owner_epoch':1}}
    queued = {'node_id':node, 'operation':expected['operations'][0], 'owner_lease_invalid':True,
              'offline':True, 'state':'queued', 'unpublished_outbox':1, 'sent_attempts':0}
    done = {**queued, 'owner':{'connection_id':'2'*32, 'owner_epoch':2}, 'idempotent_replay':True}
    for phase, value in (('baseline',baseline), ('queued',queued), ('done',done)):
        target=work / ('outbox/recovery-controller-'+phase)
        target.mkdir(parents=True)
        (target/'result.json').write_text(json.dumps(value))
    snapshot = {'identity':{'endpoint.key':'c'*64,'controller.endpoint':'d'*64},
                'services':{name:{'pid':42,'started':1} for name in ('ocserv','ocservia-agent','ocservia-privd')},
                'vpn_session':'session-1', 'icmp':True, 'reloads':2,
                'udp_isolated':True, 'single_agent_relay':True}
    recovered = {'node_id':node, 'agent_endpoint_id':'a'*64, 'queued':queued,
                 'queued_native':{'journal':0,'privd':0,'reload_delta':0,'vpn_unchanged':True,'relay_unreachable':True},
                 'before':snapshot, 'after':{**snapshot,'reloads':3}, 'operations':result['operations']}
    (work/'business.json').write_text(json.dumps({'vpn_native_session':'session-1'}))
    (result_dir/'result.json').write_text(json.dumps(recovered))
    harness.recovery('verify', result_dir)
    assert 'DEDICATED_RELAY_RECOVERY=PASS' in (work/'recovery.txt').read_text()
    for path, wrong in ((('after','reloads'),4), (('after','vpn_session'),'different'),
                        (('after','identity'),{'endpoint.key':'d'*64}),
                        (('after','services'),{'ocserv':{'pid':43,'started':2}}),
                        (('before','udp_isolated'),False), (('queued_native','journal'),1),
                        (('queued_native','relay_unreachable'),False),
                        (('queued','sent_attempts'),1), (('node_id',),'wrong')):
        broken=copy.deepcopy(recovered)
        parent=broken
        for key in path[:-1]: parent=parent[key]
        parent[path[-1]]=wrong
        (result_dir/'result.json').write_text(json.dumps(broken))
        (work/'recovery.txt').unlink(missing_ok=True)
        (work/'recovery.json').unlink(missing_ok=True)
        try: harness.recovery('verify', result_dir)
        except AssertionError: pass
        else: raise AssertionError('accepted unproven Relay recovery: '+str(path))
        assert not (work/'recovery.txt').exists() and not (work/'recovery.json').exists()

    # Run the Node phase checks with fixed local command answers, never host services.
    from types import SimpleNamespace
    native_spec=importlib.util.spec_from_file_location('native_business',ROOT/'scripts/real-e2e-native-business.py')
    native=importlib.util.module_from_spec(native_spec)
    native_spec.loader.exec_module(native)
    native.time=SimpleNamespace(sleep=lambda _:None)
    native.subprocess=SimpleNamespace(run=lambda *args,**kwargs:SimpleNamespace(returncode=7))
    native_done=work/'outbox/business-node-result'
    native_done.mkdir()
    (native_done/'result.json').write_text(json.dumps(result))
    (work/'controller-endpoint-id').write_text('e'*64)
    (work/'relay').mkdir()
    (work/'relay/url').write_text('https://relay.example.test:3443')
    reloads=[2]
    journal_rows=[0]
    def native_run(*args,**kwargs):
        if args[0]=='systemctl':
            return 'active' if args[1]=='is-active' else '42' if args[-2]=='MainPID' else '1'
        if args[0]=='id': return '65533'
        if args[1]=='sha256sum': return ('b'*64 if args[-1]=='/etc/ocserv/ocserv.conf' else 'c'*64)+' file'
        if '--prepare-enrollment' in args: return 'a'*64
        if args[1]=='cat': return 'agent\0--relay-mode\0custom\0--relay-url\0https://relay.example.test:3443\0'
        if args[1] in ('iptables','ip6tables','ip'): return ''
        if args[1]=='occtl': return json.dumps([{'ID':'session-1','Username':'t07-smoke'}])
        if args[1]=='journalctl': return 'Reloaded ocserv.service\n'*reloads[0]
        if args[1]=='sqlite3':
            if args[-1].startswith('SELECT count(*),state,error_code'): return '1|succeeded||1'
            if args[-1].startswith('SELECT count(*),state,length'): return '1|applied|1'
            return str(journal_rows[0])
        raise AssertionError(args)
    native.run=native_run
    local_result=work/'native-recovery/result.json'
    native.recovery('baseline',work/'outbox/recovery-controller-baseline',local_result,work)
    native.recovery('queued',work/'outbox/recovery-controller-queued',local_result,work)
    for rows, curl_status in ((1,7),(0,0)):
        local_result.unlink(missing_ok=True)
        journal_rows[0]=rows
        native.subprocess=SimpleNamespace(run=lambda *a,**k:SimpleNamespace(returncode=curl_status))
        try: native.recovery('queued',work/'outbox/recovery-controller-queued',local_result,work)
        except AssertionError: pass
        else: raise AssertionError('accepted an offline native effect or reachable Relay')
        assert not local_result.exists()
    journal_rows[0]=0
    reloads[0]=3
    native.recovery('result',work/'outbox/recovery-controller-done',local_result,work)
    assert json.loads(local_result.read_text())['operations']==result['operations']
print('cross-VM business and Relay recovery reconciliation/negative controls passed (local contracts only)')
