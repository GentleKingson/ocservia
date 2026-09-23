#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
ruby -r yaml -r json - "${ROOT}/.github/workflows/g6-harness-core.yml" <<'RUBY'
jobs = YAML.safe_load(File.read(ARGV[0]), aliases: true).fetch('jobs')
result = jobs.fetch('resilience-result')
abort 'both fault domains must contribute' unless result.fetch('needs').sort == %w[g6-rd-fd-a g6-rd-fd-b]
command = result.fetch('steps').find { |step| step.fetch('env', {}).key?('RESULTS') }.fetch('run')
results = result.fetch('needs').to_h { |job| [job, {'result'=>'success'}] }
abort 'successful runtime rejected' unless system({'RESULTS'=>results.to_json}, 'bash', '-euc', command, out: File::NULL)
results.keys.each do |job|
  %w[failure cancelled skipped].each do |state|
    changed = Marshal.load(Marshal.dump(results))
    changed[job]['result'] = state
    abort "invalid runtime accepted: #{job} #{state}" if system({'RESULTS'=>changed.to_json}, 'bash', '-euc', command, out: File::NULL)
  end
end
abort 'postprocessing layers returned' if (jobs.keys & %w[g6-rd-assemble g6-rd-verifier g6-rd-secret-scan g6-rd-gate]).any?
puts 'Fault-domain job failure/cancellation/skips block the single result calculation'
RUBY
