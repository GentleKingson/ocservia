#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${ROOT}"
node scripts/test-release-upgrade.mjs
ruby -r yaml - <<'RUBY'
w = YAML.safe_load(File.read('.github/workflows/release-upgrade.yml'))
triggers = w['on'] || w[true]
abort 'manual-only entrypoint required' unless triggers.keys == ['workflow_dispatch']
abort 'unexpected inputs' unless triggers['workflow_dispatch']['inputs'].keys.sort == %w[baseline_release candidate_sha version]
abort 'baseline default drift' unless triggers['workflow_dispatch']['inputs']['baseline_release']['default'] == 'v0.5.0'
abort 'write permissions' unless w['permissions'] == {'contents' => 'read'}
%w[agent-upgrade controller-upgrade].each do |name|
  job = w['jobs'][name]
  abort 'matrix must not fail fast' unless job['strategy']['fail-fast'] == false
  abort 'incomplete native matrix' unless job['strategy']['matrix']['include'] == [
    {'arch'=>'amd64','runner'=>'ubuntu-24.04'}, {'arch'=>'arm64','runner'=>'ubuntu-24.04-arm'}]
  abort 'must wait for frozen prepare' unless job['needs'] == 'prepare'
end
abort 'summary must always run' unless w['jobs']['upgrade-result']['if'] == 'always()'
abort 'summary graph incomplete' unless w['jobs']['upgrade-result']['needs'].sort == %w[agent-upgrade controller-upgrade prepare]
w['jobs'].each_value do |job|
  abort 'unsafe job' if job['environment'] || job['permissions'] || job['continue-on-error']
  abort 'explicit timeout required' unless job['timeout-minutes'].is_a?(Integer)
  job['steps'].each do |step|
    abort 'continue-on-error hides failures' if step['continue-on-error']
    abort 'action not pinned' if step['uses'] && !step['uses'].match?(/@[0-9a-f]{40}$/)
    abort 'checkout persists credentials' if step.fetch('uses','').start_with?('actions/checkout@') && step.dig('with','persist-credentials') != false
  end
end
puts 'Manual upgrade workflow contract passed'
release = YAML.safe_load(File.read('.github/workflows/release.yml'))
jobs = release.fetch('jobs')
abort 'release dispatch can publish' unless jobs['publish-release-packages']['if'] == "github.event_name == 'push'"
abort 'publication guard changed' unless jobs['publish-release-packages']['environment'] == 'release-publishing' &&
  jobs['publish-release-packages']['needs'].sort == %w[build-controller-images security validate-release-packages]
%w[agent controller].each do |component|
  job = jobs[component == 'agent' ? 'build-agent-packages' : 'build-controller-images']
  abort 'shared build path missing' unless job['steps'].any? {|s| s.fetch('run','').include?("bash scripts/build-release-#{component}.sh")}
end
build = File.read('scripts/build-release-controller.sh')
abort 'Controller no longer builds once' unless build.scan('docker buildx build').length == 1
abort 'Controller export changed' unless build.include?('type=docker,dest=') && build.include?('--platform "linux/${CONTROLLER_ARCH}"')
agent = File.read('scripts/build-release-agent.sh')
abort 'Agent release contract changed' unless agent.include?('OCSERV_AGENT_RELEASE_VERSION="${VERSION}" cargo build --locked --release') &&
  agent.include?('--package ocservia-agent --package ocservia-privd --package ocservia-upgrader')
puts 'Shared release build and publishing boundary contracts passed'
RUBY
