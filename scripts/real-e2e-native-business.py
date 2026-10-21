#!/usr/bin/env python3
"""Agent-local evidence for the real native config, reload, and VPN path."""
import json
from pathlib import Path
import re
import subprocess
import sys
import time

done_path, password_path, result_path, work_path = map(Path, sys.argv[1:])
done = json.loads(done_path.read_text())
work_path.mkdir(parents=True, exist_ok=True)
result_path.parent.mkdir(parents=True, exist_ok=True)


def run(*args, input_bytes=None):
    result = subprocess.run(args, input=input_bytes, capture_output=True, check=False)
    if result.returncode:
        raise RuntimeError(f'{args[0]} failed with exit {result.returncode}')
    return result.stdout.decode().strip()


def uuid(value):
    assert re.fullmatch(r'[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}', value)
    return value


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

operations = []
for operation in done['operations']:
    command = uuid(operation['command_id']).replace('-', '')
    operation_id = uuid(operation['id'])
    journal = run('sudo', 'sqlite3', '-readonly', '/var/lib/ocservia-agent/agent.db',
                  'SELECT count(*),state,error_code,length(privileged_result_proof)>0 '
                  f"FROM command_journal WHERE hex(command_id)=upper('{command}');")
    privd = run('sudo', 'sqlite3', '-readonly', '/var/lib/ocservia-privd/desired-effects.sqlite3',
                'SELECT count(*),state,length(response)>0 '
                f"FROM authorized_effects WHERE hex(command_id)=upper('{command}');")
    assert journal == '1|succeeded||1' and privd == '1|applied|1'
    operations.append({'id': operation_id, 'command_id': operation['command_id'],
                       'journal': journal, 'privd': privd})

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
                       stdin=subprocess.PIPE, stdout=log, stderr=log)
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
finally:
    subprocess.run(['sudo', 'ip', 'netns', 'exec', 'real-e2e-client',
                    'pkill', '-INT', '-x', 'openconnect'], capture_output=True)
    vpn.wait(timeout=15)
    log.close()
