#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
ruby -r yaml - "${ROOT}/.github/workflows/g6-harness-core.yml" <<'RUBY'
core = YAML.safe_load(File.read(ARGV[0]), aliases: true)
jobs = core.fetch("jobs")
formal = jobs.fetch("g6-rd-release-image")
build = Array(formal.fetch("steps")).find { |step| step["name"] == "Build and freeze the release images" }.fetch("run")
%w[.tools/go/bin/go GOTOOLCHAIN=local GOOS=linux GOARCH=amd64 ocservia-g6-harness release-artifacts.sha256 image-ids.tsv].each do |token|
  abort("formal release is missing #{token}") unless build.include?(token)
end
abort("formal release fell back to ambient Go") if build.match?(/(?:^|\s)go\s+build(?:\s|$)/)
%w[g6-rd-fd-a g6-rd-fd-b].each do |id|
  job = jobs.fetch(id)
  abort("#{id} must consume the shared release") unless job.fetch("needs") == "g6-rd-release-image"
  load = Array(job.fetch("steps")).find { |step| step["name"] == "Verify and load the release images" }
  abort("#{id} must use the shared frozen release verifier") unless
    load.fetch("uses") == "./.github/actions/g6-install-release"
end
RUBY
