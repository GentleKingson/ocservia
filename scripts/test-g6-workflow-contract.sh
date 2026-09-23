#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
ruby -r yaml - "${ROOT}/.github/workflows/g6-harness-core.yml" <<'RUBY'
workflow = YAML.safe_load(File.read(ARGV[0]), aliases: true)
abort 'resilience must not have publication permissions' unless workflow['permissions'] == {'contents'=>'read', 'actions'=>'read'}
jobs = workflow.fetch('jobs')
jobs.each do |id, job|
  abort "missing bounded timeout: #{id}" unless job['timeout-minutes'].is_a?(Integer) && job['timeout-minutes'] > 0
  job.fetch('steps').each do |step|
    action = step['uses']
    abort "unpinned action: #{id}" if action && !action.start_with?('./') && !action.match?(/@[0-9a-f]{40}\z/)
  end
end
%w[g6-rd-fd-a g6-rd-fd-b].each do |id|
  steps = jobs.fetch(id).fetch('steps')
  abort "cleanup is missing: #{id}" unless steps.any? { |s| s.fetch('run', '').include?('cleanup --domain') && s.fetch('if', '').include?('always()') }
  scan = steps.index { |s| s['id'] == 'raw-scan' }
  upload = steps.index { |s| s.fetch('id', '').end_with?('-raw-upload') }
  abort "raw output may upload before secret scan: #{id}" unless scan && upload && scan < upload &&
    steps[upload].fetch('if', '').include?("steps.raw-scan.outcome == 'success'")
end
puts 'Read-only resilience, bounded execution, cleanup and pre-upload scan boundaries passed'
RUBY
