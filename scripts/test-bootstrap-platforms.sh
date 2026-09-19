#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck source=scripts/go-test-environment.sh
source "${ROOT}/scripts/go-test-environment.sh"
require_test_commands ruby tar gzip

# These disposable command fixtures test routing and failures, not native Go.
ruby - "${ROOT}" <<'RUBY'
require 'tmpdir'
require 'fileutils'
require 'open3'
require 'digest'
root = ARGV.fetch(0)
version = File.read("#{root}/toolchains.lock")[/^go=(.+)$/, 1]
checksums = File.read("#{root}/scripts/checksums.txt")
bash = ENV.fetch('PATH').split(':').map { |dir| "#{dir}/bash" }.find { |path| File.executable?(path) }

def script(path, body)
  FileUtils.mkdir_p(File.dirname(path))
  File.write(path, "#!/usr/bin/env bash\nset -eu\n#{body}\n")
  File.chmod(0755, path)
end

def run(env, bash, path, *args, error: nil)
  output, status = Open3.capture2e(env, bash, path, *args)
  if error
    raise "expected #{error.inspect}: #{status}\n#{output}" if status.success? || !output.include?(error)
    raise "unbound variable diagnostic: #{output}" if output.include?('unbound variable')
  else
    raise "unexpected failure: #{status}\n#{output}" unless status.success?
  end
  output
end

Dir.mktmpdir('bootstrap-platforms-') do |tmp|
  %w[Linux-aarch64 Linux-x86_64 Darwin-arm64].each do |platform|
    target = {'Linux-aarch64' => 'linux-arm64', 'Linux-x86_64' => 'linux-amd64', 'Darwin-arm64' => 'darwin-arm64'}.fetch(platform)
    os, arch = target.split('-')
    artifact = "go#{version}.#{target}.tar.gz"
    raise "missing locked checksum for #{artifact}" unless checksums.match?(/^[a-f0-9]{64}  #{Regexp.escape(artifact)}$/)
    work = "#{tmp}/#{platform}"
    FileUtils.mkdir_p("#{work}/scripts")
    %w[bootstrap.sh env.sh].each { |name| FileUtils.cp("#{root}/scripts/#{name}", "#{work}/scripts/") }
    FileUtils.cp("#{root}/toolchains.lock", work)
    payload = "#{work}/payload"
    script("#{payload}/go/bin/go", <<~SH)
      echo executed >>"${FIXTURE_EXECUTIONS}"
      [[ "$*" == 'env GOVERSION GOHOSTOS GOHOSTARCH' ]]
      printf 'go#{version}\\n#{os}\\n#{arch}\\n'
    SH
    archive = "#{work}/fixture.tar.gz"
    _, status = Open3.capture2e('tar', '-czf', archive, '-C', payload, 'go')
    raise 'fixture archive failed' unless status.success?
    digest = Digest::SHA256.file(archive).hexdigest
    manifest = "#{work}/scripts/checksums.txt"
    File.write(manifest, "#{digest}  #{artifact}\n")
    bin = "#{work}/bin"
    script("#{bin}/uname", "case \"$1\" in -s) echo #{platform.split('-').first} ;; -m) echo #{platform.split('-').last} ;; esac")
    script("#{bin}/jq", 'exit 0')
    script("#{bin}/curl", <<~SH)
      printf '%s\\n' "$*" >>"${FIXTURE_DOWNLOADS}"
      while (($#)); do
        if [[ "$1" == --output ]]; then cp "${FIXTURE_ARCHIVE}" "$2"; exit; fi
        shift
      done
      exit 1
    SH
    env = {'PATH' => "#{bin}:#{ENV.fetch('PATH')}", 'FIXTURE_ARCHIVE' => archive,
           'FIXTURE_DOWNLOADS' => "#{work}/downloads", 'FIXTURE_EXECUTIONS' => "#{work}/executions",
           'GOOS' => 'windows', 'GOARCH' => '386', 'GOTOOLCHAIN' => 'auto'}
    bootstrap = "#{work}/scripts/bootstrap.sh"
    run(env, bash, bootstrap, 'go-test')
    raise 'wrong URL or artifact' unless File.read("#{work}/downloads").include?("https://go.dev/dl/#{artifact}")
    raise 'go-test installed extra tools' unless Dir.children("#{work}/.tools").sort == %w[bin go]
    installed = "#{work}/.tools/go/bin/go"
    before = File.stat(installed)
    FileUtils.mkdir_p("#{work}/.tools/node")
    File.write("#{work}/.tools/node/keep", 'untouched')
    run(env, bash, bootstrap, 'go-test')
    raise 'valid Go reinstalled' unless File.stat(installed).ino == before.ino && File.stat(installed).mtime == before.mtime
    raise 'unnecessary download' unless File.readlines("#{work}/downloads").length == 1
    raise 'other tool changed' unless File.read("#{work}/.tools/node/keep") == 'untouched'

    next unless platform == 'Linux-aarch64'
    # Same version but wrong host architecture must not satisfy reuse. GOARCH
    # above deliberately differs from GOHOSTARCH, as in cross-compilation.
    File.write(installed, File.read(installed).sub('arm64', 'amd64'))
    run(env, bash, bootstrap, 'go-test')
    raise 'wrong host reused' unless File.read(installed).include?('arm64')
    File.chmod(0644, installed)
    run(env, bash, bootstrap, 'go-test')
    raise 'non-executable Go reused' unless File.executable?(installed)

    # Keep a usable old version while every replacement failure is exercised.
    old = File.read(installed).sub("go#{version}", 'go0.0.0')
    File.write(installed, old)
    cache = "#{work}/.cache/downloads/#{artifact}"
    File.write(manifest, '')
    run(env, bash, bootstrap, 'go-test', error: "missing or invalid SHA-256 checksum for #{artifact}")
    File.write(manifest, "#{'0' * 64}  #{artifact}\n")
    run(env, bash, bootstrap, 'go-test', error: 'checksum mismatch')
    File.write(manifest, "#{digest}  #{artifact}\n")
    File.write(cache, 'corrupt cached archive')
    run(env, bash, bootstrap, 'go-test', error: 'checksum mismatch')
    raise 'cached failure downloaded a replacement' unless File.readlines("#{work}/downloads").length == 1
    File.delete(cache)
    File.write(manifest, "#{'0' * 64}  #{artifact}\n")
    run(env, bash, bootstrap, 'go-test', error: 'checksum mismatch')
    raise 'unverified download promoted to cache' if File.exist?(cache)
    raise 'failed verification overwrote old Go' unless File.read(installed) == old

    # A checksum-valid but wrong/unexecutable payload is never promoted either.
    %w[wrong-host non-executable].each do |failure|
      File.write("#{payload}/go/bin/go", File.read(installed).sub('go0.0.0', "go#{version}").sub('arm64', 'amd64'))
      File.chmod(failure == 'non-executable' ? 0644 : 0755, "#{payload}/go/bin/go")
      _, status = Open3.capture2e('tar', '-czf', archive, '-C', payload, 'go')
      raise 'fixture archive failed' unless status.success?
      FileUtils.rm_f(cache)
      File.write(manifest, "#{Digest::SHA256.file(archive).hexdigest}  #{artifact}\n")
      run(env, bash, bootstrap, 'go-test', error: 'invalid Go toolchain')
      raise 'invalid toolchain replaced old Go' unless File.read(installed) == old
      raise 'staging directory leaked' unless Dir.glob("#{work}/.tools/.go-install-*").empty?
    end

    FileUtils.rm_rf("#{work}/.tools")
    FileUtils.rm_rf("#{work}/.cache")
    %w[all ci-quality contracts g6-runtime g6-secret-scan go-quality go-rust-integration native rust-validation web security].each do |profile|
      run(env, bash, bootstrap, profile, error: "#{platform}/#{profile}; artifact mappings")
      raise 'unsupported profile partially installed' if File.exist?("#{work}/.tools") || File.exist?("#{work}/.cache")
    end
    # Already-supported rustup/nfpm paths remain usable without adding new maps.
    script("#{work}/.tools/cargo/bin/rustc", "echo 'rustc #{File.read("#{root}/toolchains.lock")[/^rust=(.+)$/, 1]} (fixture)'")
    %w[cargo rustup rustfmt].each { |name| script("#{work}/.tools/cargo/bin/#{name}", 'exit 0') }
    script("#{work}/.tools/bin/nfpm", "echo 'GitVersion: #{File.read("#{root}/toolchains.lock")[/^nfpm=(.+)$/, 1]}'")
    run(env, bash, bootstrap, 'rust-basic')
    run(env, bash, bootstrap, 'native-packages')
    script("#{bin}/uname", 'echo unsupported')
    run(env, bash, bootstrap, 'go-test', error: 'unsupported-unsupported/go-test; no artifact mappings')
  end

  # Minimal PATHs prove each real entrypoint fails before tests or containers.
  work = "#{tmp}/missing-tools"
  FileUtils.mkdir_p("#{work}/scripts")
  %w[env.sh go-test-environment.sh go-check.sh required-go-tests.sh test-required-go-tests.sh test-bootstrap-profiles.sh database-integration.sh database-foundation-integration.sh].each do |name|
    FileUtils.cp("#{root}/scripts/#{name}", "#{work}/scripts/")
  end
  bin = "#{work}/bin"
  FileUtils.mkdir_p(bin)
  %w[bash dirname mktemp rm].each do |name|
    path = ENV.fetch('PATH').split(':').map { |dir| "#{dir}/#{name}" }.find { |p| File.executable?(p) }
    FileUtils.ln_s(path, "#{bin}/#{name}")
  end
  script("#{bin}/go", <<~SH)
    case "$*" in
      'env CGO_ENABLED') echo "${FIXTURE_CGO:-1}" ;;
      'env CC') echo "${FIXTURE_CC:-cc}" ;;
      *) echo 'expensive Go invocation reached' >>"${FIXTURE_LAUNCHED}"; exit 99 ;;
    esac
  SH
  %w[gofmt jq setsid tee ruby python3 openssl curl sha256sum patch timeout].each { |name| script("#{bin}/#{name}", 'exit 0') }
  script("#{bin}/docker", '[[ "${FIXTURE_DOCKER:-0}" == 0 ]]')
  script("#{bin}/cc", 'exit 1')
  env = {'PATH' => bin, 'FIXTURE_LAUNCHED' => "#{work}/launched", 'DATABASE_TEST_SCOPE' => 'regression', 'ENGINE' => 'mysql', 'DATABASE_FULL_PART' => nil}
  [ ['go-check.sh', 'standard', 'go'], ['go-check.sh', 'standard', 'gofmt'],
    ['required-go-tests.sh', 'unit', 'setsid'], ['test-required-go-tests.sh', nil, 'ruby'],
    ['test-bootstrap-profiles.sh', nil, 'ruby'], ['database-integration.sh', nil, 'ruby'],
    ['database-integration.sh', nil, 'python3'], ['database-foundation-integration.sh', nil, 'openssl'],
    ['database-foundation-integration.sh', nil, 'docker'] ].each do |name, argument, missing|
    File.rename("#{bin}/#{missing}", "#{bin}/hidden-#{missing}")
    run(env, bash, "#{work}/scripts/#{name}", *[argument].compact, error: "required test command is missing: #{missing}")
    File.rename("#{bin}/hidden-#{missing}", "#{bin}/#{missing}")
  end
  run(env.merge('FIXTURE_CGO' => '0'), bash, "#{work}/scripts/go-check.sh", 'race', error: '-race requires CGO_ENABLED=1')
  run(env.merge('FIXTURE_CC' => 'missing-cc'), bash, "#{work}/scripts/go-check.sh", 'race', error: 'required test command is missing: missing-cc')
  run(env, bash, "#{work}/scripts/go-check.sh", 'race', error: 'C compiler that can compile and link')
  run(env.merge('FIXTURE_DOCKER' => '1'), bash, "#{work}/scripts/database-foundation-integration.sh", error: 'access to a running Docker daemon')
  # Full PostgreSQL needs sha256sum, not the unused shasum fallback. This PATH
  # has no shasum; stop at the injected Docker error after command preflight.
  %w[18 all].each do |major|
    run(env.merge('DATABASE_TEST_SCOPE' => 'full', 'PG_MAJOR' => major, 'FIXTURE_DOCKER' => '1'),
        bash, "#{work}/scripts/database-integration.sh", error: 'access to a running Docker daemon')
  end
  raise 'preflight started Go tests' if File.exist?("#{work}/launched")
end
puts 'Simulated bootstrap platforms, reuse, verification failures and entrypoint preflights passed (not native execution)'
RUBY
