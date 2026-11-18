#!/usr/bin/env python3
"""Offline product handoff and no-rebuild regression checks."""
import hashlib
import json
import os
import runpy
import subprocess
import tempfile
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
HELPER = ROOT / 'scripts/release-product-identity.py'
product_files = runpy.run_path(str(HELPER))['product_files']


def main():
    with tempfile.TemporaryDirectory() as directory:
        work = Path(directory)
        for kind in ('agent', 'controller'):
            for arch in ('amd64', 'arm64'):
                products = work / f'{kind}-{arch}'
                products.mkdir()
                for name in product_files(kind, '0.0.0', arch):
                    (products / name).write_bytes(name.encode())
                args = ['--directory', str(products), '--kind', kind, '--source', 'a' * 40,
                        '--version', '0.0.0', '--arch', arch]
                created = subprocess.run(['python3', str(HELPER), 'create', *args],
                                         check=True, capture_output=True, text=True)
                manifest = products / 'product-identity.json'
                original = manifest.read_bytes()
                digest = created.stdout.strip()
                assert digest == hashlib.sha256(original).hexdigest()

                def verify(extra=(), expected=True):
                    result = subprocess.run(['python3', str(HELPER), 'verify', *args,
                                             '--manifest-sha256', digest, *extra],
                                            capture_output=True, text=True)
                    assert (result.returncode == 0) == expected, result.stderr

                verify()
                for extra in (['--source', 'b' * 40], ['--version', '0.0.1'],
                              ['--arch', 'arm64' if arch == 'amd64' else 'amd64'],
                              ['--kind', 'controller' if kind == 'agent' else 'agent'],
                              ['--manifest-sha256', '0' * 64], ['--manifest-sha256', '']):
                    verify(extra, expected=False)
                # Even a self-consistent replacement manifest needs the producer digest.
                data = json.loads(original)
                data['source_commit'] = 'b' * 40
                manifest.write_text(json.dumps(data))
                verify(['--source', 'b' * 40], expected=False)
                manifest.write_bytes(original)
                for name in product_files(kind, '0.0.0', arch):
                    file = products / name
                    payload = file.read_bytes()
                    file.write_bytes(payload + b'tampered')
                    verify(expected=False)
                    file.unlink()
                    verify(expected=False)
                    target = work / 'symlink-target'
                    target.write_bytes(payload)
                    file.symlink_to(target)
                    verify(expected=False)
                    file.unlink()
                    file.write_bytes(payload)
                unexpected = products / 'unexpected.tar'
                unexpected.write_bytes(b'extra')
                verify(expected=False)
                unexpected.unlink()
                verify()

        # Execute the real Business build/consume branch with recording doubles.
        shell = '''set -euo pipefail
source "$ROOT/scripts/release-business-stages.sh"
next_stage() { echo "stage:$1"; }
bash() { echo "build:$*"; }
env() { echo "build:$*"; }
mkdir() { :; }
chown() { :; }
'''
        env = {**os.environ, 'ROOT': str(ROOT), 'ARTIFACT_DIR': str(work),
               'OUTPUT_DIR': str(work), 'IMAGES_DIR': str(work / 'controller-amd64')}
        result = subprocess.run(['bash', '-c', shell + '\nprepare_business_products\n'],
                                env={**env, 'PRODUCTS_DIR': str(work / 'agent-amd64')},
                                check=True, capture_output=True, text=True)
        assert 'stage:product_consumption' in result.stdout
        assert not (work / 'agent-build.log').exists()
        assert not (work / 'controller-build.log').exists()
        assert 'native-packages' not in result.stdout
        assert 'package_build' not in result.stdout and 'image_build' not in result.stdout
        assert (work / 'agent-product-identity.json').read_bytes() == (
            work / 'agent-amd64/product-identity.json').read_bytes()
        result = subprocess.run(['bash', '-c', shell + '\nprepare_business_products\n'], env={**env, 'PRODUCTS_DIR': ''},
                                check=True, capture_output=True, text=True)
        assert result.stdout.count('stage:agent_package_build') == 1
        assert result.stdout.count('stage:controller_image_build') == 1
    print('Product identity, tamper rejection and Business no-rebuild contracts passed')


if __name__ == '__main__':
    main()
