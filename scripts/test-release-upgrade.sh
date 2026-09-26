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
python3 scripts/test-release-business-smoke.py
node scripts/test-release-upgrade.mjs
node scripts/test-release-selection.mjs
node scripts/test-release-artifacts.mjs
node scripts/test-reuse-accepted-products.mjs
bash scripts/test-controller-candidate.sh
bash scripts/test-release-test-images.sh
bash scripts/test-release-rust-cache.sh
ruby -r yaml -r json -r tmpdir - <<'RUBY'
workflow = YAML.safe_load(File.read('.github/workflows/release-upgrade.yml'))
inputs = workflow.fetch('on', workflow[true]).fetch('workflow_dispatch').fetch('inputs')
abort 'historical diagnostics remain exposed' unless inputs.fetch('purpose').fetch('options') == %w[smoke integration] && !inputs.key?('baseline_release')
%w[release-product-upgrade.yml release-compatibility.yml].each do |file|
  abort "historical workflow remains: #{file}" if File.exist?(".github/workflows/#{file}")
end
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
release = YAML.safe_load(File.read('.github/workflows/release.yml'))
release_jobs = release.fetch('jobs')
fixtures = YAML.safe_load(File.read('.github/workflows/release-test-images.yml'))
abort 'fixture preparation must remain read-only' unless fixtures['permissions'] == {'contents' => 'read'}
build = fixtures.fetch('jobs').fetch('prepare').fetch('steps').find { |step| step['id'] == 'fixture' }
abort 'fixture lost selected component or architecture' unless
  build.dig('env', 'COMPONENT') == '${{ inputs.component }}' && build.dig('env', 'ARCH') == '${{ inputs.arch }}'
Dir.mktmpdir('release-invocation-') do |dir|
  Dir.mkdir("#{dir}/scripts")
  File.write("#{dir}/scripts/release-test-images.sh", "#!/usr/bin/env bash\n" + 'printf "%s\n" "$@" >"$CAPTURE"' + "\n")
  %w[amd64 arm64].each do |arch|
    env = {'COMPONENT' => 'test-helpers', 'ARCH' => arch, 'RUNNER_TEMP' => dir, 'CAPTURE' => "#{dir}/args"}
    abort 'fixture invocation failed' unless system(env, 'bash', '-euc', build.fetch('run'), chdir: dir)
    abort 'fixture arguments drifted' unless File.readlines(env['CAPTURE'], chomp: true) == ['build', 'test-helpers', arch, "#{dir}/fixture"]
  end
end
producer = release_jobs.fetch('helpers-amd64')
expected = {'component' => 'test-helpers', 'arch' => 'amd64', 'version' => '${{ needs.prepare.outputs.version }}'}
abort 'fixture recipe identity drifted' unless expected.all? { |key, value| producer.dig('with', key) == value }
abort 'fixture selection drifted' unless producer['if'] == "needs.prepare.outputs.complete == 'true'"
%w[business-smoke integration resilience].each do |name|
  {'id' => 'artifact-id', 'sha256' => 'sha256'}.each do |input, output|
    abort "#{name} lost helper identity" unless release_jobs.fetch(name).dig('with', "helpers-#{input}") == "${{ needs.helpers-amd64.outputs.#{output} }}"
  end
end
{'release-business.yml' => %w[test-helpers],
 'g6-harness-core.yml' => %w[test-helpers]}.each do |file, components|
  workflow = YAML.safe_load(File.read(".github/workflows/#{file}"))
  consumers = workflow.fetch('jobs').values.flat_map { |job| job.fetch('steps') }.select { |step| step['uses'] == './.github/actions/release-test-images' }
  abort "fixture reuse missing from #{file}" unless consumers.map { |step| step.dig('with', 'component') } == components
  consumers.each do |step|
    abort "#{file} lost producer identity" unless step.dig('with', 'artifact-id') == '${{ inputs.helpers-id }}' && step.dig('with', 'sha256') == '${{ inputs.helpers-sha256 }}'
  end
end
products = YAML.safe_load(File.read('.github/workflows/release-products.yml'))
abort 'product producers must not wait for upgrades' if products.to_json.include?('release-upgrade-unit.sh') || products.to_json.include?('frozen-')
abort 'release still requires historical inputs' if release.to_json.match?(/BASELINE_RELEASE|frozen-id|release-upgrade-contract/)
abort 'historical release jobs remain' if release_jobs.keys.any? { |name| name.start_with?('upgrade-', 'compatibility-', 'session-', 'rpm-') }
product_jobs = products.fetch('jobs')
abort 'stable publication must reuse accepted products instead of building' unless
  product_jobs.fetch('reuse-accepted')['if'] == "github.event_name == 'push'" &&
  product_jobs.fetch('build-agent-packages')['if'] == "github.event_name != 'push'" &&
  product_jobs.fetch('build-controller-images')['if'] == "github.event_name == 'workflow_dispatch'"
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
puts 'Current candidate workflow, producer identity and job-result boundaries passed'
RUBY
