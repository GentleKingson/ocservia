#!/usr/bin/env python3
"""Bind downloaded release products to the producer's immutable manifest digest."""
import argparse
import hashlib
import json
import re
from pathlib import Path


def product_files(kind, version, arch):
    if kind == 'controller':
        return [f'{role}-linux-{arch}.tar' for role in
                ('gateway', 'control', 'transport', 'backup', 'edge', 'relay', 'signer')]
    rpm_arch = {'amd64': 'x86_64', 'arm64': 'aarch64'}[arch]
    return [f'ocservia-agent_{version}-1_{arch}.deb',
            f'ocservia-agent-{version}-1.{rpm_arch}.rpm']


def digest(path):
    if path.is_symlink() or not path.is_file() or path.stat().st_size == 0:
        raise ValueError(f'product must be a nonempty regular file: {path.name}')
    with path.open('rb') as stream:
        return hashlib.file_digest(stream, 'sha256').hexdigest()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('mode', choices=['create', 'verify'])
    parser.add_argument('--kind', required=True, choices=['agent', 'controller'])
    parser.add_argument('--directory', required=True, type=Path)
    parser.add_argument('--source', required=True)
    parser.add_argument('--version', required=True)
    parser.add_argument('--arch', required=True, choices=['amd64', 'arm64'])
    parser.add_argument('--manifest-sha256')
    args = parser.parse_args()
    if not re.fullmatch(r'[0-9a-f]{40}', args.source):
        parser.error('source must be a full commit SHA')
    if not re.fullmatch(r'[0-9]+\.[0-9]+\.[0-9]+(-rc\.[1-9][0-9]*)?', args.version):
        parser.error('version must be a release SemVer')
    manifest = args.directory / 'product-identity.json'
    if args.mode == 'verify':
        if not re.fullmatch(r'[0-9a-f]{64}', args.manifest_sha256 or ''):
            parser.error('the producer manifest SHA-256 is required')
        if manifest.is_symlink() or digest(manifest) != args.manifest_sha256:
            raise ValueError('product manifest digest mismatch')
    files = product_files(args.kind, args.version, args.arch)
    if {entry.name for entry in args.directory.iterdir()} != set(files) | (
            {'product-identity.json'} if manifest.exists() else set()):
        raise ValueError('product inventory mismatch')
    identity = {
        'schema_version': 1,
        'source_commit': args.source, 'version': args.version, 'arch': args.arch, 'kind': args.kind,
        'files': {name: digest(args.directory / name) for name in files},
    }
    if args.mode == 'create':
        manifest.write_text(json.dumps(identity, sort_keys=True, indent=2) + '\n')
    elif json.loads(manifest.read_text()) != identity:
        raise ValueError('product identity or file digest mismatch')
    print(digest(manifest))


if __name__ == '__main__':
    main()
