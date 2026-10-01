require 'yaml'
require 'json'
require 'tmpdir'
require 'open3'
def load_workflow(name)
  YAML.safe_load(File.read(".github/workflows/#{name}.yml"), aliases: true)
end
def require_check(condition, message)
  abort message unless condition
end
release = load_workflow('release')
check = load_workflow('release-check')
products = load_workflow('release-products')
diagnostic = load_workflow('release-business-diagnostic')
manual = load_workflow('release-upgrade')
triggers = release.fetch(true)
require_check(triggers.keys.sort == %w[push workflow_dispatch] && triggers.dig('push', 'tags') == ['v*.*.*'], 'Release triggers changed')
require_check(triggers.dig('workflow_dispatch', 'inputs').keys == ['version'], 'dry-run must only select a test version')
require_check(release['permissions'] == {'contents'=>'read'}, 'Release must default to read-only')
publish = release.fetch('jobs').fetch('publish')
require_check(publish['if'] == "github.event_name == 'push'" && publish['environment'] == 'release-publishing', 'publishing must require a tag push and the publishing environment')
require_check(publish['permissions'] == {'contents'=>'write','packages'=>'write'}, 'publishing permissions changed')
require_check(publish['needs'].sort == %w[assets build-amd64 build-arm64 prepare], 'publishing must wait for both native build/smoke legs and assets')
require_check(publish['steps'].any? {|s| s.fetch('run','').include?('gh release upload') && s['run'].include?('--clobber')}, 'ordinary asset replacement missing')
require_check(release.fetch('jobs').values.none? {|j| ['./.github/workflows/ci.yml','./.github/workflows/security.yml','./.github/workflows/release-business-diagnostic.yml'].include?(j['uses'])}, 'formal Release must only build, image-scan, smoke and publish')
%w[amd64 arm64].each do |arch|
  caller = release.fetch('jobs').fetch("build-#{arch}")
  require_check(caller['uses'] == './.github/workflows/release-products.yml' && caller.dig('with','arch') == arch, "missing native #{arch} build")
end
require_check(products.fetch('jobs').keys.sort == %w[build-agent-packages build-controller-images], 'products must build directly')
products.fetch('jobs').each do |name,job|
  require_check(!job.key?('if'), "#{name} must build on tags and dispatch")
  require_check(job.fetch('runs-on').include?('ubuntu-24.04-arm'), "#{name} missing native arm64")
  require_check(job.fetch('steps').any? {|s| s.fetch('run','').include?(name == 'build-agent-packages' ? 'release-native-package-smoke.sh' : 'release-controller-image-smoke.sh')}, "#{name} missing real smoke")
end
controller = products.fetch('jobs').fetch('build-controller-images')
writer = load_workflow('ci').fetch('jobs').fetch('controller-cache')
require_check(writer['if'] == "github.event_name == 'push' && github.ref == 'refs/heads/main'", 'Controller cache writes must only run on trusted main pushes')
require_check(writer.dig('strategy','matrix','arch') == %w[amd64 arm64] && writer['runs-on'].include?('ubuntu-24.04-arm'), 'main cache writer must cover both native architectures')
require_check(!writer.key?('permissions') && !writer.key?('environment') && !writer.key?('continue-on-error'), 'cache writer must retain read-only authority and propagate export failures')
require_check(writer.dig('concurrency','group') == 'controller-cache-${{ matrix.arch }}' && writer.dig('concurrency','cancel-in-progress') == false, 'cache writers must be serialized per architecture')
require_check(writer['steps'].any? {|s| s['uses'] == './.github/actions/build-cache-credentials'} && writer['steps'].any? {|s| s['run'] == 'bash scripts/build-release-controller.sh --cache-only'}, 'writer must reuse the shared native builder and credential relay')
require_check(File.read('scripts/release-business-probe.sh').include?('bash "${ROOT}/scripts/build-release-controller.sh" >'), 'Business must retain the shared default restore-only path')
require_check(controller['steps'].any? {|s| s.fetch('run','').include?('bash scripts/build-release-controller.sh') && !s['run'].include?('--cache-only')}, 'Release must retain product build mode')
steps = controller.fetch('steps')
ordered = ['Build Controller images', 'Bootstrap pinned image-security tools', 'Scan exact Controller image archives', 'Smoke Controller images on the native runner', 'Upload Controller image archives']
indices = ordered.map {|name| steps.index {|s| s['name'] == name}}
require_check(indices.none?(&:nil?) && indices == indices.sort, 'exact image scan must follow build and precede smoke/upload')
require_check(!controller.key?('continue-on-error'), 'Controller scan failure must fail the product job')
indices.each do |index|
  require_check(!steps[index].key?('if') && !steps[index].key?('continue-on-error'), 'build/scan/smoke/upload must use normal success-only failure propagation')
end
scan = steps[indices[2]]
require_check(scan.dig('env','CONTROLLER_ARCH') == '${{ inputs.arch }}', 'scan must use the native product architecture')
roles = %w[gateway control transport backup edge relay signer mysql_backup mariadb_backup]
require_check(scan.fetch('run').include?("for name in #{roles.join(' ')}; do") && scan['run'].include?('$RUNNER_TEMP/controller-images/$name-linux-$CONTROLLER_ARCH.tar'), 'scan must consume every exact release archive')
require_check(scan['run'].include?('IMAGE_ARCHIVES_TSV="$scan_table" bash scripts/scan-release-images.sh'), 'existing scan failure semantics must remain mandatory')
require_check(steps[indices[1]]['run'] == 'scripts/bootstrap.sh image-security', 'pinned scanner bootstrap missing')
cache = steps.find {|s| s['name'] == 'Cache pinned image-security tools'}.fetch('with')
require_check(cache['path'].split == %w[.cache/downloads .tools], 'image-security cache must contain tools only, never vulnerability DB or results')
%w[runner.os runner.arch toolchains.lock scripts/checksums.txt scripts/bootstrap.sh scripts/env.sh].each {|part| require_check(cache['key'].include?(part), "image-security cache identity missing #{part}")}
require_check(!cache['key'].include?('github.sha'), 'stable tool cache must not depend on source SHA')
probe = File.read('scripts/release-business-probe.sh')
require_check(!probe.match?(/scan-release-images|image_security|bootstrap.sh.*image-security/), 'Business must not own the image-security gate')
require_check(check.fetch(true).keys == ['workflow_dispatch'] && check['permissions'] == {'contents'=>'read'}, 'Release Check must be manual and read-only')
jobs = check.fetch('jobs')
require_check(jobs.fetch('main').fetch('steps').first.fetch('run') == 'test "$GITHUB_REF" = refs/heads/main', 'Release Check must use main')
require_check(jobs.fetch('full-ci')['uses'] == './.github/workflows/ci.yml' && jobs.dig('full-ci','with','profile') == 'full', 'Full CI missing')
require_check(jobs.fetch('security')['uses'] == './.github/workflows/security.yml', 'Security missing')
require_check(jobs.fetch('business')['uses'] == './.github/workflows/release-business-diagnostic.yml' && jobs.dig('business','with') == {'version'=>'0.0.0','profile'=>'smoke','production_signer'=>true,'run-resilience'=>true}, 'Integrated Business Smoke and recovery must run')
result = jobs.fetch('result')
require_check(result['if'] == 'always()' && result['needs'].sort == %w[business full-ci main security], 'Release Check result must handle every dependency')
command = result.fetch('steps').first.fetch('run')
Dir.mktmpdir('release-check-') do |dir|
  results = result['needs'].to_h {|id| [id,{'result'=>'success'}]}
  env = {'GITHUB_STEP_SUMMARY'=>"#{dir}/summary"}
  execute = ->(data) { Open3.capture3(env.merge('RESULTS'=>data.to_json),'bash','-euo','pipefail','-c',command).last.success? }
  require_check(execute.call(results), 'all PASS must pass Release Check')
  results.each_key do |id|
    %w[failure cancelled skipped missing].each do |state|
      failed = Marshal.load(Marshal.dump(results))
      state == 'missing' ? failed.delete(id) : failed[id]['result'] = state
      require_check(!execute.call(failed), "Release Check accepted #{id}/#{state}")
    end
  end
  extra = results.merge('unknown'=>{'result'=>'success'})
  require_check(!execute.call(extra), 'Release Check accepted an unknown dependency')
  prepare = release.dig('jobs','prepare','steps').first.fetch('run')
  [['push','v1.2.3','',true],['workflow_dispatch','main','0.0.0',true],['push','main','1.2.3',false],['push','v1.2','',false],['workflow_dispatch','main','v1.2.3',false],['workflow_dispatch','main','1.2.3;false',false]].each do |event,tag,version,expected|
    status = Open3.capture3({'EVENT'=>event,'TAG'=>tag,'TEST_VERSION'=>version,'GITHUB_OUTPUT'=>"#{dir}/outputs"},'bash','-euo','pipefail','-c',prepare).last.success?
    require_check(status == expected, "version input validation failed for #{tag}/#{version}")
  end
  consumer = diagnostic.fetch('jobs').fetch('business')
  require_check(diagnostic['permissions'] == {'contents'=>'read'} && !consumer.key?('environment'), 'Business must run without publishing authority')
  require_check(consumer['runs-on'] == 'ubuntu-24.04' && !consumer.key?('strategy') && consumer.dig('env','CONTROLLER_ARCH') == 'amd64', 'Business must exercise only native amd64')
  gate = consumer['steps'].find {|s| s['name'] == 'Require completed business and actual recovery scenarios'}.fetch('run')
  recovery = gate[/node --input-type=module <<'JS'\n(.*?)\nJS\n?/m,1]
  require_check(recovery, 'actual recovery result gate missing')
  names = %w[controller agent database relay]
  sample = {'resilience_requested'=>true,'resilience_result'=>'PASS','resilience_scenarios'=>names.map {|n| {'name'=>"resilience_#{n}",'status'=>'PASS'}},'planned_topology'=>{'native_systemd_node'=>true}}
  run_recovery = ->(data, flags={}) {
    File.write("#{dir}/result.json",data.to_json)
    Open3.capture3({'DIAGNOSTICS'=>dir,'BUSINESS_RUN_RESILIENCE'=>'true'}.merge(flags),'node','--input-type=module','-e',recovery).last.success?
  }
  require_check(run_recovery.call(sample),'complete recoveries must pass')
  names.each do |name|
    failed = Marshal.load(Marshal.dump(sample))
    failed['resilience_scenarios'].reject! {|r| r['name']=="resilience_#{name}"}
    require_check(!run_recovery.call(failed), "missing #{name} recovery accepted")
  end
  %w[FAIL SKIPPED].each {|state| require_check(!run_recovery.call(sample.merge('resilience_result'=>state)), "#{state} recovery accepted")}
  require_check(!run_recovery.call(sample.merge('planned_topology'=>{})), 'recovery without native node accepted')
  require_check(run_recovery.call(sample.merge('resilience_requested'=>false,'resilience_result'=>'SKIPPED','resilience_scenarios'=>[]),{'BUSINESS_RUN_RESILIENCE'=>'false'}), 'unselected recovery must be skipped')
end
require_check(manual['permissions'] == {'contents'=>'read'} && manual.fetch('jobs').values.all? {|j| j['uses'] == './.github/workflows/release-business-diagnostic.yml'}, 'manual diagnostics must use the same local checks')
puts 'Tag builds, dispatch write guard, Release Check failure propagation and actual recovery contracts passed'

require_check(!manual.dig('jobs','business','with','profile').include?('production_signer'), 'Signer must not force integration')
