#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."
ruby -r yaml - <<'RUBY'
steps = YAML.safe_load(File.read('.github/workflows/release-products.yml')).fetch('jobs').fetch('build-agent-packages').fetch('steps')
target = steps.find { |s| s['id'] == 'agent-target' }.fetch('run')
build = File.read('scripts/build-agent-binaries.sh')
abort 'target must use the same builder hash as compilation' unless
  target.include?('sha256sum rust/agent-build.Dockerfile | cut -c1-16') &&
  build.include?('sha256sum "${ROOT}/rust/agent-build.Dockerfile" | cut -c1-16') &&
  target.include?('path=rust/target/agent-${{ inputs.arch }}-${builder_hash}') &&
  build.include?('rust/target/agent-${BUILD_CACHE_KEY}')
restore = steps.find { |s| s['id'] == 'agent-target-cache' }
save = steps.find { |s| s['name'] == 'Save Rocky Agent compilation cache' }
abort 'cache must be restored and saved explicitly' unless restore.fetch('uses').start_with?('actions/cache/restore@') && save.fetch('uses').start_with?('actions/cache/save@')
abort 'cache must contain only the isolated target directory' unless
  restore.dig('with', 'path') == '${{ steps.agent-target.outputs.path }}' && save.dig('with', 'path') == restore.dig('with', 'path')
key = restore.dig('with', 'key')
%w[runner.os inputs.arch rust/agent-build.Dockerfile toolchains.lock scripts/checksums.txt scripts/bootstrap.sh scripts/env.sh scripts/build-agent-binaries.sh rust/rust-toolchain.toml rust/.cargo/** rust/**/Cargo.toml rust/Cargo.lock].each do |boundary|
  abort "cache identity lost #{boundary}" unless key.include?(boundary)
end
abort 'fallback must not cross build identities' unless restore.dig('with', 'restore-keys') == key.delete_suffix('${{ github.sha }}')
abort 'save must use the new source identity, not the restored key' unless save.dig('with', 'key') == '${{ steps.agent-target-cache.outputs.cache-primary-key }}'
abort 'save must follow successful validation' unless save['if'] == "${{ success() && steps.agent-target-cache.outputs.cache-hit != 'true' }}" && steps.index(save) > steps.index(steps.find { |s| s['id'] == 'manifest' })
compile = steps.find { |s| s.fetch('run', '').include?('scripts/build-release-agent.sh') }
abort 'a cache hit must never skip candidate compilation' if compile.key?('if')
abort 'native ABI and candidate version checks must remain' unless
  build.include?('cargo build --locked --release') && build.include?('glibc 2.34') &&
  build.include?('[[ "$("${path}" --version)" == "${binary} ${VERSION}" ]]')
dockerfile = File.read('rust/transportd.Dockerfile')
abort 'recipe must include the complete workspace, not a hand-maintained crate list' unless
  dockerfile.include?("COPY rust/crates ./crates\nRUN cargo chef prepare --recipe-path recipe.json")
abort 'cook must match the real build package/profile/lock' unless
  dockerfile.include?('cargo chef cook --locked --release --package ocservia-transportd --recipe-path recipe.json') &&
  dockerfile.include?('cargo build --locked --release --package ocservia-transportd')
dependencies, application = dockerfile.split('FROM dependencies AS build', 2)
abort 'real sources, patched dependencies and rustflags must replace the recipe skeleton' unless
  application && ['COPY rust/crates ./crates', 'COPY rust/vendor ./vendor', 'COPY rust/.cargo ./.cargo'].all? { |line| application.include?(line) }
abort 'dependency layer must retain config and patched source inputs' unless
  dependencies.include?("COPY rust/.cargo ./.cargo") && dependencies.include?("COPY rust/vendor ./vendor")
puts 'Release Rust cache identity, unconditional rebuild and dependency layering contracts passed'
RUBY
