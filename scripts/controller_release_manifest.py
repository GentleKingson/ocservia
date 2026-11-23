#!/usr/bin/env python3
"""Generate a digest-pinned Controller deployment manifest."""
import argparse
import json
from pathlib import Path
import re
import sys


def build_manifest(version, tag, commit, platform, migration_dir, schema, images):
    roles = {'gateway', 'control', 'transport', 'backup', 'postgres', 'otel'}
    if schema == 2:
        roles.update(('edge', 'relay', 'signer'))
    if set(images) != roles or any(not re.fullmatch(r'[^\s@]+@sha256:[0-9a-f]{64}', ref)
                                   for ref in images.values()):
        raise ValueError('expected exactly one digest-pinned reference for each production image')
    if not re.fullmatch(r'[0-9]+\.[0-9]+\.[0-9]+(?:-rc\.[1-9][0-9]*)?', version) or tag != 'v' + version:
        raise ValueError('invalid release version or tag')
    if not re.fullmatch(r'[0-9a-f]{40}', commit) or platform not in ('linux/amd64', 'linux/arm64'):
        raise ValueError('invalid source commit or platform')
    revisions = []
    for entry in (migration_dir / 'sql').glob('*.sql'):
        match = re.fullmatch(r'([0-9]+)_.+\.sql', entry.name)
        if not match or int(match[1]) < 1:
            raise ValueError(f'invalid goose migration filename: {entry.name}')
        revisions.append(int(match[1]))
    if not revisions:
        raise ValueError(f'no goose SQL migrations in {migration_dir}')
    return {'manifest_version': schema, 'release_version': version, 'release_tag': tag,
            'source_commit': commit, 'platform': platform,
            'database_migration': {'epoch': 1, 'revision': max(revisions)},
            **({'signer_state_version': 1} if schema == 2 else {}), 'images': images}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ('output', 'release-version', 'release-tag', 'source-commit', 'platform'):
        parser.add_argument('--' + name, required=True)
    parser.add_argument('--migration-dir', type=Path, default=Path('control-plane/migrations'))
    parser.add_argument('--manifest-version', type=int, choices=(1, 2), default=1)
    parser.add_argument('--image', action='append', required=True)
    args = parser.parse_args()
    try:
        images = dict(image.split('=', 1) for image in args.image)
        if len(images) != len(args.image):
            raise ValueError('duplicate image')
        manifest = build_manifest(args.release_version, args.release_tag, args.source_commit,
                                  args.platform, args.migration_dir, args.manifest_version, images)
    except ValueError as error:
        parser.error(str(error))
    content = json.dumps(manifest, indent=2) + '\n'
    if args.output == '-':
        sys.stdout.write(content)
    else:
        Path(args.output).write_text(content)


if __name__ == '__main__':
    main()
