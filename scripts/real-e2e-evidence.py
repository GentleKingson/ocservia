#!/usr/bin/env python3
"""Reject same-host, wrong-role, or mixed-commit hosted rendezvous evidence."""
import json
import pathlib
import sys
import uuid


def validate(local, peer):
    for item in (local, peer):
        if str(uuid.UUID(item['boot_id'])) != item['boot_id']:
            raise ValueError('invalid boot ID')
        if (item['runner_environment'], item['runner_os'], item['pid1']) != ('github-hosted', 'ubuntu-24.04', 'systemd'):
            raise ValueError('unexpected runner')
        if not item['default_routes'] or not item['network_namespace']:
            raise ValueError('missing network evidence')
    if {local['role'], peer['role']} != {'controller', 'node'}:
        raise ValueError('wrong peer role')
    if local['boot_id'] == peer['boot_id'] or local['hostname'] == peer['hostname']:
        raise ValueError('same runner')
    if local['source_commit'] != peer['source_commit']:
        raise ValueError('mixed source commits')


if __name__ == '__main__':
    validate(*(json.loads(pathlib.Path(path).read_text()) for path in sys.argv[1:]))
