#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

python3 - "${ROOT}" <<'PY'
from pathlib import Path
import os
import re
import subprocess
import sys
from tempfile import TemporaryDirectory

root = Path(sys.argv[1])
products = (root / '.github/workflows/release-products.yml').read_text()
release = (root / '.github/workflows/release.yml').read_text()
release_check = (root / '.github/workflows/release-check.yml').read_text()
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
rc_smoke_job = job(products, 'smoke-rc-lifecycle')
prepare_job = job(release, 'prepare')
assets_job = job(release, 'assets')
publish_job = job(release, 'publish')
rc_business_job = job(release_check, 'business')
rc_quick_job = job(release_check, 'quick')
rc_result_job = job(release_check, 'result')

require('scripts/release-native-package-smoke.sh' not in build_job,
        'the package builder must not run privileged lifecycle smoke')
require('id: upload' in build_job and
        'artifact_id: ${{ steps.upload.outputs.artifact-id }}' in build_job,
        'the package builder must expose the immutable upload by exact artifact ID')
require('needs: build-agent-packages' in smoke_job,
        'the package smoke job must wait for the immutable package upload')
require('uses: ./.github/actions/download-release-product' in smoke_job and
        'artifact-id: ${{ needs.build-agent-packages.outputs.artifact_id }}' in smoke_job and
        'manifest-sha256: ${{ needs.build-agent-packages.outputs.manifest_sha256 }}' in smoke_job,
        'the package smoke job must download and verify the exact immutable build artifact')
require(smoke_job.index('artifact-id:') < smoke_job.index('scripts/release-native-package-smoke.sh'),
        'the package smoke job must download its disposable copy before lifecycle smoke')
require('version: ${{ steps.package-version.outputs.version }}' in build_job and
        'PRODUCTS_DIR: ${{ runner.temp }}/packages' in smoke_job and
        'VERSION: ${{ needs.build-agent-packages.outputs.version }}' in smoke_job,
        'candidate smoke must preserve the resolved version and downloaded package inputs')
require(smoke_job.count('scripts/release-native-package-smoke.sh') == 1 and
        rc_smoke_job.count('scripts/release-native-package-smoke.sh') == 1 and
        "RC_LIFECYCLE: 'true'" in rc_smoke_job and
        'if: inputs.run_rc_lifecycle' in rc_smoke_job and
        'needs: build-agent-packages' in rc_smoke_job and 'needs: smoke-agent-packages' not in rc_smoke_job and
        'PRODUCTS_DIR:' not in rc_smoke_job and 'Restore Rocky Agent compilation cache' in rc_smoke_job,
        'synthetic RC smoke must reuse the package build cache and run in parallel with candidate smoke')
require('python3 scripts/publish-release.py qualify' in prepare_job and
        'python3 scripts/publish-release.py qualify' in publish_job and
        publish_job.index('python3 scripts/publish-release.py qualify') < publish_job.index('gh release create'),
        'release workflow must enforce Release Check qualification both fail-fast in prepare and before publish')
require('needs: [prepare, build-amd64, build-arm64]' in assets_job and
        'uses: ./.github/actions/download-release-product' in assets_job and
        'needs.build-amd64.outputs.agent_packages_artifact_id' in assets_job and
        'needs.build-arm64.outputs.agent_packages_artifact_id' in assets_job and
        'needs.build-amd64.outputs.agent_packages_manifest_sha256' in assets_job and
        'needs.build-arm64.outputs.agent_packages_manifest_sha256' in assets_job,
        'release assets must download and verify exact agent artifacts only after both reusable workflows pass')

move_packages = next(line.strip() for line in assets_job.splitlines()
                     if line.strip().startswith('find "$RUNNER_TEMP/agent-$arch"'))
with TemporaryDirectory() as work:
    temp = Path(work)
    (temp / 'assets').mkdir()
    for arch in ('amd64', 'arm64'):
        (temp / f'agent-{arch}').mkdir()
        (temp / f'agent-{arch}' / f'package-{arch}').write_text(arch)
        (temp / f'agent-{arch}' / 'product-identity.json').write_text('{}')
    subprocess.run(['bash', '-ec', f'for arch in amd64 arm64; do\n{move_packages}\ndone'], check=True,
                   env={**os.environ, 'RUNNER_TEMP': work, 'ASSET_DIR': str(temp / 'assets')})
    require({file.name for file in (temp / 'assets').iterdir()} == {'package-amd64', 'package-arm64'} and
            all((temp / f'agent-{arch}' / 'product-identity.json').is_file() for arch in ('amd64', 'arm64')),
            'asset assembly must move both package sets and leave the identity manifests behind')

require(all("if: github.event_name == 'workflow_dispatch'" in job(release, f'build-{arch}')
            for arch in ('amd64', 'arm64')) and
        'python3 scripts/release_evidence.py record-build' in assets_job and
        'needs: prepare' in publish_job and "if: github.event_name == 'push'" in publish_job and
        'uses: ./.github/actions/download-release-product' in publish_job and
        'needs.prepare.outputs.controller_amd64_sha256' in publish_job and
        'needs.prepare.outputs.controller_arm64_sha256' in publish_job and
        'needs.prepare.outputs.assets_id' in publish_job,
        'tag publication must use the qualified build artifacts without any build dependency')

require(not (root / '.github/workflows/release-business-diagnostic.yml').exists(),
        'redundant release-business-diagnostic.yml workflow must stay retired')
require(all('needs: [products, security]' in entry and 'needs.products.outputs.build_run_id' in entry
            for entry in (rc_business_job, rc_quick_job)),
        'Release Check acceptance jobs must wait for Security and use the same dual-architecture build')
require('needs: [prepare, ci, contracts, database, security, products, business, quick]' in rc_result_job and
        'python3 scripts/release_evidence.py record-check' in rc_result_job and
        'uses: ./.github/workflows/' not in release_check,
        'Release Check must gate the qualification record on every required job and reuse upstream evidence')

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

PYTHONDONTWRITEBYTECODE=1 python3 "$ROOT/scripts/test-release-evidence.py"

work="$(mktemp -d)"
trap 'rm -rf -- "$work"' EXIT

# Pre-workspace phases (e.g. signer_prepare, integrated_entry) must not require workspace ID.
env -u OCSERVIA_BUSINESS_TEST_WORKSPACE_ID \
  OCSERVIA_BUSINESS_TEST_WORK_DIR="$work" ARTIFACT_DIR="$work" \
  PYTHONPATH="$ROOT/scripts" python3 -c '
import json
from unittest.mock import patch

import release_business_runtime as rt
env = rt.environment()
assert env.work.is_dir() and env.evidence.is_dir()

with patch.object(rt.time, "monotonic_ns", side_effect=[0, 2_000_000, 10_000_000, 13_000_000]):
    with rt.timed("recovery"):
        assert rt.environment() is env
    try:
        with rt.timed("revocation"):
            raise ValueError("scenario failed")
    except ValueError as error:
        assert str(error) == "scenario failed"
    else:
        raise AssertionError("the timer suppressed the scenario failure")

assert [json.loads(line) for line in (env.evidence / "scenario-timings.jsonl").read_text().splitlines()] == [
    {"name": "recovery", "elapsed_ms": 2, "status": "PASS"},
    {"name": "revocation", "elapsed_ms": 3, "status": "FAIL"},
]
'

# smoke_relay_recovery must enforce offline zero-side-effects, exactly-once recovery, and idempotent replay.
OCSERVIA_BUSINESS_TEST_WORK_DIR="$work" ARTIFACT_DIR="$work" \
  OCSERVIA_BUSINESS_TEST_NODE_ID="00000000-0000-7000-8000-000000000001" \
  OCSERVIA_BUSINESS_TEST_RELAY_CONTAINER="relay-test" \
  PYTHONPATH="$ROOT/scripts" python3 -c '
import json
from pathlib import Path
import release_business_scenarios as sc

state = {"relay_up": True, "epoch": 2, "conn": "aa" * 16, "reloads": 1, "journal": 3, "privd": 3, "published": False, "op_state": "queued"}
recorded = []

sc.single_relay_argv = lambda: None
sc.identity_digest = lambda: "id-digest"
sc.native_service_pids = lambda: {"ocserv": 10, "ocservia-agent": 20, "ocservia-privd": 30}
sc.node_side_effects = lambda: {"journal": state["journal"], "privd": state["privd"], "reloads": state["reloads"], "files": "same"}

def fake_run(*args, **kwargs):
    if args == ("docker", "stop", "relay-test"):
        state["relay_up"] = False
    elif args == ("docker", "start", "relay-test"):
        state["relay_up"] = True
        state["epoch"] += 1
        state["conn"] = "bb" * 16
        state["published"] = True
        state["op_state"] = "succeeded"
        state["journal"] += 1
        # Simulate privd pruning an older expired applied row when recording this effect.
        state["privd"] -= 1
        state["reloads"] += 1
    return ""

def fake_api(path, body=None, *, status=200, headers=None, **kwargs):
    if path.startswith("nodes/") and body is None:
        return {"connection_state": "online" if state["relay_up"] else "offline", "version": 4}
    if path.endswith("/service:reload"):
        return {"id": "op-1", "command_id": "cmd-1", "state": state["op_state"]}
    if path == "operations/op-1":
        return {"id": "op-1", "command_id": "cmd-1", "state": state["op_state"]}
    raise AssertionError(path)

def fake_sql(query):
    if "connection_owner_fencing" in query:
        assert "active" in query and "lease_until" not in query and "owner_epoch" not in query, query
        if "row_to_json" in query:
            assert "session_epoch" in query, query
            return json.dumps({"connection_id": state["conn"], "session_epoch": state["epoch"]}) if state["relay_up"] else ""
        assert "NOT EXISTS" in query, query
        return "f" if state["relay_up"] else "t"
    if "published_at IS NULL" in query:
        return "0" if state["published"] else "1"
    if "published_at IS NOT NULL" in query:
        return "1" if state["published"] else "0"
    if "command_attempts" in query:
        return "1" if state["published"] else "0"
    if "audit_events" in query:
        return "1"
    raise AssertionError(query)

sc.run = fake_run
sc.api = fake_api
sc.sql = fake_sql
sc.cross_check = lambda op: {"database_state": "succeeded", "journal_count_state_error_receipt": "1|succeeded||1", "root_count_state_response": "1|applied|1"}
sc.record = lambda name, **kw: recorded.append((name, kw))

sc.smoke_relay_recovery(lambda: True, outage_settle_seconds=0)
assert [name for name, _ in recorded] == ["single_relay_ownership_invalidated", "resilience_relay"]
assert recorded[-1][1]["offline_queued_zero_side_effects"] is True
assert recorded[-1][1]["idempotent_replay"] is True
assert recorded[-1][1]["after_epoch"] > recorded[-1][1]["before_epoch"]
'

echo 'Release package supply-chain guards passed'
