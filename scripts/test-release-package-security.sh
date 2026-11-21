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
require('python3 scripts/publish-release.py qualify' in prepare_job and
        'python3 scripts/publish-release.py qualify' in publish_job,
        'release workflow must enforce Release Check qualification both fail-fast in prepare and before publish')
require('needs: [prepare, build-amd64, build-arm64]' in assets_job and
        'needs.build-amd64.outputs.agent_packages_artifact_id' in assets_job and
        'needs.build-arm64.outputs.agent_packages_artifact_id' in assets_job,
        'release assets must consume exact artifacts only after both reusable workflows pass')
require('needs: [prepare, build-amd64, build-arm64, assets]' in publish_job,
        'publication must continue to require successful package smoke and asset assembly')

require(not (root / '.github/workflows/release-business-diagnostic.yml').exists(),
        'redundant release-business-diagnostic.yml workflow must stay retired')
require('needs: products-amd64' in rc_business_job and 'needs: products-amd64' in rc_quick_job,
        'Release Check amd64 acceptance jobs must depend only on products-amd64')
require('needs: [prepare, full-ci, security, products-amd64, products-arm64, business, quick]' in rc_result_job,
        'Release Check Result must gate on every required job')

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

# Behavioral checks for publish-release.py qualify gate (mocking gh CLI).
work="$(mktemp -d)"
trap 'rm -rf -- "$work"' EXIT
mkdir -p "$work/bin"
cat >"$work/bin/gh" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
case "$*" in
  *"commits/refs/tags/"*) printf '{"sha":"%s"}\n' "$MOCK_TAG_SHA" ;;
  *"workflows/release-check.yml/runs"*) printf '%s\n' "$MOCK_RUNS_JSON" ;;
  *) echo "unexpected gh call: $*" >&2; exit 1 ;;
esac
EOF
chmod 0755 "$work/bin/gh"
head_sha="$(git -C "$ROOT" rev-parse HEAD)"
run_qualify() {
  PATH="$work/bin:$PATH" GH_REPO="GentleKingson/ocservia" VERSION="2.0.0" \
    GITHUB_SHA="$1" MOCK_TAG_SHA="$2" MOCK_RUNS_JSON="$3" \
    GITHUB_STEP_SUMMARY="$work/summary.md" \
    python3 "$ROOT/scripts/publish-release.py" qualify >/dev/null 2>&1
}

# 1. Passes when the latest main workflow_dispatch Release Check succeeded for HEAD.
: >"$work/summary.md"
run_qualify "$head_sha" "$head_sha" "[{\"workflow_runs\":[{\"run_number\":10,\"head_sha\":\"${head_sha}\",\"head_branch\":\"main\",\"event\":\"workflow_dispatch\",\"status\":\"completed\",\"conclusion\":\"failure\",\"html_url\":\"https://example.invalid/10\"},{\"run_number\":11,\"head_sha\":\"${head_sha}\",\"head_branch\":\"main\",\"event\":\"workflow_dispatch\",\"status\":\"completed\",\"conclusion\":\"success\",\"html_url\":\"https://example.invalid/11\"}]}]"
grep -Fq "Release Check: https://example.invalid/11 (SHA: ${head_sha})" "$work/summary.md"

# 2. Fails when a newer Release Check failed even if an older run passed.
if run_qualify "$head_sha" "$head_sha" "[{\"workflow_runs\":[{\"run_number\":11,\"head_sha\":\"${head_sha}\",\"head_branch\":\"main\",\"event\":\"workflow_dispatch\",\"status\":\"completed\",\"conclusion\":\"success\",\"html_url\":\"https://example.invalid/11\"},{\"run_number\":12,\"head_sha\":\"${head_sha}\",\"head_branch\":\"main\",\"event\":\"workflow_dispatch\",\"status\":\"completed\",\"conclusion\":\"failure\",\"html_url\":\"https://example.invalid/12\"}]}]"; then
  echo 'expected newer failed Release Check to block qualification' >&2
  exit 1
fi

# 3. Fails when tag SHA mismatches HEAD or when no runs exist.
if run_qualify "$head_sha" "0000000000000000000000000000000000000000" '[{"workflow_runs":[]}]'; then
  echo 'expected tag mismatch to block qualification' >&2
  exit 1
fi
if run_qualify "$head_sha" "$head_sha" '[{"workflow_runs":[]}]'; then
  echo 'expected missing Release Check runs to block qualification' >&2
  exit 1
fi

# Pre-workspace phases (e.g. signer_prepare, integrated_entry) must not require workspace ID.
env -u OCSERVIA_BUSINESS_TEST_WORKSPACE_ID \
  OCSERVIA_BUSINESS_TEST_WORK_DIR="$work" ARTIFACT_DIR="$work" \
  PYTHONPATH="$ROOT/scripts" python3 -c '
import release_business_runtime as rt
env = rt.environment()
assert env.work.is_dir() and env.evidence.is_dir()
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
sc.owner = lambda: {"connection_id": state["conn"], "owner_epoch": state["epoch"]}
sc.identity_digest = lambda: "id-digest"
sc.native_service_pids = lambda: {"ocserv": 10, "ocservia-agent": 20, "ocservia-privd": 30}
sc.node_side_effects = lambda: {"journal": state["journal"], "privd": state["privd"], "reloads": state["reloads"], "files": "same"}
sc.wait_for_relay_outage = lambda node: None

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
        state["privd"] += 1
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
assert len(recorded) == 1 and recorded[0][0] == "resilience_relay"
assert recorded[0][1]["offline_queued_zero_side_effects"] is True
assert recorded[0][1]["idempotent_replay"] is True
assert recorded[0][1]["after_epoch"] > recorded[0][1]["before_epoch"]
'

echo 'Release package supply-chain guards passed'
