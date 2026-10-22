#!/usr/bin/env python3
"""One-shot release publication. Existing drafts/tags are never resumed or replaced."""
import base64
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import sys
from urllib.error import HTTPError
from urllib.parse import urlencode
from urllib.request import Request, urlopen

ROLES = ('gateway', 'control', 'transport', 'backup', 'edge', 'relay', 'signer', 'mysql_backup')
ARCHES = ('amd64', 'arm64')
PREFIX = 'ghcr.io/gentlekingson/ocservia'
ACCEPT = ', '.join(('application/vnd.oci.image.index.v1+json',
                    'application/vnd.docker.distribution.manifest.list.v2+json',
                    'application/vnd.oci.image.manifest.v1+json',
                    'application/vnd.docker.distribution.manifest.v2+json'))


def require(condition, message):
    if not condition:
        raise RuntimeError(message)


def command(*args, **kwargs):
    return subprocess.check_output(args, text=True, **kwargs).strip()


def api(path, *args):
    return json.loads(command('gh', 'api', path, *args))


def asset_files(version, directory):
    names = {'install-controller', 'controller-bootstrap.sh', 'managed-node-bootstrap.sh',
             'controller-release.json', 'controller-release-amd64.json', 'controller-release-arm64.json'}
    for arch, rpm_arch in (('amd64', 'x86_64'), ('arm64', 'aarch64')):
        archive = f'ocservia-agent-{version}-linux-{arch}.tar.gz'
        names.update((archive, archive + '.sha256', f'ocservia-agent_{version}-1_{arch}.deb',
                      f'ocservia-agent-{version}-1.{rpm_arch}.rpm'))
    require({p.name for p in directory.iterdir()} == names, 'Incomplete or unexpected release asset set')
    files = [directory / name for name in sorted(names)]
    for path in files:
        require(path.is_file() and not path.is_symlink() and path.stat().st_size > 0,
                f'Missing, empty or unsafe asset: {path.name}')
    return files


def digest(path):
    with path.open('rb') as stream:
        return 'sha256:' + hashlib.file_digest(stream, 'sha256').hexdigest()


def manifest(name, tag):
    repository = f'gentlekingson/ocservia/{name}'
    credentials = base64.b64encode(f"{os.environ['GITHUB_ACTOR']}:{os.environ['GH_TOKEN']}".encode()).decode()
    query = urlencode({'service': 'ghcr.io', 'scope': f'repository:{repository}:pull'})
    # Token failures (including 404) are errors, never evidence of a missing tag.
    with urlopen(Request('https://ghcr.io/token?' + query,
                         headers={'Authorization': 'Basic ' + credentials}), timeout=60) as response:
        token = json.load(response)['token']
    request = Request(f'https://ghcr.io/v2/{repository}/manifests/{tag}',
                      headers={'Authorization': 'Bearer ' + token, 'Accept': ACCEPT})
    try:
        with urlopen(request, timeout=60) as response:
            raw = response.read()
            actual = 'sha256:' + hashlib.sha256(raw).hexdigest()
            require(response.headers.get('Docker-Content-Digest') == actual, 'Registry digest mismatch')
            return actual, json.loads(raw)
    except HTTPError as error:
        if error.code == 404:
            errors = json.load(error).get('errors', [])
            if errors and all(item.get('code') == 'MANIFEST_UNKNOWN' for item in errors):
                return None
        raise


def absent(name, tag):
    require(manifest(name, tag) is None, f'Existing image tag must not be overwritten: {PREFIX}/{name}:{tag}')


def preflight(repo, tag, images):
    # Listing with write authority includes drafts and avoids conflating GET errors with absence.
    pages = api(f'repos/{repo}/releases?per_page=100', '--paginate', '--slurp')
    require(not any(release['tag_name'] == tag for page in pages for release in page),
            f'Release {tag} already exists (draft or published); use a new version')
    for name in ROLES:
        for arch in ARCHES:
            path = images / f'{name}-linux-{arch}.tar'
            require(path.is_file() and not path.is_symlink() and path.stat().st_size > 0,
                    f'Missing image archive: {path.name}')
        for suffix in ('', '-amd64', '-arm64'):
            absent(name, tag + suffix)


def verify_assets(repo, release_id, files):
    pages = api(f'repos/{repo}/releases/{release_id}/assets?per_page=100', '--paginate', '--slurp')
    assets = [asset for page in pages for asset in page]
    require(sorted(asset['name'] for asset in assets) == sorted(path.name for path in files),
            'Remote release asset set is incomplete or contains duplicates/unexpected files')
    by_name = {asset['name']: asset for asset in assets}
    for path in files:
        asset = by_name[path.name]
        require(asset['state'] == 'uploaded' and asset['size'] == path.stat().st_size
                and asset.get('digest') == digest(path), f'Remote asset verification failed: {path.name}')


def draft(repo, tag, sha, prerelease):
    release = api(f'repos/{repo}/releases/tags/{tag}')
    require(release['tag_name'] == tag and release['draft'] is True
            and release['prerelease'] == prerelease and release['target_commitish'] == sha,
            'Expected a new draft for this exact build SHA')
    return release['id']


def verify_images(expected):
    for (name, tag), expected_digest in expected.items():
        current = manifest(name, tag)
        require(current is not None and current[0] == expected_digest, f'Published image changed: {name}:{tag}')


def finish(repo, version, sha, files, images):
    tag = 'v' + version
    prerelease = '-rc.' in version
    release_id = draft(repo, tag, sha, prerelease)
    command('gh', 'release', 'upload', tag, *(str(path) for path in files))
    verify_assets(repo, release_id, files)
    command('docker', 'login', 'ghcr.io', '-u', os.environ['GITHUB_ACTOR'], '--password-stdin',
            input=os.environ['GH_TOKEN'])
    expected = {}
    for name in ROLES:
        children = {}
        for arch in ARCHES:
            source = f'{PREFIX}/{name}:{version}-linux-{arch}'
            target = f'{PREFIX}/{name}:{tag}-{arch}'
            command('docker', 'load', '--input', str(images / f'{name}-linux-{arch}.tar'))
            local = json.loads(command('docker', 'image', 'inspect', source))[0]
            require(local['Os'] == 'linux' and local['Architecture'] == arch, f'Wrong image platform: {source}')
            command('docker', 'tag', source, target)
            absent(name, f'{tag}-{arch}')
            command('docker', 'push', target)
            remote = manifest(name, f'{tag}-{arch}')
            require(remote is not None and remote[1].get('config', {}).get('digest') == local['Id'],
                    f'Pushed image does not match scanned archive: {target}')
            children[arch] = remote[0]
            expected[name, f'{tag}-{arch}'] = remote[0]
        absent(name, tag)
        command('docker', 'buildx', 'imagetools', 'create', '--tag', f'{PREFIX}/{name}:{tag}',
                *(f'{PREFIX}/{name}@{children[arch]}' for arch in ARCHES))
        index = manifest(name, tag)
        descriptors = index[1].get('manifests', []) if index else []
        require(len(descriptors) == 2 and
                {(d['platform']['os'], d['platform']['architecture'], d['digest']) for d in descriptors}
                == {('linux', arch, children[arch]) for arch in ARCHES}, f'Invalid multi-arch index: {name}')
        expected[name, tag] = index[0]
    # Recheck all boundaries immediately before making the Release public.
    verify_images(expected)
    require(draft(repo, tag, sha, prerelease) == release_id, 'Draft identity changed')
    verify_assets(repo, release_id, files)
    require(api(f'repos/{repo}/commits/tags/{tag}')['sha'] == sha, 'Release tag moved during publication')
    command('gh', 'api', f'repos/{repo}/releases/{release_id}', '--method', 'PATCH',
            '-F', 'draft=false', '-f', f'make_latest={"false" if prerelease else "true"}')
    published = api(f'repos/{repo}/releases/{release_id}')
    require(published['draft'] is False and published.get('immutable') is True
            and published['tag_name'] == tag and published['prerelease'] == prerelease,
            'Release is not published and immutable; enable repository Immutable Releases before publishing')
    verify_assets(repo, release_id, files)
    verify_images(expected)
    if prerelease:
        require(api(f'repos/{repo}/releases/latest')['tag_name'] != tag, 'RC must not become Latest')
    with open(os.environ['GITHUB_STEP_SUMMARY'], 'a') as summary:
        summary.write(f'Published immutable Release {tag} at {sha}: {len(files)} assets, 24 verified image tags.\n')


def main():
    phase = sys.argv[1]
    version = os.environ['VERSION']
    require(re.fullmatch(r'[0-9]+\.[0-9]+\.[0-9]+(?:-rc\.[1-9][0-9]*)?', version), 'Invalid version')
    root = Path(os.environ['RUNNER_TEMP'])
    files = asset_files(version, root / 'assets')
    if phase == 'check-assets':
        return
    repo = os.environ['GH_REPO']
    if phase == 'preflight':
        preflight(repo, 'v' + version, root / 'images')
    elif phase == 'finish':
        finish(repo, version, os.environ['GITHUB_SHA'], files, root / 'images')
    else:
        raise RuntimeError('Unknown publication phase')


if __name__ == '__main__':
    try:
        main()
    except (RuntimeError, OSError, ValueError, KeyError, subprocess.CalledProcessError) as error:
        print(f'Release publication failed: {error}', file=sys.stderr)
        sys.exit(1)
