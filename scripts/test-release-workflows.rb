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
require_check(release.fetch('jobs').values.none? {|j| ['./.github/workflows/ci.yml','./.github/workflows/security.yml','./.github/workflows/release-business-diagnostic.yml'].include?(j['uses'])}, 'formal Release must only build, smoke and publish')
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
require_check(check.fetch(true).keys == ['workflow_dispatch'] && check['permissions'] == {'contents'=>'read'}, 'Release Check must be manual and read-only')
jobs = check.fetch('jobs')
require_check(jobs.fetch('main').fetch('steps').first.fetch('run') == 'test "$GITHUB_REF" = refs/heads/main', 'Release Check must use main')
require_check(jobs.fetch('full-ci')['uses'] == './.github/workflows/ci.yml' && jobs.dig('full-ci','with','profile') == 'full', 'Full CI missing')
require_check(jobs.fetch('security')['uses'] == './.github/workflows/security.yml', 'Security missing')
require_check(jobs.fetch('business')['uses'] == './.github/workflows/release-business-diagnostic.yml' && jobs.dig('business','with') == {'version'=>'0.0.0','profile'=>'extended','production_signer'=>true,'run-resilience'=>true}, 'Integrated Business and Resilience must run')
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
  require_check(consumer.dig('strategy','matrix','arch').include?('["amd64","arm64"]'), 'Integrated must exercise both native architectures')
  gate = consumer['steps'].find {|s| s['name'] == 'Require completed business and actual recovery scenarios'}.fetch('run')
  recovery = gate[/node --input-type=module <<'JS'\n(.*?)\nJS\n?/m,1]
  require_check(recovery, 'actual recovery result gate missing')
  names = %w[controller agent_privd transport database_api relay complete]
  sample = {'resilience_requested'=>true,'resilience_result'=>'PASS','resilience_scenarios'=>names.map {|n| {'name'=>"resilience_#{n}",'status'=>'PASS'}},'planned_topology'=>{'native_systemd_node'=>true}}
  run_recovery = ->(data, flags={}) {
    File.write("#{dir}/result.json",data.to_json)
    Open3.capture3({'DIAGNOSTICS'=>dir,'BUSINESS_RUN_RESILIENCE'=>'true','INTEGRATED_INSTALL_ONLY'=>'false'}.merge(flags),'node','--input-type=module','-e',recovery).last.success?
  }
  require_check(run_recovery.call(sample),'complete recoveries must pass')
  names.each do |name|
    failed = Marshal.load(Marshal.dump(sample))
    failed['resilience_scenarios'].reject! {|r| r['name']=="resilience_#{name}"}
    require_check(!run_recovery.call(failed), "missing #{name} recovery accepted")
  end
  %w[FAIL SKIPPED].each {|state| require_check(!run_recovery.call(sample.merge('resilience_result'=>state)), "#{state} recovery accepted")}
  require_check(!run_recovery.call(sample.merge('planned_topology'=>{})), 'recovery without native node accepted')
  arm = sample.merge('resilience_result'=>'SKIPPED','resilience_scenarios'=>[])
  require_check(run_recovery.call(arm,{'INTEGRATED_INSTALL_ONLY'=>'true'}), 'arm64 install-only scope must remain distinct')
end
require_check(manual['permissions'] == {'contents'=>'read'} && manual.fetch('jobs').values.all? {|j| j['uses'] == './.github/workflows/release-business-diagnostic.yml'}, 'manual diagnostics must use the same local checks')
puts 'Tag builds, dispatch write guard, Release Check failure propagation and actual recovery contracts passed'
