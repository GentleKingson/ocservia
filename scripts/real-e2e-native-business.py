#!/usr/bin/env python3
"""Agent-local evidence for native config, VPN and sole Relay recovery."""
import json
from pathlib import Path
import re
import subprocess
import sys
import time


def run(*args, input_bytes=None):
    result = subprocess.run(args, input=input_bytes, capture_output=True, check=False)
    if result.returncode:
        raise RuntimeError(f'{args[0]} failed with exit {result.returncode}')
    return result.stdout.decode().strip()


def uuid(value):
    assert re.fullmatch(r'[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}', value)
    return value


def operation_result(operation):
    command = uuid(operation['command_id']).replace('-', '')
    operation_id = uuid(operation['id'])
    journal = run('sudo', 'sqlite3', '-readonly', '/var/lib/ocservia-agent/agent.db',
                  'SELECT count(*),state,error_code,length(privileged_result_proof)>0 '
                  f"FROM command_journal WHERE hex(command_id)=upper('{command}');")
    privd = run('sudo', 'sqlite3', '-readonly', '/var/lib/ocservia-privd/desired-effects.sqlite3',
                'SELECT count(*),state,length(response)>0 '
                f"FROM authorized_effects WHERE hex(command_id)=upper('{command}');")
    assert journal == '1|succeeded||1' and privd == '1|applied|1'
    return {'id':operation_id, 'command_id':operation['command_id'], 'journal':journal, 'privd':privd}


def native_snapshot(work, expected):
    assert run('sudo', 'sha256sum', '/etc/ocserv/ocserv.conf').split()[0] == expected['config_hash']
    assert run('sudo', '-u', 'ocserv-agent', '/usr/libexec/ocservia/ocservia-agent',
               '--controller', (work / 'controller-endpoint-id').read_text().strip(),
               '--prepare-enrollment') == expected['agent_endpoint_id']
    services = {}
    for name in ('ocserv', 'ocservia-agent', 'ocservia-privd'):
        assert run('systemctl', 'is-active', name) == 'active'
        pid = int(run('systemctl', 'show', name, '-p', 'MainPID', '--value'))
        started = int(run('systemctl', 'show', name, '-p', 'ExecMainStartTimestampMonotonic', '--value'))
        assert pid > 0 and started > 0
        services[name] = {'pid':pid, 'started':started}
    argv = run('sudo', 'cat', f'/proc/{services["ocservia-agent"]["pid"]}/cmdline').rstrip('\0').split('\0')
    assert [argv[i+1] for i, arg in enumerate(argv) if arg == '--relay-url'] == [(work / 'relay/url').read_text().strip()]
    assert argv[argv.index('--relay-mode')+1] == 'custom'
    uid = run('id', '-u', 'ocserv-agent')
    for tool in ('iptables', 'ip6tables') if Path('/proc/net/if_inet6').read_text().strip() else ('iptables',):
        run('sudo', tool, '-C', 'OUTPUT', '-m', 'owner', '--uid-owner', uid,
            '-p', 'udp', '!', '--dport', '53', '-j', 'REJECT')
    identity = {}
    for name in ('endpoint.key', 'controller.endpoint'):
        digest = run('sudo', 'sha256sum', '/var/lib/ocservia-agent/identity/' + name).split()[0]
        assert re.fullmatch('[0-9a-f]{64}', digest)
        identity[name] = digest
    run('sudo', 'ip', 'netns', 'exec', 'real-e2e-client', 'ping', '-c', '3', '-W', '2', '10.208.0.1')
    users = json.loads(run('sudo', 'occtl', '--json', 'show', 'users'))
    sessions = [str(user['ID']) for user in users if user['Username'] == 't07-smoke']
    assert sessions == [expected['vpn']['native_session']]
    return {'identity':identity, 'services':services, 'vpn_session':sessions[0], 'icmp':True,
            'reloads':run('sudo', 'journalctl', '--no-pager', '-u', 'ocserv', '-o', 'cat').count('Reloaded ocserv.service'),
            'single_agent_relay':True, 'udp_isolated':True}


def recovery(phase, source, result_path, work):
    expected = json.loads((work / 'outbox/business-node-result/result.json').read_text())
    value = json.loads((source / 'result.json').read_text())
    assert value['node_id'] == expected['node_id']
    baseline_path = work / 'recovery-baseline.json'
    if phase == 'baseline':
        assert value['agent_endpoint_id'] == expected['agent_endpoint_id'] and value['path_mode'] == 'relay'
        snapshot = native_snapshot(work, expected)
        baseline_path.write_text(json.dumps(snapshot))
        result = {'node_id':expected['node_id'], 'agent_endpoint_id':expected['agent_endpoint_id'], 'before':snapshot}
    elif phase == 'queued':
        before = json.loads(baseline_path.read_text())
        assert native_snapshot(work, expected) == before
        command = uuid(value['operation']['command_id']).replace('-', '')
        uuid(value['operation']['id'])
        for database, table in (('/var/lib/ocservia-agent/agent.db', 'command_journal'),
                                ('/var/lib/ocservia-privd/desired-effects.sqlite3', 'authorized_effects')):
            assert run('sudo', 'sqlite3', '-readonly', database,
                       f"SELECT count(*) FROM {table} WHERE hex(command_id)=upper('{command}');") == '0'
        probe = subprocess.run(['curl', '--silent', '--show-error', '--noproxy', '*', '--connect-timeout', '3',
                                '--max-time', '5', '--cacert', str(work / 'relay/ca.pem'),
                                (work / 'relay/url').read_text().strip()], capture_output=True)
        assert probe.returncode in (7, 28), 'stopped Relay remained reachable or failed for another reason'
        result = {'queued':value, 'queued_native':{'journal':0, 'privd':0, 'reload_delta':0,
                                                  'vpn_unchanged':True, 'relay_unreachable':True}}
        (work / 'recovery-queued.json').write_text(json.dumps(result))
    elif phase == 'result':
        before = json.loads(baseline_path.read_text())
        queued = json.loads((work / 'recovery-queued.json').read_text())
        assert value['operation'] == queued['queued']['operation'] and value['idempotent_replay'] is True
        time.sleep(3)
        after = native_snapshot(work, expected)
        assert before['identity'] == after['identity'] and before['services'] == after['services']
        assert before['vpn_session'] == after['vpn_session'] and after['reloads'] == before['reloads'] + 1
        result = {**queued, 'node_id':expected['node_id'], 'agent_endpoint_id':expected['agent_endpoint_id'],
                  'before':before, 'after':after, 'operations':[operation_result(value['operation'])]}
    else:
        raise ValueError('unknown native recovery phase')
    result_path.parent.mkdir(parents=True, exist_ok=True)
    result_path.write_text(json.dumps(result, sort_keys=True) + '\n')


def business(done_path, password_path, result_path, work_path):
    done = json.loads(done_path.read_text())
    work_path.mkdir(parents=True, exist_ok=True)
    result_path.parent.mkdir(parents=True, exist_ok=True)
    node = uuid(done['node_id'])
    assert re.fullmatch(r'[0-9a-f]{64}', done['agent_endpoint_id'])
    assert re.fullmatch(r'[0-9a-f]{64}', done['config_hash'])
    assert run('sudo', '-u', 'ocserv-agent', '/usr/libexec/ocservia/ocservia-agent',
               '--controller', (work_path / 'controller-endpoint-id').read_text().strip(),
               '--prepare-enrollment') == done['agent_endpoint_id']
    assert run('sudo', 'sha256sum', '/etc/ocserv/ocserv.conf').split()[0] == done['config_hash']
    for service in ('ocserv', 'ocservia-agent', 'ocservia-privd'):
        assert run('systemctl', 'is-active', service) == 'active'
        assert int(run('systemctl', 'show', service, '-p', 'MainPID', '--value')) > 0
    reload_events = run('sudo', 'journalctl', '--no-pager', '-u', 'ocserv', '-o', 'cat').count('Reloaded ocserv.service')
    assert reload_events >= 1

    operations = [operation_result(operation) for operation in done['operations']]

    # The dedicated netns reaches only the local native ocserv listener over veth.
    run('sudo', 'ip', 'netns', 'add', 'real-e2e-client')
    run('sudo', 'ip', 'link', 'add', 'real-e2e-host', 'type', 'veth', 'peer', 'name', 'real-e2e-peer')
    run('sudo', 'ip', 'link', 'set', 'real-e2e-peer', 'netns', 'real-e2e-client')
    run('sudo', 'ip', 'addr', 'add', '10.207.0.1/30', 'dev', 'real-e2e-host')
    run('sudo', 'ip', 'link', 'set', 'real-e2e-host', 'up')
    run('sudo', 'ip', 'netns', 'exec', 'real-e2e-client', 'ip', 'addr', 'add', '10.207.0.2/30', 'dev', 'real-e2e-peer')
    run('sudo', 'ip', 'netns', 'exec', 'real-e2e-client', 'ip', 'link', 'set', 'real-e2e-peer', 'up')
    run('sudo', 'ip', 'netns', 'exec', 'real-e2e-client', 'ip', 'link', 'set', 'lo', 'up')
    script = work_path / 'vpn-script'
    script.write_text('#!/bin/sh\nset -eu\nif [ "$reason" = connect ]; then\n'
                      '  ip link set "$TUNDEV" up\n'
                      '  ip addr add "$INTERNAL_IP4_ADDRESS/24" dev "$TUNDEV"\nfi\n')
    script.chmod(0o700)
    log = (work_path / 'vpn-client.log').open('wb')
    vpn = subprocess.Popen(['sudo', 'ip', 'netns', 'exec', 'real-e2e-client', 'openconnect',
                            '--non-inter', '--protocol=anyconnect', '--user=t07-smoke',
                            '--passwd-on-stdin', '--cafile', str(work_path / 'vpn-ca.crt'),
                            '--script', str(script), 'https://10.207.0.1:44443'],
                           stdin=subprocess.PIPE, stdout=log, stderr=log, start_new_session=True)
    keep_vpn = False
    try:
        vpn.stdin.write(password_path.read_bytes().strip() + b'\n')
        vpn.stdin.close()
        deadline = time.monotonic() + 50
        session = None
        icmp = False
        while time.monotonic() < deadline:
            if vpn.poll() is not None:
                raise RuntimeError('native OpenConnect client exited before VPN acceptance')
            ping = subprocess.run(['sudo', 'ip', 'netns', 'exec', 'real-e2e-client', 'ping',
                                   '-c', '2', '-W', '2', '10.208.0.1'], capture_output=True)
            icmp = ping.returncode == 0
            users = json.loads(run('sudo', 'occtl', '--json', 'show', 'users'))
            matching = [str(user['ID']) for user in users if user['Username'] == 't07-smoke']
            if icmp and len(matching) == 1:
                session = matching[0]
                break
            time.sleep(2)
        assert icmp and session, 'native VPN session and ICMP were not both observed'
        result_path.write_text(json.dumps({
            'node_id': node, 'agent_endpoint_id': done['agent_endpoint_id'],
            'config_hash': done['config_hash'],
            'native_services': ['ocserv', 'ocservia-agent', 'ocservia-privd'],
            'native_reload_events': reload_events,
            'operations': operations,
            'vpn': {'connected': True, 'native_session': session, 'icmp': True}}) + '\n')
        keep_vpn = True
    finally:
        # Successful B leaves this same session alive for R; job cleanup owns it.
        if not keep_vpn:
            subprocess.run(['sudo', 'ip', 'netns', 'exec', 'real-e2e-client',
                            'pkill', '-INT', '-x', 'openconnect'], capture_output=True)
            vpn.wait(timeout=15)
        log.close()


if __name__ == '__main__':
    if sys.argv[1].startswith('recovery-'):
        recovery(sys.argv[1].removeprefix('recovery-'), *map(Path, sys.argv[2:]))
    else:
        business(*map(Path, sys.argv[1:]))
