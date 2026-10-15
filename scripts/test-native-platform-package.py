#!/usr/bin/env python3
"""Offline rejection checks for the pre-install package verification boundary."""
import hashlib
import importlib.util
import json
import pathlib
import tempfile
from unittest.mock import patch

spec = importlib.util.spec_from_file_location('platform_package', pathlib.Path(__file__).with_name('verify-native-platform-package.py'))
module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(module)

with tempfile.TemporaryDirectory() as temp:
    root = pathlib.Path(temp)
    name = 'ocserv_1.5.0-1.debian13.2_arm64.deb'
    package = b'offline package bytes; dpkg fields are tested independently'
    digest = hashlib.sha256(package).hexdigest()
    sums = f'{digest}  {name}\n'.encode()
    release = {'id': module.RELEASE_ID, 'tag_name': '1.5.0-2', 'draft': False, 'prerelease': False,
               'body': '`1.5.0-1~debian13.2`', 'assets': [{'name': name, 'digest': 'sha256:' + digest}]}
    fields = 'Package: ocserv\nVersion: 1.5.0-1~debian13.2\nArchitecture: arm64\n'
    # Existing state must remain untouched on every validation failure.
    identity = root / 'endpoint.key'
    identity.write_bytes(b'existing identity sentinel')
    for fault in (None, 'sha256', 'sums', 'architecture', 'version', 'package', 'metadata', 'download'):
        (root / name).write_bytes(package + (b'corrupt' if fault == 'sha256' else b''))
        (root / 'SHA256SUMS').write_bytes(sums + (b'corrupt' if fault == 'sums' else b''))
        (root / 'release.json').write_text(json.dumps({**release, 'tag_name': 'latest'} if fault == 'metadata' else release))
        if fault == 'download':
            (root / name).unlink()
        output = fields
        if fault == 'architecture':
            output = fields.replace('arm64', 'amd64')
        if fault == 'version':
            output = fields.replace('1.5.0', '1.5.1')
        if fault == 'package':
            output = fields.replace('Package: ocserv', 'Package: other')
        passed = False
        with patch.dict(module.PACKAGES, {('debian13', 'arm64'): digest}), \
                patch.object(module, 'SUMS_SHA256', hashlib.sha256(sums).hexdigest()), \
                patch.object(module.subprocess, 'check_output', return_value=output):
            try:
                module.verify(root, 'debian13', 'arm64')
                passed = True
            except (AssertionError, FileNotFoundError):
                pass
        assert passed == (fault is None), fault
        assert identity.read_bytes() == b'existing identity sentinel'
print('Pinned package: missing download, digest, metadata, package, architecture and version rejection PASS')
