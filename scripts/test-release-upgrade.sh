#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${ROOT}"
fixture="$(mktemp -d)"
trap 'rm -rf -- "${fixture}"' EXIT
# The auth fixture changes only Controller settings. Never race the unchanged
# transport container against its trust backend, or continue after failed health.
# shellcheck disable=SC1090
source <(sed -n '/^configure_auth_peers() {/,/^}/p' scripts/release-business-probe.sh)
(
  # shellcheck disable=SC2317 # Called by the sourced configure_auth_peers.
  compose() { printf '%s\n' "$*" >>"${fixture}/auth-order"; }
  # shellcheck disable=SC2317 # Called by the sourced configure_auth_peers.
  python3() { printf '%s\n' "python3 $*" >>"${fixture}/auth-order"; }
  configure_auth_peers
)
[[ "$(sed -n '1p' "${fixture}/auth-order")" == 'up -d --no-deps --wait control-plane' ]]
[[ "$(sed -n '2p' "${fixture}/auth-order")" == 'start transportd' ]]
[[ "$(sed -n '3p' "${fixture}/auth-order")" == "python3 ${ROOT}/scripts/release-business-api.py transport_ready" ]]
[[ "$(wc -l <"${fixture}/auth-order")" == 3 ]]
set +e
(
  set -e
  # shellcheck disable=SC2317 # Called by the sourced configure_auth_peers.
  compose() { printf '%s\n' "$*" >>"${fixture}/auth-failed-order"; return 19; }
  configure_auth_peers
)
auth_status=$?
set -e
[[ "${auth_status}" == 19 && "$(wc -l <"${fixture}/auth-failed-order")" == 1 ]]
# Exercise the same local clone/check-out path from a genuinely shallow repo.
# shellcheck disable=SC1090
source <(sed -n '/^checkout_baseline_source() {/,/^}/p' scripts/release-controller-upgrade-smoke.sh)
# shellcheck disable=SC1090
source <(sed -n '/^require_changed_descriptor() {/,/^}/p' scripts/release-controller-upgrade-smoke.sh)
git init -q "${fixture}/origin"
git -C "${fixture}/origin" config user.name test
git -C "${fixture}/origin" config user.email test@example.invalid
git -C "${fixture}/origin" commit --allow-empty -qm parent
parent="$(git -C "${fixture}/origin" rev-parse HEAD)"
git -C "${fixture}/origin" commit --allow-empty -qm baseline
baseline_commit="$(git -C "${fixture}/origin" rev-parse HEAD)"
mkdir -p "${fixture}/origin/deploy/production"
printf 'changed\n' >"${fixture}/origin/deploy/production/compose.yaml"
git -C "${fixture}/origin" add deploy/production/compose.yaml
git -C "${fixture}/origin" commit -qm candidate
candidate_commit="$(git -C "${fixture}/origin" rev-parse HEAD)"
git clone -q --depth=1 "file://${fixture}/origin" "${fixture}/candidate"
(
  cd "${fixture}/candidate"
  if git cat-file -e "${baseline_commit}^{commit}" 2>/dev/null; then exit 1; fi
  checkout_baseline_source . "${fixture}/baseline" "${baseline_commit}"
  [[ "$(git rev-parse HEAD)" == "${candidate_commit}" ]]
  if git -C "${fixture}/baseline" cat-file -e "${parent}^{commit}" 2>/dev/null; then exit 1; fi
  [[ "$(git -C "${fixture}/baseline" rev-parse HEAD)" == "${baseline_commit}" ]]
  require_changed_descriptor "${fixture}/baseline" "${baseline_commit}" "${candidate_commit}" deploy/production/compose.yaml
  if require_changed_descriptor "${fixture}/baseline" "${baseline_commit}" "${candidate_commit}" unchanged; then exit 1; fi
  if require_changed_descriptor . "${baseline_commit}" "${candidate_commit}" deploy/production/compose.yaml 2>/dev/null; then exit 1; fi
  git clone -q --no-hardlinks . "${fixture}/active-candidate"
  git -C "${fixture}/active-candidate" fetch --quiet --no-tags --depth=1 "${fixture}/baseline" "${baseline_commit}"
  [[ "$(git -C "${fixture}/active-candidate" rev-parse HEAD)" == "${candidate_commit}" ]]
  require_changed_descriptor "${fixture}/active-candidate" "${baseline_commit}" "${candidate_commit}" deploy/production/compose.yaml
  if checkout_baseline_source . "${fixture}/invalid" invalid; then exit 1; fi
)
python3 scripts/test-release-business-smoke.py
node scripts/test-release-upgrade.mjs
bash scripts/test-release-session-compatibility.sh
node scripts/test-release-selection.mjs
node scripts/test-release-artifacts.mjs
ruby -r yaml -r json - <<'RUBY'
workflow = YAML.safe_load(File.read('.github/workflows/release-upgrade.yml'))
abort 'upgrade validation must not receive write permissions' unless workflow['permissions'] == {'contents' => 'read'}
jobs = workflow.fetch('jobs')
%w[agent-upgrade controller-upgrade session-compatibility].each do |name|
  matrix = jobs.fetch(name).fetch('strategy').fetch('matrix').fetch('include')
  abort "missing supported architecture: #{name}" unless matrix.map { |row| row['arch'] }.sort == %w[amd64 arm64]
end
baselines = jobs.fetch('session-compatibility').fetch('steps').filter_map { |step| step.dig('env', 'BASELINE_RELEASE') }
abort 'application matrix must contain only the supported baseline' unless baselines == ['v1.0.0']
%w[agent-upgrade controller-upgrade].each do |name|
  download = jobs.fetch(name).fetch('steps').find { |step| step.fetch('uses', '').start_with?('actions/download-artifact@') }
  abort 'frozen input must use its producer artifact ID' unless download.dig('with', 'artifact-ids')
end
gate = jobs.fetch('upgrade-result')
required = %w[prepare agent-upgrade controller-upgrade]
abort 'upgrade summary must observe all native units' unless gate.fetch('needs').sort == required.sort
command = gate.fetch('steps').find { |step| step.fetch('env', {}).key?('NEEDS') }.fetch('run')
results = required.to_h { |job| [job, {'result' => 'success'}] }
abort 'complete native units rejected' unless system({'NEEDS' => results.to_json}, 'bash', '-euc', command, out: File::NULL)
required.each do |job|
  %w[failure skipped cancelled].each do |state|
    changed = Marshal.load(Marshal.dump(results))
    changed[job]['result'] = state
    abort "#{job} #{state} accepted" if system({'NEEDS' => changed.to_json}, 'bash', '-euc', command, out: File::NULL)
  end
end
release = YAML.safe_load(File.read('.github/workflows/release.yml'))
publish = release.fetch('jobs').fetch('publish-release-packages')
abort 'production approval lost' unless publish['environment'] == 'release-publishing'
abort 'publish bypasses Release Check' unless publish.fetch('needs').include?('release-check')
abort 'manual dispatch can publish' unless publish['if'] == "github.event_name == 'push'"
release.fetch('jobs').each do |name, job|
  next if name == 'publish-release-packages'
  abort "write permission in validation: #{name}" if job.fetch('permissions', {}).values.include?('write')
  abort "production secret in validation: #{name}" if job.to_json.include?('secrets.') || job.key?('secrets')
end
abort 'source security missing' unless release['jobs'].values.any? { |job| job['uses'] == './.github/workflows/security.yml' }
puts 'Native workflow matrix, producer identity and job-result boundaries passed'
RUBY
