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
bash scripts/test-release-test-images.sh
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
release_jobs = release.fetch('jobs')
fixtures = YAML.safe_load(File.read('.github/workflows/release-test-images.yml'))
abort 'fixture preparation must remain read-only' unless fixtures['permissions'] == {'contents' => 'read'}
fixture_steps = fixtures.fetch('jobs').fetch('prepare').fetch('steps')
%w[helpers session rpm].zip(%w[test-helpers session-base rpm-test]).each do |id, component|
  build = fixture_steps.find { |step| step['id'] == id }
  abort "missing fixture recipe #{component}" unless build.fetch('run').include?("release-test-images.sh build #{component}")
  abort 'single-architecture diagnostics only need RPM fixtures' unless build['if'] == (id == 'rpm' ? nil : 'inputs.complete')
end
{'release-compatibility.yml' => %w[test-helpers session-base],
 'release-business.yml' => %w[test-helpers], 'release-product-upgrade.yml' => %w[rpm-test],
 'g6-harness-core.yml' => %w[test-helpers]}.each do |file, components|
  workflow = YAML.safe_load(File.read(".github/workflows/#{file}"))
  consumers = workflow.fetch('jobs').values.flat_map { |job| job.fetch('steps') }.select { |step| step['uses'] == './.github/actions/release-test-images' }
  abort "fixture reuse missing from #{file}" unless consumers.map { |step| step.dig('with', 'component') } == components
  consumers.each do |step|
    prefix = {'test-helpers' => 'helpers', 'session-base' => 'session', 'rpm-test' => 'rpm'}.fetch(step.dig('with', 'component'))
    abort "#{file} lost producer identity" unless step.dig('with', 'artifact-id') == "${{ inputs.#{prefix}-id }}" && step.dig('with', 'sha256') == "${{ inputs.#{prefix}-sha256 }}"
  end
end
products = YAML.safe_load(File.read('.github/workflows/release-products.yml'))
abort 'product producers must not wait for upgrades' if products.to_json.include?('release-upgrade-unit.sh') || products.to_json.include?('frozen-')
upgrades = YAML.safe_load(File.read('.github/workflows/release-product-upgrade.yml'))
abort 'upgrades must remain read-only and secret-free' unless upgrades['permissions'] == {'contents' => 'read'} && !upgrades.to_json.include?('secrets')
unit = upgrades.fetch('jobs').fetch('upgrade')
abort 'upgrade units must be isolated native jobs' unless unit['runs-on'] == "${{ inputs.arch == 'arm64' && 'ubuntu-24.04-arm' || 'ubuntu-24.04' }}"
abort 'one failed component must not cancel another' unless unit.dig('strategy', 'fail-fast') == false
matrix = unit.fetch('strategy').fetch('matrix').fetch('include')
abort 'both product upgrades required' unless matrix.map { |row| row['component'] }.sort == %w[agent controller]
matrix.each do |row|
  component = row.fetch('component')
  abort 'upgrade must consume its own producer identity' unless row['artifact-id'] == "${{ inputs.#{component}-id }}" && row['sha256'] == "${{ inputs.#{component}-sha256 }}"
end
steps = unit.fetch('steps')
download = steps.find { |step| step.fetch('uses', '').start_with?('actions/download-artifact@') }
abort 'upgrade lost frozen producer ID' unless download.dig('with', 'artifact-ids') == '${{ inputs.frozen-id }}'
consume = steps.find { |step| step['uses'] == './.github/actions/release-artifacts' }.fetch('with')
{'artifact-id' => '${{ matrix.artifact-id }}', 'sha256' => '${{ matrix.sha256 }}',
 'component' => '${{ matrix.component }}', 'arch' => '${{ inputs.arch }}',
 'version' => '${{ inputs.version }}'}.each do |key, value|
  abort "upgrade candidate verification lost #{key}" unless consume[key] == value
end
validate = steps.find { |step| step.fetch('run', '').include?('release-upgrade-unit.sh') }
abort 'upgrade must use existing unit entrypoint' unless validate['run'] == 'bash scripts/release-upgrade-unit.sh "${{ matrix.component }}" "${{ inputs.arch }}"'
abort 'upgrade must reuse verified candidates without rebuilding' unless validate.dig('env', 'CANDIDATE_PRODUCTS') == consume['path'] && validate.dig('env', 'CANDIDATE_MANIFEST_SHA256') == consume['sha256']
abort 'upgrade lost frozen digest' unless validate.dig('env', 'FROZEN_SHA256') == '${{ inputs.frozen-sha256 }}'
abort 'upgrade lost frozen file' unless validate.dig('env', 'FROZEN_FILE') == "#{download.dig('with', 'path')}/frozen.json"
%w[amd64 arm64].each do |arch|
  caller = release_jobs.fetch("upgrade-#{arch}")
  abort 'upgrade must follow its native products, fixtures and prepare' unless caller['needs'].sort == ['prepare', "build-#{arch}", "test-images-#{arch}"].sort
  abort 'upgrade must also run for single-architecture diagnostics' if caller.key?('if')
  abort 'upgrade workflow not called' unless caller['uses'] == './.github/workflows/release-product-upgrade.yml'
  expected = {'arch' => arch, 'version' => '${{ needs.prepare.outputs.version }}'}
  %w[frozen-id frozen-sha256].each { |key| expected[key] = "${{ needs.prepare.outputs.#{key} }}" }
  %w[agent-id agent-sha256 controller-id controller-sha256].each { |key| expected[key] = "${{ needs.build-#{arch}.outputs.#{key} }}" }
  %w[rpm-id rpm-sha256].each { |key| expected[key] = "${{ needs.test-images-#{arch}.outputs.#{key} }}" }
  abort 'upgrade caller must preserve all producer identities' unless caller['with'] == expected
  abort 'Release Check must observe upgrade results' unless release_jobs.fetch('release-check').fetch('needs').include?("upgrade-#{arch}")
  producer = release_jobs.fetch("test-images-#{arch}")
  abort 'fixtures must prepare independently of candidate builds' unless producer['needs'] == 'prepare' && producer['uses'] == './.github/workflows/release-test-images.yml'
  abort 'fixture architecture selection drifted' unless producer['if'] == release_jobs.fetch("build-#{arch}")['if']
  abort 'Release Check must observe fixture preparation' unless release_jobs.fetch('release-check').fetch('needs').include?("test-images-#{arch}")
end
%w[compatibility-amd64 compatibility-arm64 business-smoke integration resilience validate-release-packages controller-image-security].each do |name|
  arch = name == 'compatibility-arm64' ? 'arm64' : 'amd64'
  expected = %w[validate-release-packages controller-image-security].include?(name) ? %w[prepare build-amd64 build-arm64] : ['prepare', "build-#{arch}", "test-images-#{arch}"]
  abort "#{name} must not wait for upgrade results" unless release_jobs.fetch(name).fetch('needs').sort == expected.sort
  next if %w[validate-release-packages controller-image-security].include?(name)
  %w[helpers-id helpers-sha256].each do |key|
    abort "#{name} lost shared fixture identity" unless release_jobs.fetch(name).dig('with', key) == "${{ needs.test-images-#{arch}.outputs.#{key} }}"
  end
end
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
release_gate = release.fetch('jobs').fetch('release-check')
gate_command = release_gate.fetch('steps').find { |step| step.dig('env', 'RESULTS') }.fetch('run')
[false, true].repeated_permutation(2) do |integration, resilience|
  selection = {'candidate' => 'a' * 40, 'integration' => {'selected' => integration}, 'resilience' => {'selected' => resilience}}
  results = release_gate.fetch('needs').to_h { |job| [job, {'result' => 'success'}] }
  results['integration']['result'] = integration ? 'success' : 'skipped'
  results['resilience']['result'] = resilience ? 'success' : 'skipped'
  results['business-smoke']['result'] = integration ? 'skipped' : 'success'
  env = {'SELECTION' => selection.to_json, 'GITHUB_SHA' => selection['candidate']}
  abort 'valid selected release scope rejected' unless system(env.merge('RESULTS' => results.to_json), 'bash', '-euc', gate_command, out: File::NULL)
  results.each_key do |job|
    ['failure', 'cancelled', 'skipped', nil].each do |state|
      next if results[job]['result'] == state
      changed = Marshal.load(Marshal.dump(results))
      changed[job]['result'] = state
      abort "release accepted #{job}: #{state}" if system(env.merge('RESULTS' => changed.to_json), 'bash', '-euc', gate_command, out: File::NULL, err: File::NULL)
    end
  end
end
puts 'Native workflow matrix, producer identity and job-result boundaries passed'
RUBY
