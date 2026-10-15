#!/usr/bin/env python3
"""Verify the three audited ocserv-backport 1.5.0-2 assets before installation."""
import hashlib
import json
import pathlib
import subprocess
import sys

RELEASE_ID = 404716043
SUMS_SHA256 = 'c46217ce66f441d013be171e332cbd83e1088f1f8d08d1cab242d12dc5693cb1'
PACKAGES = {
    ('ubuntu24.04', 'arm64'): '33f64b7464a8f47420952a883b287c4fce7d4df430412487b85d879ae7bfb173',
    ('debian13', 'amd64'): '1d48e8880fd26ce300faea8ca5d9b043f21c9401ffdf23aab262bebc5e3ff74a',
    ('debian13', 'arm64'): '6975395e67a656b51335bbda61a9e8a99307ae2df5f1577c0c7be8c1fff43e61',
}


def verify(directory, distro, arch):
    expected = PACKAGES[distro, arch]
    name = f'ocserv_1.5.0-1.{distro}.2_{arch}.deb'
    version = f'1.5.0-1~{distro}.2'
    release = json.loads((directory / 'release.json').read_text())
    assert release['id'] == RELEASE_ID and release['tag_name'] == '1.5.0-2'
    assert release['draft'] is False and release['prerelease'] is False
    assert f'`{version}`' in release['body'], 'version differs from release metadata'
    sums = (directory / 'SHA256SUMS').read_bytes()
    assert hashlib.sha256(sums).hexdigest() == SUMS_SHA256, 'SHA256SUMS digest mismatch'
    assert sums.decode().splitlines().count(f'{expected}  {name}') == 1
    assets = [a for a in release['assets'] if a['name'] == name]
    assert len(assets) == 1 and assets[0]['digest'] == 'sha256:' + expected
    assert hashlib.sha256((directory / name).read_bytes()).hexdigest() == expected, 'package SHA256 mismatch'
    fields = subprocess.check_output(['dpkg-deb', '-f', str(directory / name),
                                      'Package', 'Version', 'Architecture'], text=True).strip()
    assert fields == f'Package: ocserv\nVersion: {version}\nArchitecture: {arch}', fields
    return {'package': name, 'version': version, 'architecture': arch, 'sha256': expected,
            'release_id': RELEASE_ID, 'release_tag': '1.5.0-2'}


if __name__ == '__main__':
    print(json.dumps(verify(pathlib.Path(sys.argv[1]), sys.argv[2], sys.argv[3]), sort_keys=True))
