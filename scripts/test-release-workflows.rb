require 'yaml'
require 'json'
require 'tmpdir'
require 'open3'
# Release notes: owner attribution is cosmetic, and cleanup must be idempotent.
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
require_check(publish['if'] == "github.event_name == 'push'" && publish['environment'] == {'name'=>'release-publishing', 'deployment'=>false}, 'publishing must require a tag push and the publishing environment without deployment records')
require_check(publish['permissions'] == {'contents'=>'write','packages'=>'write'}, 'publishing permissions changed')
require_check(publish['needs'].sort == %w[assets build-amd64 build-arm64 prepare], 'publishing must wait for both native build/smoke legs and assets')
require_check(publish['steps'].any? {|s| s.fetch('run','').match?(/gh release create[^\n]*--generate-notes/)}, 'Release must retain native generated notes')
publish_body = publish['steps'].find {|step| step['name'] == 'Publish version images and Release assets'}.fetch('run')
require_check(publish_body.include?('scripts/strip-release-owner-attribution.py') && publish_body.include?('gh release edit "v$VERSION" --notes-file "$notes_after"'), 'release notes owner cleanup missing')
require_check(publish['steps'].first.fetch('uses', '').start_with?('actions/checkout@'), 'publication must check out the release notes filter')
filter = File.expand_path('scripts/strip-release-owner-attribution.py')
original = "## What's Changed\n* Fix A by @GentleKingson in https://github.com/GentleKingson/ocservia/pull/123\n* Fix B by @Other in https://github.com/GentleKingson/ocservia/pull/456\n**Full Changelog**: https://github.com/GentleKingson/ocservia/compare/a...b\nBy @GentleKingson: documentation\n"
expected = original.sub('Fix A by @GentleKingson in', 'Fix A in')
first, err, status = Open3.capture3('python3', filter, stdin_data: original)
require_check(status.success? && first == expected, "release note owner filter broke content: #{err}")
second, _, status = Open3.capture3('python3', filter, stdin_data: first)
require_check(status.success? && second == first, 'release note cleanup must be idempotent')

changelog = YAML.safe_load(File.read('.github/release.yml')).fetch('changelog')
require_check(changelog.dig('exclude','labels') == ['release/internal'], 'internal-only PRs must be excluded from release notes')
categories = changelog.fetch('categories')
expected_categories = {
  'Breaking changes'=>'release/breaking',
  'Security'=>'release/security',
  'Controller'=>'area/controller',
  'Agent'=>'area/agent',
  'Relay & Transport'=>'area/relay',
  'Database & Backup'=>'area/database',
  'Authentication & Authorization'=>'area/auth',
  'Web & API'=>'area/web',
  'Deployment & Operations'=>'area/deployment',
  'Dependencies'=>'area/dependencies',
  'Other changes'=>'*'
}
require_check(categories.map {|c| [c['title'], c['labels']]} == expected_categories.map {|title,label| [title,[label]]}, 'release-note categories, labels or catch-all order changed')
# Frozen exceptions from the v1.1.0 history-rewrite audit, not a second notes generator.
bridge = File.read('.github/release-notes/v1.1.0-history-rewrite.md')
bridge_prs = bridge.scan(/\(#(\d+)\)/).flatten.map(&:to_i)
expected_bridge_prs = [299,302,303,304,305,306,307,308,309,310,311,312,313,314,317,318,319,320,321,322,323,324,326,328,334,335,336,337,338,339,341,349,352,355,356,359]
require_check(bridge_prs.sort == expected_bridge_prs, 'history bridge must contain exactly the 36 omitted user-visible PRs, once each')
{'Breaking changes'=>[317,320,321,322,326,339,352], 'Security'=>[341,355,356]}.each do |heading, expected|
  section = bridge.split("### #{heading}\n", 2).last.to_s.split("\n### ", 2).first
  require_check(section.scan(/\(#(\d+)\)/).flatten.map(&:to_i).sort == expected, "history bridge lost #{heading} entries")
end
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
require_check(writer['needs'] == 'ci-relevance' && writer['if'] == "github.event_name == 'push' && github.ref == 'refs/heads/main' && needs.ci-relevance.outputs.run_controller_cache == 'true'", 'Controller cache writes must only run on trusted main pushes with Controller image inputs changed')
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
roles = %w[gateway control transport backup edge relay signer mysql_backup]
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
quick = jobs.fetch('quick')
require_check(quick['needs'] == 'main' && quick['steps'].any? {|s| s.fetch('run','').include?('bash scripts/release-quick-acceptance.sh')}, 'Quick Install acceptance must run')
result = jobs.fetch('result')
require_check(result['if'] == 'always()' && result['needs'].sort == %w[business full-ci main quick security], 'Release Check result must handle every dependency')
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
  summary = consumer['steps'].find {|s| s['name'] == 'Summarize business timings and sanitized failures'}
  require_check(summary && summary['if'] == 'always()', 'business timings must remain visible on failure')
  File.write("#{dir}/result.json", {'probe_status'=>'FAIL','last_stage'=>'single_instance_recovery','exit_code'=>1,'timings'=>[{'stage'=>'controller_image_build','seconds'=>42}],'private_detail'=>'omit-this-field'}.to_json)
  File.write("#{dir}/api-checkpoints.jsonl", {'name'=>'real_vpn_relay_recovery','time'=>'2026-01-01T00:00:00Z','status'=>'PASS','private_detail'=>'omit-this-field'}.to_json + "\n")
  File.write("#{dir}/probe.log", "unrelated log line\nRuntimeError: timeout: owner lease invalidation\n")
  output, _, status = Open3.capture3({'DIAGNOSTICS'=>dir},'bash','-euo','pipefail','-c',summary.fetch('run'))
  require_check(status.success? && output.include?('controller_image_build') && output.include?('real_vpn_relay_recovery') && output.include?('RuntimeError: timeout'), 'safe diagnostic summary missing')
  require_check(!output.include?('omit-this-field') && !output.include?('unrelated log line'), 'summary must select safe fields instead of dumping logs')
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
require_check(manual['permissions'] == {'contents'=>'read'} && manual.dig('jobs','business','uses') == './.github/workflows/release-business-diagnostic.yml', 'manual business diagnostics must use the same local checks')
require_check(manual.dig('jobs','business','if') == "inputs.purpose == 'smoke' || inputs.purpose == 'integration'", 'platform diagnostics must not run business acceptance')
require_check(manual.dig('jobs','native-platform','uses') == './.github/workflows/release-native-platform.yml' && manual.dig('jobs','native-platform','if') == "inputs.purpose == 'native-ubuntu-arm64' || inputs.purpose == 'native-debian13'", 'native diagnostics must be explicitly selected')
platform = load_workflow('release-native-platform')
require_check(platform.fetch(true).keys == ['workflow_call'] && platform['permissions'] == {'contents'=>'read'}, 'platform diagnostics must be read-only and reusable only')
require_check(platform.dig('jobs','native','timeout-minutes') == 60 && platform.dig('jobs','native','runs-on').include?('ubuntu-24.04-arm'), 'native diagnostics need bounded native runners')
puts 'Tag builds, dispatch write guard, Release Check failure propagation and actual recovery contracts passed'

require_check(!manual.dig('jobs','business','with','profile').include?('production_signer'), 'Signer must not force integration')
# Execute publication commands with recording doubles: no external writes.
Dir.mktmpdir('release-rc-') do |dir|
  prepare = release.dig('jobs','prepare','steps').first.fetch('run')
  %w[1.2.3 1.2.3-rc.1 1.2.3-rc.10].each do |version|
    [['push',"v#{version}",''],['workflow_dispatch','main',version]].each do |event,tag,input|
      File.write("#{dir}/outputs", '')
      _, error, status = Open3.capture3({'EVENT'=>event,'TAG'=>tag,'TEST_VERSION'=>input,'GITHUB_OUTPUT'=>"#{dir}/outputs"},'bash','-euo','pipefail','-c',prepare)
      require_check(status.success?, "valid RC input failed: #{error}")
      require_check(File.read("#{dir}/outputs").include?("is_prerelease=#{version.include?('-rc.') ? 'true' : 'false'}\n"), 'wrong prerelease output')
    end
  end
  %w[v1.2.3 v1.2.3-rc.1 1.2.3-rc 1.2.3-rc1 1.2.3-rc.0 1.2.3-rc.01 1.2.3-beta.1 1.2.3+build 1.2.3-rc.1+build].each do |version|
    [['push',"v#{version}",''],['workflow_dispatch','main',version]].each do |event,tag,input|
      status = Open3.capture3({'EVENT'=>event,'TAG'=>tag,'TEST_VERSION'=>input,'GITHUB_OUTPUT'=>"#{dir}/outputs"},'bash','-euo','pipefail','-c',prepare).last
      require_check(!status.success?, "invalid RC input accepted: #{version}")
    end
  end
  File.write("#{dir}/gh", <<~'SH')
    #!/usr/bin/env bash
    printf '%s\n' "$*" >>"$CALLS"
    case "$1 $2" in
      'release list') echo "${STABLE_BASE-v1.1.0}" ;;
      'release view')
        if [[ "$#" == 3 ]]; then [[ "${RELEASE_EXISTS:-false}" == true ]]
        elif [[ "$*" == *isDraft* ]]; then echo "${PREVIOUS_RC_VALID:-true}"
        elif [[ "$*" == *isPrerelease* ]]; then echo "$IS_PRERELEASE"
        elif [[ "$*" == *'--json body'* ]]; then printf '%s\n' "${NOTES_BODY:-}"
        else exit 1
        fi ;;
      'api repos/test/repo/releases/latest') echo v1.1.0 ;;
      'api repos/test/repo/contents/.github/release-notes/v1.1.0-history-rewrite.md?ref=test-publish-sha')
        [[ "$*" == *'Accept: application/vnd.github.raw+json'* && "${BRIDGE_FAILURE:-false}" != true ]] || exit 1
        cat "$BRIDGE_FILE" ;;
      'release create') printf '%s\0' "$@" >"$CREATE_ARGS" ;;
      'release edit') cp "$5" "$RUNNER_TEMP/edited-notes" ;;
      'release upload') ;;
      *) exit 2 ;;
    esac
  SH
  File.write("#{dir}/docker", "#!/usr/bin/env bash\ncat >/dev/null\n")
  File.chmod(0755,"#{dir}/gh","#{dir}/docker")
  env={'PATH'=>"#{dir}:#{ENV.fetch('PATH')}",'GITHUB_OUTPUT'=>"#{dir}/outputs",'CALLS'=>"#{dir}/calls",'GH_REPO'=>'test/repo','GH_TOKEN'=>'test','GITHUB_ACTOR'=>'test','RUNNER_TEMP'=>dir,'GITHUB_SHA'=>'test-publish-sha','BRIDGE_FILE'=>File.expand_path('.github/release-notes/v1.1.0-history-rewrite.md'),'CREATE_ARGS'=>"#{dir}/create-args"}
  notes = publish['steps'].find {|s| s['id']=='notes'}.fetch('run')
  publication = publish['steps'].find {|s| s['name']=='Publish version images and Release assets'}.fetch('run')
  [['1.2.3-rc.1','v1.1.0'],['1.2.3-rc.2','v1.2.3-rc.1'],['1.2.3-rc.10','v1.2.3-rc.9'],['1.2.3','v1.1.0'],['1.3.0-rc.1','v1.2.3'],['1.3.0','v1.2.3'],['2.0.0-rc.2','v2.0.0-rc.1'],['2.0.0-rc.3','v2.0.0-rc.2'],['2.0.0','v1.1.0']].each do |version,base|
    File.write("#{dir}/outputs",'')
    File.write("#{dir}/calls",'')
    current=env.merge('VERSION'=>version,'IS_PRERELEASE'=>version.include?('-rc.') ? 'true' : 'false','STABLE_BASE'=>version.start_with?('1.3.0') ? 'v1.2.3' : 'v1.1.0')
    _,err,status=Open3.capture3(current,'bash','-euo','pipefail','-c',notes)
    require_check(status.success? && File.read("#{dir}/outputs").include?("base=#{base}\n"), "notes base failed #{version}: #{err}")
    _,err,status=Open3.capture3(current.merge('NOTES_BASE'=>base),'bash','-euo','pipefail','-c',publication)
    require_check(status.success?, "publication failed: #{err}")
    create=File.binread("#{dir}/create-args").split("\0")
    require_check(create[create.index('--notes-start-tag')+1] == base && create.include?('--verify-tag') && create.include?('--generate-notes'), 'publication lost native notes, explicit base or tag verification')
    require_check(create.include?('--notes') == (base == 'v1.1.0' || version == '2.0.0-rc.2'), 'supplement must only apply to its selected version or history base')
    if base == 'v1.1.0'
      require_check(create[create.index('--notes')+1] == File.read(env['BRIDGE_FILE']).rstrip, 'history bridge content changed in CLI arguments')
    else
      require_check(!File.read("#{dir}/calls").include?('/contents/'), 'other bases must not fetch the history bridge')
    end
    if version == '2.0.0-rc.2'
      supplement = File.read('.github/release-notes/v2.0.0-rc.2.md').rstrip
      require_check(create[create.index('--notes')+1] == supplement, 'RC.2 supplement content changed in CLI arguments')
      combined, error, status = Open3.capture3('python3', filter, stdin_data: supplement + "\n\n" + original)
      require_check(status.success? && combined == supplement + "\n\n" + expected, "cleanup changed supplement, contributor or comparison: #{error}")
    end
    require_check(create.include?('--prerelease') == version.include?('-rc.') && create.include?('--latest=false') == version.include?('-rc.'), 'wrong publication flags')
  end
  [['1.3.0','v1.2.3'],['2.0.0-rc.2','v2.0.0-rc.1']].each do |version,base|
    [false, true].each do |exists|
      [original, expected].each do |body|
        File.write("#{dir}/calls", '')
        current = env.merge('VERSION'=>version, 'IS_PRERELEASE'=>version.include?('-rc.').to_s, 'NOTES_BASE'=>base, 'RELEASE_EXISTS'=>exists.to_s, 'NOTES_BODY'=>body)
        _, err, status = Open3.capture3(current, 'bash', '-euo', 'pipefail', '-c', publication)
        require_check(status.success?, "notes cleanup publication failed: #{err}")
        calls = File.read("#{dir}/calls")
        require_check(calls.include?('release create ') == !exists, 'reruns must preserve the existing Release')
        require_check(calls.include?('release edit ') == (body != expected), 'only matching owner entries should trigger an edit')
        require_check(File.read("#{dir}/edited-notes") == expected + "\n", 'publication changed other release notes') if body != expected
        require_check(calls.include?("release upload v#{version} "), 'notes cleanup must retain asset uploads')
      end
    end
  end
  File.write("#{dir}/calls", '')
  failed=env.merge('VERSION'=>'2.0.0-rc.2','IS_PRERELEASE'=>'true','NOTES_BASE'=>'v2.0.0-rc.1')
  Dir.chdir(dir) do
    require_check(!Open3.capture3(failed,'bash','-euo','pipefail','-c',publication).last.success?, 'missing RC.2 supplement must fail publication')
  end
  require_check(!File.read("#{dir}/calls").include?('release create '), 'missing RC.2 supplement must not create a partial Release')
  File.write("#{dir}/calls", '')
  failed=env.merge('VERSION'=>'1.2.3-rc.1','IS_PRERELEASE'=>'true','NOTES_BASE'=>'v1.1.0','BRIDGE_FAILURE'=>'true')
  require_check(!Open3.capture3(failed,'bash','-euo','pipefail','-c',publication).last.success?, 'missing history bridge must fail publication')
  require_check(!File.read("#{dir}/calls").include?('release create '), 'bridge fetch failure must not create a partial Release')
  [['1.2.3-rc.2',{'PREVIOUS_RC_VALID'=>'false'}],['1.2.3-rc.1',{'STABLE_BASE'=>''}]].each do |version,extra|
    require_check(!Open3.capture3(env.merge('VERSION'=>version).merge(extra),'bash','-euo','pipefail','-c',notes).last.success?, 'missing comparison base accepted')
  end
end
