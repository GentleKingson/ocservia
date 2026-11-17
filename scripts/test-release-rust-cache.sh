#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."
ruby -r yaml - <<'RUBY'
steps = YAML.safe_load(File.read('.github/workflows/release-products.yml')).fetch('jobs').fetch('build-agent-packages').fetch('steps')
target = steps.find { |s| s['id'] == 'agent-target' }.fetch('run')
restore = steps.find { |s| s['id'] == 'agent-target-cache' }
save = steps.find { |s| s.fetch('uses', '').start_with?('actions/cache/save@') && s.dig('with', 'path') == '${{ steps.agent-target.outputs.path }}' }
abort 'cache must be restored and saved explicitly' unless restore.fetch('uses').start_with?('actions/cache/restore@') && save.fetch('uses').start_with?('actions/cache/save@')
abort 'cache must contain only the isolated target directory' unless
  restore.dig('with', 'path') == '${{ steps.agent-target.outputs.path }}' && save.dig('with', 'path') == restore.dig('with', 'path')
key = restore.dig('with', 'key')
%w[runner.os inputs.arch rust/agent-build.Dockerfile toolchains.lock scripts/checksums.txt scripts/bootstrap.sh scripts/env.sh scripts/build-agent-binaries.sh rust/rust-toolchain.toml rust/.cargo/** rust/**/Cargo.toml rust/Cargo.lock].each do |boundary|
  abort "cache identity lost #{boundary}" unless key.include?(boundary)
end
abort 'fallback must not cross build identities' unless restore.dig('with', 'restore-keys') == key.delete_suffix('${{ github.sha }}')
abort 'save must use the new source identity, not the restored key' unless save.dig('with', 'key') == '${{ steps.agent-target-cache.outputs.cache-primary-key }}'
abort 'save must follow successful validation' unless save['if'] == "${{ success() && steps.agent-target-cache.outputs.cache-hit != 'true' }}" && steps.index(save) > steps.index(steps.find { |s| s['id'] == 'smoke-packages' })
compile = steps.find { |s| s['id'] == 'build-packages' }
abort 'a cache hit must never skip binary compilation' if compile.key?('if')
require 'tmpdir'
require 'fileutils'
require 'open3'
require 'digest'
# Run the native builder and its complete container command with fake external
# tools. The compilation/ABI/version checks stay in the production script.
Dir.mktmpdir('native-builder-') do |tmp|
  FileUtils.mkdir_p("#{tmp}/scripts")
  %w[build-agent-binaries.sh env.sh].each { |name| FileUtils.cp("scripts/#{name}", "#{tmp}/scripts/") }
  FileUtils.mkdir_p("#{tmp}/rust")
  FileUtils.cp('rust/agent-build.Dockerfile', "#{tmp}/rust/")
  write_tool = lambda do |path, body|
    FileUtils.mkdir_p(File.dirname(path))
    File.write(path, "#!/usr/bin/env bash\nset -euo pipefail\n#{body}\n")
    File.chmod(0755, path)
  end
  write_tool.call("#{tmp}/scripts/bootstrap.sh", '[[ "$*" == native-packages ]]')
  write_tool.call("#{tmp}/.tools/bin/uname", 'echo "${FIXTURE_HOST}"')
  write_tool.call("#{tmp}/.tools/bin/getconf", 'echo "${FIXTURE_LIBC:-glibc 2.34}"')
  %w[rpm rustc file].each { |name| write_tool.call("#{tmp}/.tools/bin/#{name}", 'exit 0') }
  write_tool.call("#{tmp}/.tools/bin/readelf", 'echo "${FIXTURE_ELF:-$ELF_MACHINE}"')
  write_tool.call("#{tmp}/.tools/bin/cargo", <<~'SH')
    printf '%s|%s\n' "$CARGO_TARGET_DIR" "$*" >>"$FIXTURE_COMPILE_LOG"
    [[ "${FIXTURE_COMPILE_FAIL:-false}" == false ]] || exit 19
    mkdir -p "$CARGO_TARGET_DIR/release"
    for binary in ocservia-agent ocservia-privd ocservia-upgrader; do
      printf '#!/bin/sh\necho "%s %s"\n' "$binary" "${FIXTURE_BINARY_VERSION:-$VERSION}" >"$CARGO_TARGET_DIR/release/$binary"
      chmod 755 "$CARGO_TARGET_DIR/release/$binary"
    done
  SH
  write_tool.call("#{tmp}/.tools/bin/docker", <<~'SH')
    case "$1" in
      info) echo "${FIXTURE_DAEMON:-$PACKAGE_ARCH}" ;;
      image) echo "$PACKAGE_ARCH" ;;
      build) cat >/dev/null ;;
      run)
        shift
        while (($#)); do
          case "$1" in
            --rm) shift ;;
            --platform|--user|--volume|--workdir|--security-opt) shift 2 ;;
            --cap-drop=*|--security-opt=*) shift ;;
            --env)
              # Docker's task HOME value is intentionally scoped to its child.
              if [[ "$2" == *=* ]]; then export "$2"; fi
              shift 2 ;;
            *) shift; exec "$@" ;;
          esac
        done
        exit 2 ;;
      *) exit 2 ;;
    esac
  SH
  env = {'PATH' => "#{tmp}/.tools/bin:#{ENV.fetch('PATH')}", 'VERSION' => '1.2.3',
         'FIXTURE_COMPILE_LOG' => "#{tmp}/compile.log"}
  %w[amd64 arm64].each do |arch|
    native_env = env.merge('PACKAGE_ARCH' => arch, 'FIXTURE_HOST' => arch == 'amd64' ? 'x86_64' : 'aarch64')
    execute = ->(extra = {}) { Open3.capture2e(native_env.merge(extra), 'bash', "#{tmp}/scripts/build-agent-binaries.sh", chdir: tmp) }
    File.write("#{tmp}/compile.log", '')
    output, status = execute.call
    abort "native builder failed: #{output}" unless status.success?
    cache = "rust/target/agent-#{arch}-#{Digest::SHA256.file('rust/agent-build.Dockerfile').hexdigest[0, 16]}"
    calls = File.readlines("#{tmp}/compile.log", chomp: true)
    abort "wrong native compilation identity: #{calls}" unless calls == ["#{tmp}/#{cache}|build --locked --release --package ocservia-agent --package ocservia-privd --package ocservia-upgrader"]
    # The workflow's complete target resolution must identify that same path.
    command = target.gsub('${{ inputs.arch }}', arch)
    output, status = Open3.capture2e({'GITHUB_OUTPUT' => "#{tmp}/outputs"}, 'bash', '-euo', 'pipefail', '-c', command)
    abort "workflow cache target failed: #{output}" unless status.success? && File.readlines("#{tmp}/outputs", chomp: true).last == "path=#{cache}"
    %w[ocservia-agent ocservia-privd ocservia-upgrader].each do |binary|
      output, status = Open3.capture2e("#{tmp}/rust/target/release/#{binary}", '--version')
      abort 'builder did not publish verified binaries' unless status.success? && output.strip == "#{binary} 1.2.3"
    end
    [{'FIXTURE_LIBC' => 'glibc 2.35'}, {'FIXTURE_ELF' => 'foreign machine'},
     {'FIXTURE_BINARY_VERSION' => '0.0.0'}, {'FIXTURE_COMPILE_FAIL' => 'true'},
     {'FIXTURE_DAEMON' => 'foreign'}, {'FIXTURE_HOST' => 'foreign'}].each do |failure|
      FileUtils.rm_rf("#{tmp}/rust/target/release")
      output, status = execute.call(failure)
      abort "native validation accepted #{failure}: #{output}" if status.success?
      abort 'failed native validation published binaries' if File.exist?("#{tmp}/rust/target/release")
    end
  end
end
# Dockerfile checks describe stage inputs and command arguments, independent of
# whitespace, comments, continuations, or adjacency between instructions.
require 'shellwords'
def docker_instructions(path)
  File.read(path).gsub(/\\\r?\n/, ' ').lines.filter_map do |line|
    line = line.strip
    next if line.empty? || line.start_with?('#')
    instruction, arguments = line.split(/\s+/, 2)
    [instruction.upcase, Shellwords.split(arguments)]
  end
end
builder_runs = docker_instructions('rust/agent-build.Dockerfile').select { |op, _| op == 'RUN' }.flat_map(&:last)
%w[minrate=512k timeout=15 max_parallel_downloads=10].each do |option|
  abort "DNF slow-mirror bound missing #{option}" unless builder_runs.include?("--setopt=#{option}")
end
abort 'keep official mirror selection, retry defaults and package verification' if
  builder_runs.any? { |arg| arg.match?(/\A(?:--setopt=)?(?:retries=|fastestmirror=|mirrorlist=|baseurl=|gpgcheck=0|sslverify=0)|\A--nogpgcheck\z/) }
stages = {}
current = nil
docker_instructions('rust/transportd.Dockerfile').each do |instruction|
  op, args = instruction
  if op == 'FROM'
    current = args[-2]&.casecmp?('AS') ? args.last.downcase : "anonymous-#{stages.length}"
    stages[current] = []
  else
    stages.fetch(current) << instruction
  end
end
planner = stages.fetch('planner')
copy = planner.index(['COPY', %w[rust/crates ./crates]])
prepare = planner.index(['RUN', %w[cargo chef prepare --recipe-path recipe.json]])
abort 'recipe must include the complete workspace' unless copy && prepare && copy < prepare
%w[dependencies build].each do |stage|
  inputs = stages.fetch(stage)
  %w[.cargo vendor].each do |source|
    abort "#{stage} lost patched source/configuration inputs" unless inputs.include?(['COPY', ["rust/#{source}", "./#{source}"]])
  end
end
abort 'real sources must replace the recipe skeleton' unless stages.fetch('build').include?(['COPY', %w[rust/crates ./crates]])
{'dependencies' => %w[cargo chef cook], 'build' => %w[cargo build]}.each do |stage, command|
  run = stages.fetch(stage).find { |op, args| op == 'RUN' && args.first(command.length) == command }&.last
  abort "#{stage} lost the locked release package" unless run &&
    %w[--locked --release].all? { |flag| run.include?(flag) } && run.each_cons(2).include?(%w[--package ocservia-transportd])
  abort 'cook must use the planner recipe' if stage == 'dependencies' && !run.each_cons(2).include?(%w[--recipe-path recipe.json])
end
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
business = workflows.fetch('release-business-diagnostic').dig('jobs', 'business', 'steps')
products = YAML.safe_load(File.read('.github/workflows/release-products.yml')).dig('jobs', 'build-agent-packages', 'steps')
%w[agent-target agent-target-cache].each do |id|
  actual = business.find { |s| s['id'] == id }
  expected = products.find { |s| s['id'] == id }
  check(actual.reject { |key, _| key == 'name' } == expected.reject { |key, _| key == 'name' }, "Business #{id} must match the native release builder")
end
check(business.none? { |s| s.dig('with', 'path').to_s.lines.map(&:strip).include?('rust/target') }, 'Business must not cache all Rust targets')
targets = Dir.mktmpdir('controller-build-dispatch-') do |dir|
  File.write("#{dir}/docker", <<~'SH')
    #!/usr/bin/env bash
    set -euo pipefail
    [[ "$1 $2" != 'buildx create' ]] || exit 0
    [[ "$1 $2" == 'buildx build' ]] || exit 2
    printf '%s\0' "$@" >>"$DOCKER_TRACE"
    printf '\n' >>"$DOCKER_TRACE"
    for arg in "$@"; do
      if [[ "$arg" == type=docker,dest=* ]]; then printf 'fixture image\n' >"${arg#type=docker,dest=}"; fi
    done
  SH
  File.write("#{dir}/uname", "#!/bin/sh\necho x86_64\n")
  File.chmod(0755, "#{dir}/docker", "#{dir}/uname")
  env = {'PATH' => "#{dir}:#{ENV.fetch('PATH')}", 'DOCKER_TRACE' => "#{dir}/trace",
         'VERSION' => '1.2.3', 'SOURCE_COMMIT' => 'a' * 40, 'CONTROLLER_ARCH' => 'amd64',
         'BUILDX_BUILDER' => 'fixture', 'BUILD_CACHE_AVAILABLE' => 'false', 'OUTPUT_DIR' => "#{dir}/images"}
  output, error, status = Open3.capture3(env, 'bash', 'scripts/build-release-controller.sh')
  check(status.success?, "Controller build dispatch failed: #{output}#{error}")
  File.readlines("#{dir}/trace", chomp: true).map do |line|
    args = line.split("\0")
    image = args.fetch(args.index('--tag') + 1).delete_prefix('ghcr.io/gentlekingson/ocservia/').delete_suffix(':1.2.3-linux-amd64')
    dockerfile = args.fetch(args.index('--file') + 1).delete_prefix(Dir.pwd + '/')
    check(args.include?("type=docker,dest=#{dir}/images/#{image}-linux-amd64.tar"), 'Controller archive identity drifted')
    [image, dockerfile]
  end
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
