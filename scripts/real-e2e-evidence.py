#!/usr/bin/env python3
"""Reject same-host, wrong-role, or mixed-commit hosted rendezvous evidence."""
import json
import pathlib
import re
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
        if not re.fullmatch(r'[a-f0-9]{40}', item['source_commit']):
            raise ValueError('invalid source commit')
        if item['runner_arch'] != 'x86_64' or not re.fullmatch(r'[0-9]+-[0-9]+', item['run_id']):
            raise ValueError('unexpected architecture or run attempt')
        if set(item['relay']) != {'url', 'ca_sha256'}:
            raise ValueError('missing Relay evidence')
    if {local['role'], peer['role']} != {'controller', 'node'}:
        raise ValueError('wrong peer role')
    # Hosted VM images can reuse hostnames and namespace inode numbers.
    # Kernel boot IDs, not image labels, identify the independent instances.
    if local['boot_id'] == peer['boot_id']:
        raise ValueError('same runner')
    if 'relay_vm' in local or 'relay_vm' in peer:
        if local.get('relay_vm') != peer.get('relay_vm'):
            raise ValueError('different Relay VMs')
        relay = local['relay_vm']
        if relay['boot_id'] in (local['boot_id'], peer['boot_id']):
            raise ValueError('Relay shares a peer VM')
        if relay['source_commit'] != local['source_commit']:
            raise ValueError('Relay source mismatch')
    if local['source_commit'] != peer['source_commit']:
        raise ValueError('mixed source commits')
    if local['run_id'] != peer['run_id'] or local['relay'] != peer['relay']:
        raise ValueError('mixed run attempts or Relay configuration')


if __name__ == '__main__':
    validate(*(json.loads(pathlib.Path(path).read_text()) for path in sys.argv[1:]))
