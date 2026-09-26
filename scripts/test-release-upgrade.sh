#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${ROOT}"
fixture="$(mktemp -d)"
trap 'rm -rf -- "${fixture}"' EXIT
(
  VERSION=1.0.2
  for PRODUCTION_SIGNER_ACCEPTANCE in false true; do
    # shellcheck disable=SC1090
    source <(sed -n '/^managed_options=/,+1p' scripts/release-business-probe.sh)
    [[ "${managed_options[0]} ${managed_options[1]}" == '--version v1.0.2' ]]
    if [[ "$PRODUCTION_SIGNER_ACCEPTANCE" == true ]]; then
      [[ "${managed_options[2]}" == --root-lifecycle && "${#managed_options[@]}" == 3 ]]
    else
      [[ "${#managed_options[@]}" == 2 ]]
    fi
  done
)
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
node scripts/test-reuse-accepted-products.mjs
bash scripts/test-controller-candidate.sh
bash scripts/test-release-test-images.sh
bash scripts/test-release-rust-cache.sh
ruby -r yaml -r json - <<'RUBY'
workflow = YAML.safe_load(File.read('.github/workflows/release-upgrade.yml'))
abort 'upgrade validation must not receive write permissions' unless workflow['permissions'] == {'contents' => 'read'}
jobs = workflow.fetch('jobs')
ordinary = jobs.fetch('business-probe')
privileged = jobs.fetch('production-signer-probe')
abort 'ordinary business diagnostics gained write permissions' unless ordinary['permissions'] == {'contents' => 'read', 'packages' => 'read'}
abort 'ordinary diagnostics must exclude production publication' unless
  ordinary['if'] == "${{ !inputs.production_signer && (inputs.purpose == 'smoke' || inputs.purpose == 'integration') }}" &&
  ordinary.dig('with', 'production_signer') == false
abort 'candidate publication must be explicitly selected integration' unless
  privileged['if'] == "${{ inputs.production_signer && inputs.purpose == 'integration' }}" &&
  privileged['permissions'] == {'contents' => 'read', 'packages' => 'read'} &&
  privileged.dig('with', 'production_signer') == true && privileged.dig('with', 'profile') == 'extended'
abort 'only the opt-in caller may hold write permissions' unless
  jobs.select { |_, job| job.fetch('permissions', {}).values.include?('write') }.keys == ['production-signer-products']
diagnostic_path = './.github/workflows/release-business-diagnostic.yml'
abort 'business callers must use the same checks' unless ordinary['uses'] == diagnostic_path && privileged['uses'] == diagnostic_path
diagnostic = YAML.safe_load(File.read(diagnostic_path))
consumer = diagnostic.fetch('jobs').fetch('business')
producer = jobs.fetch('production-signer-products')
abort 'clean consumer must be read-only' unless consumer['permissions'] == {'contents' => 'read', 'packages' => 'read'}
abort 'Registry publication must remain opt-in' unless producer['if'] == "${{ inputs.production_signer && inputs.purpose == 'integration' }}" &&
  producer['permissions'] == {'contents' => 'read', 'actions' => 'read', 'packages' => 'write'} &&
  producer['uses'] == './.github/workflows/release-integrated-candidate.yml'
abort 'diagnostics must contain no write-capable job' if
  diagnostic.fetch('jobs').values.any? { |job| job.fetch('permissions', {}).values.include?('write') }
candidate = YAML.safe_load(File.read('.github/workflows/release-integrated-candidate.yml')).fetch('jobs')
abort 'only publication may write packages' unless
  candidate.select { |_, job| job.fetch('permissions', {}).values.include?('write') }.keys == ['publish']
%w[amd64 arm64].each do |arch|
  abort 'candidate must reuse native release products' unless
    candidate.fetch(arch)['uses'] == './.github/workflows/release-products.yml' &&
    candidate.fetch(arch).dig('with', 'arch') == arch
end
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
build = fixture_steps.find { |step| step['id'] == 'fixture' }
abort 'fixture invocation must build exactly its selected component' unless build.fetch('run') == 'bash scripts/release-test-images.sh build "$COMPONENT" "$ARCH" "${RUNNER_TEMP}/fixture"' && build.dig('env', 'COMPONENT') == '${{ inputs.component }}'
abort 'fixture invocation must publish one artifact' unless fixture_steps.count { |step| step.fetch('uses', '').start_with?('actions/upload-artifact@') } == 1
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
product_jobs = products.fetch('jobs')
abort 'stable publication must reuse accepted products instead of building' unless
  product_jobs.fetch('reuse-accepted')['if'] == "github.event_name == 'push'" &&
  product_jobs.fetch('build-agent-packages')['if'] == "github.event_name != 'push'" &&
  product_jobs.fetch('build-controller-images')['if'] == "github.event_name == 'workflow_dispatch'"
upgrades = YAML.safe_load(File.read('.github/workflows/release-product-upgrade.yml'))
abort 'upgrades must remain read-only and secret-free' unless upgrades['permissions'] == {'contents' => 'read'} && !upgrades.to_json.include?('secrets')
unit = upgrades.fetch('jobs').fetch('upgrade')
abort 'upgrade units must be isolated native jobs' unless unit['runs-on'] == "${{ inputs.arch == 'arm64' && 'ubuntu-24.04-arm' || 'ubuntu-24.04' }}"
abort 'upgrade invocation must contain only one component' if unit.key?('strategy')
steps = unit.fetch('steps')
download = steps.find { |step| step.fetch('uses', '').start_with?('actions/download-artifact@') }
abort 'upgrade lost frozen producer ID' unless download.dig('with', 'artifact-ids') == '${{ inputs.frozen-id }}'
consume = steps.find { |step| step['uses'] == './.github/actions/release-artifacts' }.fetch('with')
{'artifact-id' => '${{ inputs.artifact-id }}', 'sha256' => '${{ inputs.sha256 }}',
 'component' => '${{ inputs.component }}', 'arch' => '${{ inputs.arch }}',
 'version' => '${{ inputs.version }}'}.each do |key, value|
  abort "upgrade candidate verification lost #{key}" unless consume[key] == value
end
validate = steps.find { |step| step.fetch('run', '').include?('release-upgrade-unit.sh') }
abort 'upgrade must use existing unit entrypoint' unless validate['run'] == 'bash scripts/release-upgrade-unit.sh "${{ inputs.component }}" "${{ inputs.arch }}"'
abort 'upgrade must reuse verified candidates without rebuilding' unless validate.dig('env', 'CANDIDATE_PRODUCTS') == consume['path'] && validate.dig('env', 'CANDIDATE_MANIFEST_SHA256') == consume['sha256']
abort 'upgrade lost frozen digest' unless validate.dig('env', 'FROZEN_SHA256') == '${{ inputs.frozen-sha256 }}'
abort 'upgrade lost frozen file' unless validate.dig('env', 'FROZEN_FILE') == "#{download.dig('with', 'path')}/frozen.json"
rpm_consumer = steps.find { |step| step['uses'] == './.github/actions/release-test-images' }
abort 'only Agent upgrades may consume RPM fixtures' unless rpm_consumer['if'] == "inputs.component == 'agent'"
%w[amd64 arm64].each do |arch|
  %w[agent controller].each do |component|
    name = "upgrade-#{component}-#{arch}"
    caller = release_jobs.fetch(name)
    dependencies = ['prepare', "build-#{arch}"]
    dependencies << "rpm-#{arch}" if component == 'agent'
    abort "#{name} has unrelated dependencies" unless caller['needs'].sort == dependencies.sort
    abort 'upgrade must also run for single-architecture diagnostics' if caller.key?('if')
    abort 'upgrade workflow not called' unless caller['uses'] == './.github/workflows/release-product-upgrade.yml'
    expected = {'component' => component, 'arch' => arch, 'version' => '${{ needs.prepare.outputs.version }}',
                'artifact-id' => "${{ needs.build-#{arch}.outputs.#{component}-id }}",
                'sha256' => "${{ needs.build-#{arch}.outputs.#{component}-sha256 }}"}
    %w[frozen-id frozen-sha256].each { |key| expected[key] = "${{ needs.prepare.outputs.#{key} }}" }
    if component == 'agent'
      expected['rpm-id'] = "${{ needs.rpm-#{arch}.outputs.artifact-id }}"
      expected['rpm-sha256'] = "${{ needs.rpm-#{arch}.outputs.sha256 }}"
    end
    abort 'upgrade caller must preserve all producer identities' unless caller['with'] == expected
    abort 'Release Check must observe upgrade results' unless release_jobs.fetch('release-check').fetch('needs').include?(name)
  end
  {'helpers' => 'test-helpers', 'session' => 'session-base', 'rpm' => 'rpm-test'}.each do |prefix, component|
    producer = release_jobs.fetch("#{prefix}-#{arch}")
    abort 'fixtures must prepare independently' unless producer['needs'] == 'prepare' && producer['uses'] == './.github/workflows/release-test-images.yml'
    expected = {'component' => component, 'arch' => arch, 'version' => '${{ needs.prepare.outputs.version }}'}
    abort 'fixture recipe identity drifted' unless producer['with'] == expected
    condition = prefix == 'rpm' ? release_jobs.fetch("build-#{arch}")['if'] : "needs.prepare.outputs.complete == 'true'"
    abort 'fixture selection drifted' unless producer['if'] == condition
  end
end
%w[compatibility-amd64 compatibility-arm64 business-smoke integration resilience validate-release-packages controller-image-security].each do |name|
  arch = name == 'compatibility-arm64' ? 'arm64' : 'amd64'
  security = %w[validate-release-packages controller-image-security].include?(name)
  compatibility = name.start_with?('compatibility-')
  expected = security ? %w[prepare build-amd64 build-arm64] : ['prepare', "build-#{arch}", "helpers-#{arch}"]
  expected << "session-#{arch}" if compatibility
  abort "#{name} has unrelated dependencies" unless release_jobs.fetch(name).fetch('needs').sort == expected.sort
  next if security
  prefixes = compatibility ? %w[helpers session] : %w[helpers]
  prefixes.each do |prefix|
    {'id' => 'artifact-id', 'sha256' => 'sha256'}.each do |input, output|
      abort "#{name} lost #{prefix} identity" unless release_jobs.fetch(name).dig('with', "#{prefix}-#{input}") == "${{ needs.#{prefix}-#{arch}.outputs.#{output} }}"
    end
  end
end
publish = release.fetch('jobs').fetch('publish-release-packages')
publish_names = publish.fetch('steps').map { |step| step['name'] }
bind_index = publish_names.index('Verify accepted bindings before any stable Registry write')
push_index = publish_names.index('Push Controller images & assemble multi-platform indexes')
abort 'accepted bindings must be verified on the publishing runner before Registry writes' unless bind_index && push_index && bind_index < push_index
binding = publish.fetch('steps').fetch(bind_index)
abort 'both native legs must reuse the same accepted bundle before publishing' unless
  binding.dig('env', 'ARM64_EXPECTED') == '${{ needs.build-arm64.outputs.accepted-sha256 }}' &&
  binding.fetch('run').include?('[[ "$EXPECTED" =~ ^[0-9a-f]{64}$ && "$EXPECTED" == "$ARM64_EXPECTED" ]]')
abort 'dispatch package validation must not require stable acceptance' if
  release_jobs.fetch('validate-release-packages').to_json.include?('accepted-integrated')
abort 'production approval lost' unless publish['environment'] == 'release-publishing'
abort 'publish bypasses Release Check' unless publish.fetch('needs').include?('release-check')
abort 'publish must handle optional skips but reject dispatch, cancellation and failed acceptance' unless
  publish['if'] == "${{ !cancelled() && github.event_name == 'push' && needs.release-check.result == 'success' }}"
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
