#!/usr/bin/env python3
"""Static quality, supply-chain and coverage guards for GitHub Actions workflows."""
import json
from pathlib import Path
import re
import shutil
import subprocess
import sys

ROOT = Path(__file__).resolve().parent.parent
SHA_PIN = re.compile(r'^[^/]+/[^@]+@[0-9a-f]{40}$')
DIGEST_PIN = re.compile(r'@sha256:[0-9a-f]{64}$')


def load_yaml(path):
    raw = path.read_text(encoding='utf-8')
    try:
        import yaml  # type: ignore[import-not-found]
        return yaml.safe_load(raw)
    except ModuleNotFoundError:
        pass
    for cmd in (
        ['/usr/bin/python3', '-c', 'import json, sys, yaml; print(json.dumps(yaml.safe_load(sys.stdin.read())))'],
        ['ruby', '-ryaml', '-rjson', '-e', 'puts JSON.generate(YAML.safe_load(STDIN.read, aliases: true))'],
    ):
        if shutil.which(cmd[0]) or Path(cmd[0]).is_file():
            result = subprocess.run(cmd, input=raw, text=True, capture_output=True)
            if result.returncode == 0:
                return json.loads(result.stdout)
    raise RuntimeError(f'No YAML parser available to validate {path}')


def bootstrap_profiles():
    script = (ROOT / 'scripts/bootstrap.sh').read_text(encoding='utf-8')
    match = re.search(r'^case "\$\{PROFILE\}" in\n\s+([^\n]+)\)\s*;;', script, re.MULTILINE)
    if not match:
        raise RuntimeError('Unable to extract supported profiles from scripts/bootstrap.sh')
    return {item.strip() for item in match.group(1).split('|') if item.strip()}


def check_step(errors, label, step, valid_profiles):
    uses = step.get('uses')
    with_map = step.get('with') or {}
    if isinstance(uses, str):
        if not uses.startswith('./'):
            if not SHA_PIN.match(uses):
                errors.append(f'{label}: third-party action is not pinned to a 40-char commit SHA: {uses}')
        if uses.startswith('actions/checkout@'):
            if with_map.get('persist-credentials') is not False:
                errors.append(f'{label}: actions/checkout must explicitly set persist-credentials: false')
        if uses == './.github/actions/setup-tools':
            profiles = str(with_map.get('profiles', '')).strip()
            if profiles and '${{' not in profiles:
                for profile in profiles.split():
                    if profile not in valid_profiles:
                        errors.append(f'{label}: unknown setup-tools profile "{profile}"')
    run_text = step.get('run')
    if isinstance(run_text, str):
        normalized_run = re.sub(r'\\\n\s*', ' ', run_text)
        for line in normalized_run.splitlines():
            stripped = line.strip()
            if stripped.startswith('#'):
                continue
            for match in re.finditer(r'(?:^|\s)docker\s+run\b([^\n]*)', stripped):
                tokens = match.group(1).split()
                for token in tokens:
                    cleaned = token.strip('"\'')
                    if ':' not in cleaned or cleaned.startswith('-') or '=' in cleaned or '/' in cleaned.split(':', 1)[0] and cleaned.startswith('/'):
                        continue
                    if cleaned.startswith(('127.0.0.1:', '${', '$', '/')):
                        continue
                    if cleaned.startswith('mcr.microsoft.com/playwright:v${pw_version}-noble'):
                        continue
                    if re.match(r'^[a-z0-9]+(?:[._/-][a-z0-9]+)*:[A-Za-z0-9._-]+(?:@sha256:[0-9a-f]{64})?$', cleaned):
                        if not DIGEST_PIN.search(cleaned):
                            errors.append(f'{label}: docker run image must be digest-pinned: {cleaned}')


def check_workflows(errors, valid_profiles):
    workflows_dir = ROOT / '.github/workflows'
    for wf_path in sorted(workflows_dir.glob('*.yml')):
        doc = load_yaml(wf_path)
        if not isinstance(doc, dict):
            errors.append(f'{wf_path.name}: top-level YAML document must be a mapping')
            continue
        perms = doc.get('permissions')
        if perms != {'contents': 'read'}:
            errors.append(f'{wf_path.name}: top-level permissions must be strictly {{contents: read}}, got {perms!r}')
        jobs = doc.get('jobs')
        if not isinstance(jobs, dict) or not jobs:
            errors.append(f'{wf_path.name}: missing jobs mapping')
            continue
        for job_id, job in jobs.items():
            if not isinstance(job, dict):
                errors.append(f'{wf_path.name}:{job_id}: job must be a mapping')
                continue
            if 'uses' in job:
                uses = str(job['uses'])
                if not uses.startswith('./') and not SHA_PIN.match(uses):
                    errors.append(f'{wf_path.name}:{job_id}: reusable workflow must be SHA-pinned: {uses}')
                continue
            if 'timeout-minutes' not in job:
                errors.append(f'{wf_path.name}:{job_id}: job must declare timeout-minutes')
            for idx, step in enumerate(job.get('steps') or [], 1):
                if isinstance(step, dict):
                    step_name = step.get('name') or step.get('id') or step.get('uses') or f'step-{idx}'
                    check_step(errors, f'{wf_path.name}:{job_id}:{step_name}', step, valid_profiles)


def check_actions(errors, valid_profiles):
    actions_dir = ROOT / '.github/actions'
    for action_path in sorted(actions_dir.glob('*/action.yml')):
        doc = load_yaml(action_path)
        if not isinstance(doc, dict):
            errors.append(f'{action_path}: top-level YAML document must be a mapping')
            continue
        runs = doc.get('runs') or {}
        for idx, step in enumerate(runs.get('steps') or [], 1):
            if isinstance(step, dict):
                step_name = step.get('name') or step.get('id') or step.get('uses') or f'step-{idx}'
                check_step(errors, f'{action_path.parent.name}:{step_name}', step, valid_profiles)


def check_web_e2e_coverage(errors):
    e2e_specs = {p.name for p in (ROOT / 'web/e2e').glob('*.spec.ts')}
    runner_src = (ROOT / 'web/test/run-auth-browser.mjs').read_text(encoding='utf-8')
    ci_src = (ROOT / '.github/workflows/ci.yml').read_text(encoding='utf-8')
    covered = set(re.findall(r'["\']([a-z0-9-]+\.spec\.ts)["\']', runner_src))
    specs_match = re.search(r'specs=\(([^)]+)\)', ci_src)
    if specs_match:
        covered.update(specs_match.group(1).split())
    missing = sorted(e2e_specs - covered)
    if missing:
        errors.append(f'web/e2e specs missing from ci.yml web-browser coverage: {", ".join(missing)}')


def check_specific_contracts(errors):
    builder = (ROOT / '.github/workflows/release-agent.yml').read_text(encoding='utf-8')
    if "hashFiles('rust/Cargo.lock')" not in builder:
        errors.append('release-agent.yml: native-packages setup-tools must pass a rust/Cargo.lock key-suffix')
    for workflow in ('release-agent.yml', 'release-controller.yml'):
        if "github.event_name == 'push' && github.ref_name" in (ROOT / '.github/workflows' / workflow).read_text(encoding='utf-8'):
            errors.append(f'{workflow}: reusable workflow must not couple to caller github.event_name/ref_name')

    security_doc = load_yaml(ROOT / '.github/workflows/security.yml')
    includes = security_doc['jobs']['scan']['strategy']['matrix']['include']
    for item in includes:
        if item.get('profile') == 'npm-security' and item.get('cache_npm') is True:
            errors.append('security.yml: npm-security audit must not enable cache-npm')
        if item.get('name') == 'Go & Web licenses' and item.get('cache_npm') is True:
            errors.append('security.yml: Go & Web licenses must not enable cache-npm')
        if item.get('profile') == 'rust-security':
            cmd_lines = [line.strip() for line in str(item.get('command', '')).splitlines()]
            if any(line == 'cargo audit' for line in cmd_lines):
                errors.append('security.yml: rust-security must not run duplicate bare cargo audit alongside cargo deny')

    ci_doc = load_yaml(ROOT / '.github/workflows/ci.yml')
    for wf_name, doc in (('ci.yml', ci_doc), ('security.yml', security_doc)):
        on_triggers = doc.get('on') or doc.get(True) or {}
        if isinstance(on_triggers, dict) and 'workflow_call' in on_triggers:
            errors.append(f'{wf_name}: unused workflow_call trigger must stay removed')

    recovery_steps = ci_doc['jobs']['database-recovery-full'].get('steps') or []
    for step in recovery_steps:
        if isinstance(step, dict) and step.get('uses') == './.github/actions/setup-tools':
            errors.append('ci.yml: database-recovery-full must not invoke setup-tools')

    controller_wf = (ROOT / '.github/workflows/release-controller.yml').read_text(encoding='utf-8')
    for flag in ('scan-release-images.sh --update-db-only', 'scan-release-images.sh --use-prefetched-db'):
        if flag not in controller_wf:
            errors.append(f'release-controller.yml: must invoke {flag}')


def check_required_go_tests(errors):
    manifest = ROOT / 'scripts/required-go-tests.txt'
    pkg_sources = {}
    for lineno, raw_line in enumerate(manifest.read_text(encoding='utf-8').splitlines(), 1):
        line = raw_line.strip()
        if not line or line.startswith('#'):
            continue
        parts = line.split()
        if len(parts) != 3:
            errors.append(f'required-go-tests.txt:{lineno}: expected 3 fields, got {len(parts)}')
            continue
        _, rel_pkg, test_spec = parts
        pkg_dir = ROOT / 'control-plane' / rel_pkg
        if not pkg_dir.is_dir():
            errors.append(f'required-go-tests.txt:{lineno}: missing package directory control-plane/{rel_pkg}')
            continue
        if rel_pkg not in pkg_sources:
            pkg_sources[rel_pkg] = '\n'.join(
                p.read_text(encoding='utf-8') for p in sorted(pkg_dir.glob('*_test.go'))
            )
        src = pkg_sources[rel_pkg]
        segments = test_spec.split('/')
        parent = segments[0]
        if not re.search(rf'\bfunc\s+{re.escape(parent)}\s*\(', src):
            errors.append(f'required-go-tests.txt:{lineno}: missing Go test {parent} in control-plane/{rel_pkg}')
            continue
        for subtest in segments[1:]:
            if f'"{subtest}"' not in src:
                errors.append(
                    f'required-go-tests.txt:{lineno}: missing Go subtest "{subtest}" in control-plane/{rel_pkg}'
                )


def main():
    errors = []
    valid_profiles = bootstrap_profiles()
    check_workflows(errors, valid_profiles)
    check_actions(errors, valid_profiles)
    check_web_e2e_coverage(errors)
    check_specific_contracts(errors)
    check_required_go_tests(errors)
    if errors:
        for err in errors:
            print(f'workflow-contract FAIL: {err}', file=sys.stderr)
        sys.exit(1)
    print('Workflow quality and contract checks passed')


if __name__ == '__main__':
    main()
