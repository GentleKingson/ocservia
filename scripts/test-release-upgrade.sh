#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${ROOT}"
fixture="$(mktemp -d)"
trap 'rm -rf -- "${fixture}"' EXIT
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
node scripts/test-release-upgrade.mjs
bash scripts/test-release-session-compatibility.sh
ruby -r yaml - <<'RUBY'
w = YAML.safe_load(File.read('.github/workflows/release-upgrade.yml'))
triggers = w['on'] || w[true]
abort 'manual-only entrypoint required' unless triggers.keys == ['workflow_dispatch']
abort 'unexpected inputs' unless triggers['workflow_dispatch']['inputs'].keys.sort == %w[baseline_release candidate_sha session_compatibility version]
abort 'session matrix must be opt-in' unless triggers['workflow_dispatch']['inputs']['session_compatibility'] == {
  'description'=>'Also run published v0.6.0 and v0.6.1 nodes against this candidate on both native architectures',
  'type'=>'boolean', 'default'=>false}
abort 'baseline default drift' unless triggers['workflow_dispatch']['inputs']['baseline_release']['default'] == 'v0.6.0'
abort 'write permissions' unless w['permissions'] == {'contents' => 'read'}
%w[agent-upgrade controller-upgrade].each do |name|
  job = w['jobs'][name]
  abort 'matrix must not fail fast' unless job['strategy']['fail-fast'] == false
  abort 'incomplete native matrix' unless job['strategy']['matrix']['include'] == [
    {'arch'=>'amd64','runner'=>'ubuntu-24.04'}, {'arch'=>'arm64','runner'=>'ubuntu-24.04-arm'}]
  abort 'must wait for frozen prepare' unless job['needs'] == 'prepare'
  execution = job['steps'].find { |step| step.fetch('run','').include?('bash scripts/release-upgrade-unit.sh') }.fetch('run')
  removal = "printf '%s\\n' -1 | sudo tee /proc/sys/fs/binfmt_misc/status >/dev/null"
  abort 'hosted binfmt handlers must be removed before strict native checks' unless
    execution.include?(removal) && execution.index(removal) < execution.index('bash scripts/release-upgrade-unit.sh')
end
session = w['jobs'].fetch('session-compatibility')
abort 'session matrix must be explicit and native' unless session['if'] == 'inputs.session_compatibility' &&
  session['needs'] == 'prepare' && session['strategy'] == w['jobs']['agent-upgrade']['strategy']
cells = session['steps'].select { |step| step.fetch('run','').include?('scripts/release-session-compatibility.sh run') }
abort 'published application baselines drift' unless cells.map { |step| step.dig('env','BASELINE_RELEASE') } == %w[v0.6.0 v0.6.1]
abort 'second cell must survive first cell failure, not build failure' unless
  cells[1]['if'] == "${{ !cancelled() && steps.build.outcome == 'success' }}"
abort 'session matrix must use the shipped transport launcher' unless
  File.read('scripts/build-release-session-images.sh').include?('build_image G6RD_TRANSPORTD_IMAGE transport rust/transportd.Dockerfile')
abort 'summary must always run' unless w['jobs']['upgrade-result']['if'] == 'always()'
abort 'summary graph incomplete' unless w['jobs']['upgrade-result']['needs'].sort == %w[agent-upgrade controller-upgrade prepare]
w['jobs'].each_value do |job|
  abort 'unsafe job' if job['environment'] || job['permissions'] || job['continue-on-error']
  abort 'explicit timeout required' unless job['timeout-minutes'].is_a?(Integer)
  job['steps'].each do |step|
    abort 'continue-on-error hides failures' if step['continue-on-error']
    abort 'action not pinned' if step['uses'] && step['uses'] != './.github/actions/g6-cache-credentials' && !step['uses'].match?(/@[0-9a-f]{40}$/)
    abort 'checkout persists credentials' if step.fetch('uses','').start_with?('actions/checkout@') && step.dig('with','persist-credentials') != false
  end
end
puts 'Manual upgrade workflow contract passed'
release = YAML.safe_load(File.read('.github/workflows/release.yml'))
jobs = release.fetch('jobs')
baseline_steps = jobs['build-agent-packages']['steps'].select { |step| step.fetch('name','').start_with?('Validate published ') }
abort 'published upgrade baseline drift' unless baseline_steps.length == 1 &&
  baseline_steps[0]['name'] == 'Validate published v0.6.0 upgrade (${{ matrix.package_arch }})' &&
  baseline_steps[0].dig('env','BASELINE_RELEASE') == 'v0.6.0'
abort 'release dispatch can publish' unless jobs['publish-release-packages']['if'] == "github.event_name == 'push'"
abort 'release must call candidate security checks' unless
  jobs.fetch('security').fetch('uses') == './.github/workflows/security.yml'
abort 'publication guard changed' unless jobs['publish-release-packages']['environment'] == 'release-publishing' &&
  jobs['publish-release-packages']['needs'].sort == %w[build-controller-images security validate-release-packages]
%w[agent controller].each do |component|
  job = jobs[component == 'agent' ? 'build-agent-packages' : 'build-controller-images']
  abort 'shared build path missing' unless job['steps'].any? {|s| s.fetch('run','').include?("bash scripts/build-release-#{component}.sh")}
end
build = File.read('scripts/build-release-controller.sh')
abort 'Controller no longer builds once' unless build.scan('scripts/g6-buildx-cache.sh').length == 1
abort 'Controller caches must isolate image and architecture' unless build.include?('controller-v1-${name}-linux-${CONTROLLER_ARCH}')
abort 'Controller export changed' unless build.include?('type=docker,dest=') && build.include?('--platform "linux/${CONTROLLER_ARCH}"')
agent = File.read('scripts/build-release-agent.sh')
abort 'shared Agent build must use the native ABI builder' unless
  agent.include?('bash "${ROOT}/scripts/build-agent-binaries.sh"') && !agent.include?('cargo build')
abi = File.read('scripts/build-agent-binaries.sh')
controller_upgrade = File.read('scripts/release-controller-upgrade-smoke.sh')
abort 'Controller smoke must check out the frozen baseline source' unless
  controller_upgrade.include?('checkout_baseline_source "${ROOT}" "${work}/baseline" "${baseline_commit}"')
abort 'rollback evidence must compare the verified trees and reject Git errors' unless
  controller_upgrade.include?('require_changed_descriptor "${work}/baseline" "${baseline_commit}" "${candidate_commit}" "${descriptor}"') &&
  controller_upgrade.include?('git -C "${work}/candidate" fetch --quiet --no-tags --depth=1 "${work}/baseline" "${baseline_commit}"')
abort 'Agent release contract changed' unless abi.include?('OCSERV_AGENT_RELEASE_VERSION="${VERSION}" cargo build --locked --release') &&
  abi.include?('--package ocservia-agent --package ocservia-privd --package ocservia-upgrader') &&
  abi.include?('CARGO_TARGET_DIR="${OCSERVIA_ROOT}/rust/target/agent-${BUILD_CACHE_KEY}"') &&
  abi.include?('[[ "$(getconf GNU_LIBC_VERSION)" == "glibc 2.34" ]]') &&
  abi.include?('[[ "$("${path}" --version)" == "${binary} ${VERSION}" ]]')
abort 'Agent ABI builder must pin its Rocky 9 base' unless
  File.read('rust/agent-build.Dockerfile').match?(/^FROM rockylinux:9@sha256:[0-9a-f]{64}$/)
native = File.read('scripts/release-native-package-smoke.sh')
abort 'real native lifecycle fixtures must use the ABI builder' unless
  native.include?('build-agent-binaries.sh') && !native.include?('cargo build')
abort 'duplicate native build returned' unless native.scan('bash "${ROOT}/scripts/build-agent-binaries.sh"').length == 1
lifecycle = jobs['build-agent-packages']['steps'].find { |step| step.fetch('name','').start_with?('Validate native package lifecycle') }
abort 'release lifecycle must reuse the real candidate' unless
  lifecycle.dig('env','CANDIDATE_DIR') == '${{ runner.temp }}/packages' && lifecycle.dig('env','VERSION') == '${{ env.version }}'
jobs.each_value do |job|
  job.fetch('steps', []).each do |step|
    next unless step.fetch('uses','').start_with?('actions/download-artifact@')
    pattern = step.fetch('with').fetch('pattern')
    abort 'release download must bind both exact architectures and this attempt' unless
      pattern.end_with?('-{amd64,arm64}-${{ github.run_id }}-${{ github.run_attempt }}')
  end
end
baseline = File.read('scripts/release-baseline-upgrade-smoke.sh')
abort 'candidate package smoke must execute all three binaries in both runtimes' unless
  baseline.scan('for binary in ocservia-agent ocservia-privd ocservia-upgrader; do').length == 2 &&
  baseline.include?('sudo "/usr/libexec/ocservia/${binary}" --version') &&
  baseline.include?('docker exec "${container}" "/usr/libexec/ocservia/${binary}" --version')
puts 'Shared release build and publishing boundary contracts passed'
RUBY
