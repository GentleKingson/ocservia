#!/usr/bin/python3
"""Container process supervision only; Agent, privd and Ocserv are real binaries."""
import json
import os
from pathlib import Path
import signal
import socket
import subprocess
import sys
import time

args = sys.argv[1:]
if 'ocserv.service' in args:
    os.execv('/usr/bin/ocservia-e2e-ocserv-systemctl', ['systemctl'] + args)
root = Path('/run/rebind-services')
if args[0] == 'list-units':
    sys.exit(0)
if args[0] == 'show':
    launcher = 'ocservia-agent-relays' if args[1] == 'ocservia-agent.service' else 'ocservia-privd'
    print('{ path=/usr/libexec/ocservia/' + launcher + ' ; argv[]=/usr/libexec/ocservia/' + launcher + ' ; }')
    sys.exit(0)
assert args[0] in ('start', 'stop')
for name in args[1:]:
    assert name in ('ocservia-agent.service', 'ocservia-privd.service')
    path = root / (name + '.json')
    config = json.loads(path.read_text())
    if args[0] == 'stop':
        if config.get('pid'):
            try:
                os.kill(config['pid'], signal.SIGTERM)
            except ProcessLookupError:
                pass
            for _ in range(150):
                try:
                    status = Path('/proc') / str(config['pid']) / 'stat'
                    if not status.exists() or status.read_text().split()[2] == 'Z':
                        break
                except FileNotFoundError:
                    break
                time.sleep(.1)
            else:
                raise RuntimeError('service failed to stop')
            config.pop('pid', None)
    elif 'pid' not in config:
        with open(root / (name + '.log'), 'ab', buffering=0) as log:
            p = subprocess.Popen(config['argv'], env=config['env'], user=config['uid'], group=65533,
                                 extra_groups=[], stdin=subprocess.DEVNULL, stdout=log, stderr=log,
                                 start_new_session=True)
        config['pid'] = p.pid
        if name == 'ocservia-privd.service':
            for _ in range(100):
                if p.poll() is not None:
                    raise RuntimeError('privd failed to start')
                try:
                    with socket.socket(socket.AF_UNIX) as probe:
                        probe.connect('/run/ocserv-platform/privd.sock')
                    break
                except OSError:
                    time.sleep(.1)
            else:
                raise RuntimeError('privd socket did not become ready')
    path.write_text(json.dumps(config))
