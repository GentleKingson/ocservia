#!/usr/bin/env python3
"""Exercise Actions evidence selection and publication with mock API responses."""

import importlib.util
import io
import json
import os
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch
from zipfile import ZipFile

import release_evidence as evidence


REPO = 'org/project'
SHA = 'a' * 40
BASE = 'b' * 40
ASSET_DIGEST = 'c' * 64
MANIFEST_DIGEST = 'd' * 64


def run(run_id, title, **fields):
    return {'id': run_id, 'run_number': run_id, 'run_attempt': 1, 'head_sha': SHA, 'head_branch': 'codex/release-dag-MkmqHg',
            'head_repository': {'full_name': REPO}, 'display_title': title, 'event': 'workflow_dispatch',
            'status': 'completed', 'conclusion': 'success', 'html_url': f'https://example.invalid/{run_id}', **fields}


def job(name, conclusion='success', attempt=1, steps=()):
    return {'name': name, 'conclusion': conclusion, 'run_attempt': attempt, 'steps': steps}


def receipt(producer, version, **record):
    buffer = io.BytesIO()
    with ZipFile(buffer, 'w') as archive:
        archive.writestr('release-check.json', json.dumps({
            'schema': 2, 'candidate': evidence.coordinates(producer, version), **record,
        }))
    return buffer.getvalue()


class CISelection(unittest.TestCase):
    def test_picks_exact_quick_and_reuses_only_passed_full_jobs(self):
        runs = [run(1, 'Merge', event='push', conclusion='failure'),
                run(2, 'Basic CI full', conclusion='failure'),
                run(3, 'Basic CI quick', head_sha=BASE),
                run(4, 'Basic CI quick', head_branch='topic'),
                run(5, 'Basic CI quick', event='pull_request')]
        quick_jobs = [job('Basic CI Result'), job('database-smoke'),
                      job('ci-tools', steps=[{'name': f'Contract baseline {BASE}', 'conclusion': 'success'}])]
        full_jobs = [job('go', 'failure'), job('database-smoke', 'failure'),
                     job('database-smoke', attempt=2), job('database-recovery-full')]
        with patch.object(evidence, 'list_items', side_effect=[runs, quick_jobs, runs, full_jobs]), \
                patch.object(evidence, 'dispatch') as dispatch:
            result = evidence.ci(REPO, SHA, BASE, 0)
        self.assertTrue(result['contract_done'])
        self.assertTrue(result['database_smoke_done'])
        self.assertTrue(result['database_done'])
        self.assertTrue(result['recovery_done'])
        dispatch.assert_not_called()

    def test_quick_only_with_a_different_baseline_needs_release_checks(self):
        runs = [run(1, 'Basic CI quick')]
        quick_jobs = [job('Basic CI Result'), job('database-smoke', 'skipped'),
                      job('ci-tools', steps=[{'name': f'Contract baseline {SHA}', 'conclusion': 'success'}])]
        with patch.object(evidence, 'list_items', side_effect=[runs, quick_jobs, runs]):
            result = evidence.ci(REPO, SHA, BASE, 0)
        self.assertFalse(result['contract_done'])
        self.assertFalse(result['database_smoke_done'])
        self.assertFalse(result['database_done'])
        self.assertFalse(result['recovery_done'])


class Dispatch(unittest.TestCase):
    def test_missing_run_dispatches_once_and_waits_for_returned_id(self):
        completed = run(1, 'Security Checks')
        queued = {**completed, 'status': 'queued', 'conclusion': None}
        with patch.object(evidence, 'list_items', return_value=[]), patch.object(evidence.time, 'sleep'), \
                patch.object(evidence, 'api', side_effect=[{'object': {'sha': SHA}},
                                                          {'workflow_run_id': 1, 'html_url': completed['html_url']},
                                                          queued, completed]) as api:
            selected = evidence.find_or_dispatch(REPO, 'security.yml', SHA, lambda item: item['head_sha'] == SHA,
                                                 {}, float('inf'))
        self.assertEqual(selected, completed)
        dispatch = api.call_args_list[1]
        self.assertEqual(json.loads(dispatch.kwargs['data']), {'ref': 'codex/release-dag-MkmqHg', 'inputs': {'expected_sha': SHA}})
        self.assertIn('--method', dispatch.args)
        self.assertEqual([call.args[1] for call in api.call_args_list[2:]], ['actions/runs/1'] * 2)

    def test_existing_running_run_waits_without_dispatch(self):
        completed = run(1, 'Security Checks')
        with patch.object(evidence, 'list_items', return_value=[{**completed, 'status': 'in_progress'}]), \
                patch.object(evidence.time, 'sleep'), patch.object(evidence, 'api', return_value=completed), \
                patch.object(evidence, 'dispatch') as dispatch:
            self.assertEqual(evidence.find_or_dispatch(REPO, 'security.yml', SHA, lambda item: True,
                                                       {}, float('inf')), completed)
        dispatch.assert_not_called()

    def test_moved_main_fails_before_dispatch(self):
        with patch.object(evidence, 'api', return_value={'object': {'sha': BASE}}) as api:
            with self.assertRaisesRegex(RuntimeError, 'main moved'):
                evidence.dispatch(REPO, 'security.yml', SHA, {})
        api.assert_called_once_with(REPO, 'git/ref/heads/codex/release-dag-MkmqHg')


class Publication(unittest.TestCase):
    def fixture(self, version, *, candidate_changes=None, record_version=None, corrupt=False,
                expire=False, artifact_run_id=10, asset_digest=ASSET_DIGEST):
        check = run(10, f'Release Check v{version}', run_attempt=2)
        products = {key: {'id': index, 'manifest_sha256': MANIFEST_DIGEST}
                    for index, key in enumerate(evidence.PRODUCTS, 20)}
        record = receipt({**check, **(candidate_changes or {})}, record_version or version,
                         products=products, assets={'id': 25, 'sha256': ASSET_DIGEST})
        artifacts = [
            {'id': item['id'], 'name': f'{prefix}-{arch}-{artifact_run_id}-1', 'expired': expire}
            for index, (key, item) in enumerate(products.items())
            for prefix, arch in [('agent-packages' if index < 2 else 'controller-images', key.split('_')[1])]
        ]
        artifacts.extend([
            {'id': 25, 'name': 'release-assets-10-1', 'expired': False, 'digest': f'sha256:{asset_digest}'},
            {'id': 30, 'name': 'release-check-10-2', 'expired': False,
             'digest': evidence.artifact_digest(record if not corrupt else b'other bytes')},
        ])

        def gh(repo, path, *args, data=None):
            self.assertIsNone(data)
            if path == 'actions/artifacts/30/zip':
                return record
            listings = {
                'actions/runs/10/artifacts': artifacts,
                'actions/workflows/release-check.yml/runs': [check, run(12, 'Release Check v8.0.0', conclusion='failure')],
            }
            self.assertIn('--paginate', args)
            return '\n'.join(json.dumps(item) for item in listings[path.split('?', 1)[0]]).encode()

        return gh

    def test_tag_publish_uses_qualification_artifacts_for_final_and_rc(self):
        spec = importlib.util.spec_from_file_location('publish_release', Path(__file__).with_name('publish-release.py'))
        publisher = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(publisher)
        for version in ('2.0.0', '2.0.0-rc.1'):
            with self.subTest(version=version), tempfile.TemporaryDirectory() as tmp, \
                    patch.object(evidence, 'gh', side_effect=self.fixture(version)), \
                    patch.object(publisher, 'command', return_value=SHA), \
                    patch.object(publisher, 'api', return_value={'sha': SHA}), \
                    patch.dict(os.environ, GITHUB_OUTPUT=f'{tmp}/output', GITHUB_STEP_SUMMARY=f'{tmp}/summary',
                               REQUIRE_QUALIFICATION_ID='30'):
                publisher.qualify(REPO, f'v{version}', SHA)
                outputs = dict(line.split('=', 1) for line in Path(f'{tmp}/output').read_text().splitlines())
                self.assertEqual(outputs['qualification_run_id'], '10')
                self.assertEqual(outputs['controller_amd64_id'], '22')
                self.assertEqual(outputs['controller_arm64_id'], '23')
                self.assertEqual(outputs['assets_id'], '25')
                with patch.dict(os.environ, REQUIRE_QUALIFICATION_ID='29'):
                    with self.assertRaisesRegex(RuntimeError, 'qualification changed'):
                        publisher.qualify(REPO, f'v{version}', SHA)
                with patch.object(publisher, 'api', return_value={'sha': BASE}):
                    with self.assertRaisesRegex(RuntimeError, 'tag no longer matches'):
                        publisher.qualify(REPO, f'v{version}', SHA)

    def test_latest_failed_or_running_check_prevents_reuse_of_older_success(self):
        for state, conclusion in [('completed', 'failure'), ('in_progress', None)]:
            runs = [run(1, 'Release Check v2.0.0'),
                    run(2, 'Release Check v2.0.0', status=state, conclusion=conclusion)]
            with self.subTest(state=state), patch.object(evidence, 'list_items', return_value=runs):
                with self.assertRaisesRegex(RuntimeError, 'successful run'):
                    evidence.qualify(REPO, SHA, '2.0.0')

    def test_invalid_qualification_or_artifact_blocks_publication(self):
        for kwargs, message in [(dict(candidate_changes={'head_sha': BASE}), 'Candidate mismatch'),
                                (dict(record_version='2.1.0'), 'Candidate mismatch'),
                                (dict(corrupt=True), 'Artifact digest mismatch'),
                                (dict(expire=True), 'expired'),
                                (dict(artifact_run_id=11), 'unexpected artifact'),
                                (dict(asset_digest=MANIFEST_DIGEST), 'Release assets digest mismatch')]:
            with self.subTest(kwargs=kwargs), patch.object(evidence, 'gh', side_effect=self.fixture('2.0.0', **kwargs)):
                with self.assertRaisesRegex(RuntimeError, message):
                    evidence.qualify(REPO, SHA, '2.0.0')


if __name__ == '__main__':
    unittest.main()
