#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

python3 - "${ROOT}" <<'PY'
from pathlib import Path
import re
import sys

root = Path(sys.argv[1])
products = (root / '.github/workflows/release-products.yml').read_text()
release = (root / '.github/workflows/release.yml').read_text()
smoke_script = (root / 'scripts/release-native-package-smoke.sh').read_text()
builder = (root / 'rust/agent-build.Dockerfile').read_text()


def require(condition, message):
    if not condition:
        raise SystemExit(message)


def job(document, name):
    marker = f'  {name}:\n'
    start = document.find(marker)
    require(start >= 0, f'missing workflow job: {name}')
    match = re.search(r'^  [a-zA-Z0-9_-]+:\n', document[start + len(marker):], re.MULTILINE)
    end = len(document) if match is None else start + len(marker) + match.start()
    return document[start:end]


build_job = job(products, 'build-agent-packages')
smoke_job = job(products, 'smoke-agent-packages')
assets_job = job(release, 'assets')
publish_job = job(release, 'publish')

require('scripts/release-native-package-smoke.sh' not in build_job,
        'the package builder must not run privileged lifecycle smoke')
require('id: upload' in build_job and
        'artifact_id: ${{ steps.upload.outputs.artifact-id }}' in build_job,
        'the package builder must expose the immutable upload by exact artifact ID')
require('needs: build-agent-packages' in smoke_job,
        'the package smoke job must wait for the immutable package upload')
require('actions/download-artifact@9000827ccba6bdab643e8b6fd33ac0654aef8333' in smoke_job and
        'artifact-ids: ${{ needs.build-agent-packages.outputs.artifact_id }}' in smoke_job,
        'the package smoke job must download the exact immutable build artifact')
require(smoke_job.index('artifact-ids:') < smoke_job.index('scripts/release-native-package-smoke.sh'),
        'the package smoke job must download its disposable copy before lifecycle smoke')
require('version: ${{ steps.package-version.outputs.version }}' in build_job and
        'PRODUCTS_DIR: ${{ runner.temp }}/packages' in smoke_job and
        'VERSION: ${{ needs.build-agent-packages.outputs.version }}' in smoke_job,
        'candidate smoke must preserve the resolved version and downloaded package inputs')
require(smoke_job.count('scripts/release-native-package-smoke.sh') == 2 and
        "RC_LIFECYCLE: 'true'" in smoke_job,
        'the isolated job must preserve candidate and RC lifecycle coverage')
require('needs: [prepare, build-amd64, build-arm64]' in assets_job and
        'needs.build-amd64.outputs.agent_packages_artifact_id' in assets_job and
        'needs.build-arm64.outputs.agent_packages_artifact_id' in assets_job,
        'release assets must consume exact artifacts only after both reusable workflows pass')
require('needs: [prepare, build-amd64, build-arm64, assets]' in publish_job,
        'publication must continue to require successful package smoke and asset assembly')

base = builder.splitlines()[0]
require(re.fullmatch(r'FROM rockylinux:9@sha256:[0-9a-f]{64}', base) is not None,
        'the shared Rocky Linux 9 Agent builder base must remain digest pinned')
require('FROM rockylinux:9\n' not in smoke_script,
        'the RPM smoke helper must not resolve the mutable Rocky Linux tag')
require('"${ROOT}/rust/agent-build.Dockerfile"' in smoke_script and
        '--build-arg "ROCKY_IMAGE=${rocky_image}"' in smoke_script and
        'FROM ${ROCKY_IMAGE}' in smoke_script and
        'rockylinux:9@sha256:[0-9a-f]{64}' in smoke_script,
        'the RPM smoke helper must validate and reuse the shared immutable Rocky base')
PY

echo 'Release package supply-chain guards passed'
