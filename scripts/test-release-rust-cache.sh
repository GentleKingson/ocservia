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
abort 'save must follow successful validation' unless save['if'] == "${{ success() && steps.agent-target-cache.outputs.cache-hit != 'true' }}" && steps.index(save) > steps.index(steps.find { |s| s['name'].start_with?('Validate native package lifecycle') })
compile = steps.find { |s| s.fetch('run', '').include?('scripts/build-release-agent.sh') }
abort 'a cache hit must never skip binary compilation' if compile.key?('if')
abort 'native ABI and package version checks must remain' unless
  build.include?('cargo build --locked --release') && build.include?('glibc 2.34') &&
  build.include?('[[ "$("${path}" --version)" == "${binary} ${VERSION}" ]]')
builder = File.read('rust/agent-build.Dockerfile')
%w[minrate=512k timeout=15 max_parallel_downloads=10].each do |option|
  abort "DNF slow-mirror bound missing #{option}" unless builder.include?("--setopt=#{option}")
end
abort 'keep official mirror selection, retry defaults and package verification' if
  builder.match?(/retries=|fastestmirror=|mirrorlist=|baseurl=|gpgcheck=0|sslverify=0|--nogpgcheck/)
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
ruby -r yaml - <<'RUBY'
require 'fileutils'
require 'open3'
require 'tmpdir'
def check(value, message)
  abort message unless value
end
workflows = %w[ci security release-business-diagnostic].to_h do |name|
  [name, YAML.safe_load(File.read(".github/workflows/#{name}.yml"))]
end
workflows.each do |name, workflow|
  workflow.fetch('jobs').each_value do |job|
    job.fetch('steps', []).each do |step|
      next unless step.fetch('uses', '').start_with?('actions/cache')
      options = step.fetch('with')
      paths = options.fetch('path').lines.map(&:strip)
      check(paths.none? { |path| path.include?('node_modules') }, "#{name} must not cache installed npm dependencies")
      next unless options.fetch('key').start_with?('tooling-')
      key = options.fetch('key')
      check(!key.include?('github.sha'), "#{name} stable tools must survive source changes")
      %w[runner.os runner.arch toolchains.lock scripts/checksums.txt scripts/bootstrap.sh scripts/env.sh].each do |input|
        check(key.include?(input), "#{name} tool cache lost #{input}")
      end
      check(!paths.include?('rust/target'), "#{name} tools must not include compilation output")
    end
  end
end
ci = workflows.fetch('ci').fetch('jobs')
rust = ci.fetch('rust').fetch('steps').find { |s| s.dig('with', 'path') == 'rust/target/debug' }
check(rust && rust['if'] == "needs.ci-relevance.outputs.run_rust == 'true'", 'Rust object cache must follow Rust routing')
key = rust.dig('with', 'key')
%w[toolchains.lock rust/rust-toolchain.toml rust/Cargo.lock rust/**/Cargo.toml rust/.cargo/** rust/vendor/** scripts/rust-check.sh].each do |input|
  check(key.include?(input), "Rust object cache lost #{input}")
end
check(rust.dig('with', 'restore-keys') == key.delete_suffix('${{ github.sha }}'), 'Rust objects must restore only within their build identity')
[ci.fetch('web'), workflows.fetch('security').dig('jobs', 'scan')].each do |job|
  cache = job.fetch('steps').find { |s| s.dig('with', 'path') == '.cache/npm' }
  check(cache && cache.dig('with', 'key').include?('web/**/package-lock.json'), 'npm download cache must track lockfiles')
end
check(File.read('scripts/bootstrap.sh').include?('(cd "${ROOT}/web" && npm ci)'), 'deterministic npm install must remain')
business = workflows.fetch('release-business-diagnostic').dig('jobs', 'business', 'steps')
products = YAML.safe_load(File.read('.github/workflows/release-products.yml')).dig('jobs', 'build-agent-packages', 'steps')
%w[agent-target agent-target-cache].each do |id|
  actual = business.find { |s| s['id'] == id }
  expected = products.find { |s| s['id'] == id }
  check(actual == expected, "Business #{id} must match the native release builder")
end
check(business.none? { |s| s.dig('with', 'path').to_s.lines.map(&:strip).include?('rust/target') }, 'Business must not cache all Rust targets')
targets = File.readlines('scripts/build-release-controller.sh').grep(/^\s*build_image\s/).map do |line|
  match = line.match(/^\s*build_image ([a-z_]+) ([\w.\/-]+Dockerfile)\s*$/)
  check(match, "unsupported Controller build_image call: #{line.strip}")
  match.captures
end
check(targets.length == 8 && targets.map(&:first).uniq.length == targets.length,
      'Controller builder must declare all eight distinct image targets')
probes = Hash.new { |hash, path| hash[path] = [] }
targets.each do |image, dockerfile|
  probes[dockerfile] << image
  File.foreach(dockerfile) do |line|
    check(!line.match?(/^\s*ADD\b/i), "ADD inputs need explicit Controller routing coverage: #{dockerfile}")
    next unless line.match?(/^\s*COPY\b/i)
    words = line.strip.split
    words.shift
    options = []
    options << words.shift while words.first&.start_with?('--')
    check(options.all? { |option| option.match?(/\A--(?:from=[\w.-]+|chmod=[0-7]{3,4})\z/) },
          "unsupported COPY options in #{dockerfile}: #{line.strip}")
    next if options.any? { |option| option.start_with?('--from=') }
    check(words.length >= 2 && words.all? { |word| word.match?(/\A[\w.\/-]+\z/) },
          "unsupported COPY format in #{dockerfile}: #{line.strip}")
    words[0...-1].each do |source|
      check(!source.split('/').include?('..') && (File.file?(source) || File.directory?(source)),
            "unsupported COPY source in #{dockerfile}: #{source}")
      path = File.directory?(source) ? File.join(source, 'ci-cache-input-probe.txt') : source
      probes[path] << image
    end
  end
end
router = File.expand_path('scripts/ci-relevance.sh')
Dir.mktmpdir('controller-cache-inputs-') do |tmp|
  fixture = File.join(tmp, 'repo')
  FileUtils.mkdir_p(fixture)
  run = lambda do |*command|
    output, error, status = Open3.capture3(*command, chdir: fixture)
    check(status.success?, "Controller cache routing fixture failed: #{command.join(' ')}\n#{error}")
    output.strip
  end
  run.call('git', 'init', '-q')
  run.call('git', 'config', 'user.name', 'test')
  run.call('git', 'config', 'user.email', 'test@example.invalid')
  run.call('git', 'commit', '--allow-empty', '-qm', 'base')
  base = run.call('git', 'rev-parse', 'HEAD')
  probes.each do |path, images|
    run.call('git', 'checkout', '-q', '--detach', base)
    file = File.join(fixture, path)
    FileUtils.mkdir_p(File.dirname(file))
    File.write(file, "change\n")
    run.call('git', 'add', '-f', '--', path)
    run.call('git', 'commit', '-qm', 'change')
    output = File.join(tmp, 'routing')
    File.write(output, '')
    run.call('bash', router, 'push', base, run.call('git', 'rev-parse', 'HEAD'), output, 'quick')
    routing = File.readlines(output, chomp: true).to_h { |line| line.split('=', 2) }
    selected = routing.fetch('controller_cache_images').split
    check(routing.fetch('run_controller_cache') == 'true' && (images.uniq - selected).empty?,
          "Controller cache routing lost #{images.uniq.join(' ')} for COPY/Dockerfile input #{path}: #{selected.join(' ')}")
  end
end
puts "Controller cache routing covers #{probes.length} direct COPY and Dockerfile inputs"
puts 'CI tools, Rust objects, npm downloads and Business isolated target cache contracts passed'
RUBY
