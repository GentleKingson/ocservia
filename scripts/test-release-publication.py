#!/usr/bin/env python3
"""Offline publication rehearsal with a synthetic version; no external writes."""
import copy
import hashlib
import importlib.util
import io
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch
from urllib.error import HTTPError, URLError

spec = importlib.util.spec_from_file_location('publication', Path(__file__).with_name('publish-release.py'))
pub = importlib.util.module_from_spec(spec)
spec.loader.exec_module(pub)
VERSION = '0.0.0-rc.1'
TAG = 'v' + VERSION
SHA = 'a' * 40


class PublicationTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)
        self.assets = self.root / 'assets'
        self.images = self.root / 'images'
        self.assets.mkdir()
        self.images.mkdir()
        names = ['install-controller', 'controller-bootstrap.sh', 'managed-node-bootstrap.sh',
                 'controller-release.json', 'controller-release-amd64.json', 'controller-release-arm64.json']
        for arch, rpm in [('amd64', 'x86_64'), ('arm64', 'aarch64')]:
            archive = f'ocservia-agent-{VERSION}-linux-{arch}.tar.gz'
            names += [archive, archive + '.sha256', f'ocservia-agent_{VERSION}-1_{arch}.deb',
                      f'ocservia-agent-{VERSION}-1.{rpm}.rpm']
            for name in pub.ROLES:
                (self.images / f'{name}-linux-{arch}.tar').write_bytes(b'test image archive')
        for name in names:
            (self.assets / name).write_bytes(name.encode())
        self.files = pub.asset_files(VERSION, self.assets)
        self.release = {'id': 42, 'tag_name': TAG, 'draft': True, 'immutable': False,
                        'prerelease': True, 'target_commitish': SHA}
        self.remote_assets = []
        self.remote_images = {}
        self.calls = []
        self.published = False
        self.fault = None
        self.push_failures = 0
        self.env = patch.dict(os.environ, {'GH_TOKEN': 'test', 'GITHUB_ACTOR': 'test',
                                          'GITHUB_STEP_SUMMARY': str(self.root / 'summary')})
        self.env.start()
        self.addCleanup(self.env.stop)

    def api(self, path, *args):
        if '/assets?' in path:
            return [copy.deepcopy(self.remote_assets)]
        if path == f'repos/test/repo/commits/refs/tags/{self.release["tag_name"]}':
            return {'sha': 'b' * 40 if self.fault == 'moved-tag' else SHA}
        if path.endswith('/latest'):
            return {'tag_name': 'v0.0.0'}
        if path.endswith('/releases?per_page=100'):
            return [[], [copy.deepcopy(self.release)]]
        if path.endswith('/releases/42'):
            return copy.deepcopy(self.release)
        raise AssertionError((path, args))

    def manifest(self, name, tag):
        return self.remote_images.get((name, tag))

    def command(self, *args, **kwargs):
        self.calls.append(args)
        if args[:3] == ('gh', 'release', 'upload'):
            self.assertNotIn('--clobber', args)
            self.remote_assets = [{'name': p.name, 'size': p.stat().st_size, 'state': 'uploaded',
                                   'digest': pub.digest(p)} for p in self.files]
            if self.fault == 'upload':
                raise subprocess.CalledProcessError(1, args)
            if self.fault == 'missing-asset':
                self.remote_assets.pop()
            if self.fault == 'bad-digest':
                self.remote_assets[0]['digest'] = 'sha256:bad'
            if self.fault == 'starter':
                self.remote_assets[0]['state'] = 'starter'
            if self.fault == 'duplicate':
                self.remote_assets.append(self.remote_assets[0])
        elif args[:3] == ('docker', 'image', 'inspect'):
            arch = args[3].rsplit('-', 1)[1]
            return json.dumps([{'Os': 'linux', 'Architecture': 'arm64' if self.fault == 'platform' else arch,
                                'Id': 'sha256:config-' + arch}])
        elif args[:2] == ('docker', 'push'):
            if self.push_failures:
                self.push_failures -= 1
                raise subprocess.CalledProcessError(1, args, output='layer: Pushed\n', stderr='unknown blob\n')
            if self.fault == 'push':
                raise subprocess.CalledProcessError(1, args)
            name, tag = args[2].rsplit('/', 1)[1].split(':')
            arch = tag.rsplit('-', 1)[1]
            self.remote_images[name, tag] = ('sha256:' + name + arch,
                                             {'config': {'digest': 'wrong' if self.fault == 'config' else 'sha256:config-' + arch}})
        elif args[:4] == ('docker', 'buildx', 'imagetools', 'create'):
            name, tag = args[5].rsplit('/', 1)[1].split(':')
            for ref in args[6:]:
                self.assertIn('@sha256:', ref)
            descriptors = [{'platform': {'os': 'linux', 'architecture': arch},
                            'digest': self.remote_images[name, f'{tag}-{arch}'][0]} for arch in pub.ARCHES]
            if self.fault == 'index':
                descriptors.pop()
            self.remote_images[name, tag] = ('sha256:' + name, {'manifests': descriptors})
        elif args[:2] == ('gh', 'api'):
            self.assertIn('draft=false', args)
            self.assertIn('make_latest=' + ('false' if self.release['prerelease'] else 'true'), args)
            self.assertEqual(len(self.remote_assets), 14)
            self.assertEqual(len(self.remote_images), 24)
            if self.fault == 'publish':
                raise subprocess.CalledProcessError(1, args)
            self.published = True
            self.release.update(draft=False, immutable=self.fault != 'mutable')
            if self.fault == 'changed-after-publish':
                self.remote_images['gateway', TAG] = ('sha256:changed', {})
        return ''

    def finish(self, version=VERSION):
        with patch.object(pub, 'api', side_effect=self.api), patch.object(pub, 'manifest', side_effect=self.manifest), \
                patch.object(pub, 'command', side_effect=self.command):
            pub.finish('test/repo', version, SHA, self.files, self.images)

    def test_complete_draft_to_immutable_release(self):
        self.finish()
        self.assertTrue(self.published)
        self.assertIn('14 assets, 24 verified image tags', (self.root / 'summary').read_text())

    def test_blob_error_retries_the_same_image_before_publication(self):
        self.push_failures = 1
        with patch('time.sleep') as sleep:
            self.finish()
        pushes = [args for args in self.calls if args[:2] == ('docker', 'push')]
        self.assertEqual(len(pushes), 17)
        self.assertEqual(pushes[0], pushes[1])
        sleep.assert_called_once_with(5)
        self.assertTrue(self.published)

    def test_persistent_blob_error_is_bounded_and_leaves_draft_unpublished(self):
        self.push_failures = 10
        with patch('time.sleep') as sleep, self.assertRaises(subprocess.CalledProcessError):
            self.finish()
        self.assertEqual(len([args for args in self.calls if args[:2] == ('docker', 'push')]), 3)
        self.assertEqual([call.args for call in sleep.call_args_list], [(5,), (15,)])
        self.assertFalse(self.published)
        self.assertFalse((self.root / 'summary').exists())

    def test_stable_publication_sets_latest(self):
        self.release.update(tag_name='v0.0.0', prerelease=False)
        self.finish(version='0.0.0')
        self.assertTrue(self.published)

    def test_build_only_asset_check_entrypoint(self):
        env = dict(os.environ, VERSION=VERSION, RUNNER_TEMP=str(self.root))
        entrypoint = ['python3', str(Path(pub.__file__)), 'check-assets']
        result = subprocess.run(entrypoint, env=env, capture_output=True, text=True)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.files[0].unlink()
        result = subprocess.run(entrypoint, env=env, capture_output=True, text=True)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('Incomplete', result.stderr)

    def test_failures_never_report_success(self):
        for fault in ('upload', 'missing-asset', 'bad-digest', 'starter', 'duplicate', 'platform',
                      'push', 'config', 'index', 'moved-tag', 'publish', 'mutable', 'changed-after-publish'):
            with self.subTest(fault=fault):
                self.fault = fault
                self.release.update(draft=True, immutable=False)
                self.remote_images = {}
                self.published = False
                with self.assertRaises((RuntimeError, subprocess.CalledProcessError)):
                    self.finish()
                self.assertEqual(self.published, fault in ('mutable', 'changed-after-publish'))
                self.assertFalse((self.root / 'summary').exists())

    def test_existing_release_including_draft_on_later_page_blocks_preflight(self):
        for draft in (True, False):
            with patch.object(pub, 'api', return_value=[[], [{'tag_name': TAG, 'draft': draft}]]), \
                    patch.object(pub, 'manifest') as registry:
                with self.assertRaisesRegex(RuntimeError, 'already exists'):
                    pub.preflight('test/repo', TAG, self.images)
                registry.assert_not_called()

    def test_preflight_checks_every_tag_and_rejects_each_collision(self):
        with patch.object(pub, 'api', return_value=[[]]), patch.object(pub, 'manifest', return_value=None) as registry:
            pub.preflight('test/repo', TAG, self.images)
            self.assertEqual(registry.call_count, 24)
        for name in pub.ROLES:
            for suffix in ('', '-amd64', '-arm64'):
                with patch.object(pub, 'api', return_value=[[]]), patch.object(pub, 'manifest', side_effect=self.manifest):
                    self.remote_images = {(name, TAG + suffix): ('sha256:existing', {})}
                    with self.assertRaisesRegex(RuntimeError, 'must not be overwritten'):
                        pub.preflight('test/repo', TAG, self.images)

    def test_collision_after_preflight_cannot_push(self):
        self.remote_images['gateway', TAG + '-amd64'] = ('sha256:existing', {})
        with self.assertRaisesRegex(RuntimeError, 'must not be overwritten'):
            self.finish()
        self.assertFalse(any(args[:2] == ('docker', 'push') for args in self.calls))
        self.assertFalse(self.published)

    def test_release_api_failure_is_not_absence(self):
        with patch.object(pub, 'api', side_effect=subprocess.CalledProcessError(1, 'gh')), \
                patch.object(pub, 'manifest') as registry:
            with self.assertRaises(subprocess.CalledProcessError):
                pub.preflight('test/repo', TAG, self.images)
            registry.assert_not_called()

    def test_missing_image_archive_blocks_preflight(self):
        (self.images / 'gateway-linux-amd64.tar').unlink()
        with patch.object(pub, 'api', return_value=[[]]), patch.object(pub, 'manifest') as registry:
            with self.assertRaisesRegex(RuntimeError, 'Missing image'):
                pub.preflight('test/repo', TAG, self.images)
            registry.assert_not_called()

    def test_asset_inventory(self):
        self.assertEqual(len(self.files), 14)
        path = self.files[0]
        path.write_bytes(b'')
        with self.assertRaisesRegex(RuntimeError, 'empty'):
            pub.asset_files(VERSION, self.assets)
        path.unlink()
        with self.assertRaisesRegex(RuntimeError, 'Incomplete'):
            pub.asset_files(VERSION, self.assets)

    def test_unexpected_or_wrong_draft_is_rejected(self):
        for change in ({'draft': False}, {'target_commitish': 'b' * 40}, {'prerelease': False}):
            with patch.object(pub, 'api', return_value=[[self.release | change]]):
                with self.assertRaisesRegex(RuntimeError, 'Expected a new draft'):
                    pub.draft('test/repo', TAG, SHA, True)

    def test_draft_lookup_includes_later_pages_and_avoids_published_only_tag_endpoint(self):
        with patch.object(pub, 'api', side_effect=self.api) as api:
            self.assertEqual(pub.draft('test/repo', TAG, SHA, True), 42)
            api.assert_called_once_with('repos/test/repo/releases?per_page=100', '--paginate', '--slurp')

    def test_missing_duplicate_and_wrong_tag_drafts_are_rejected(self):
        for pages in ([[]], [[self.release, self.release]], [[self.release | {'tag_name': 'other'}]]):
            with self.subTest(pages=pages), patch.object(pub, 'api', return_value=pages):
                with self.assertRaisesRegex(RuntimeError, 'Expected a new draft'):
                    pub.draft('test/repo', TAG, SHA, True)


class PushTest(unittest.TestCase):
    def test_failed_commands_preserve_stdout_and_stderr(self):
        with patch.object(sys, 'stderr', new_callable=io.StringIO) as output:
            with self.assertRaises(subprocess.CalledProcessError) as caught:
                pub.command(sys.executable, '-c',
                            'import sys; print("layer: Pushed"); print("unknown blob", file=sys.stderr); sys.exit(1)')
        self.assertIn('layer: Pushed', output.getvalue())
        self.assertIn('unknown blob', output.getvalue())
        self.assertIn('unknown blob', caught.exception.stderr)

    def test_json_stdout_is_not_mixed_with_stderr(self):
        with patch.object(sys, 'stderr', new_callable=io.StringIO) as output:
            result = pub.command(sys.executable, '-c', 'import sys; print("{}"); print("warning", file=sys.stderr)')
        self.assertEqual(json.loads(result), {})
        self.assertIn('warning', output.getvalue())

    def test_other_push_errors_are_not_retried(self):
        for message in ('denied', 'unauthorized', 'timeout', 'manifest unknown', ''):
            error = subprocess.CalledProcessError(1, ['docker', 'push'], stderr=message)
            with self.subTest(message=message), patch.object(pub, 'manifest', return_value=None), \
                    patch.object(pub, 'command', side_effect=error) as push, patch('time.sleep') as sleep:
                with self.assertRaises(subprocess.CalledProcessError):
                    pub.push_image('gateway', TAG)
                self.assertEqual(push.call_count, 1)
                sleep.assert_not_called()

    def test_retry_never_overwrites_a_manifest_or_ignores_registry_failure(self):
        error = subprocess.CalledProcessError(1, ['docker', 'push'], stderr='unknown blob\n')
        for next_result in (('sha256:existing', {}), URLError('registry unavailable')):
            with self.subTest(next_result=next_result), patch.object(pub, 'manifest', side_effect=[None, next_result]), \
                    patch.object(pub, 'command', side_effect=error) as push, patch('time.sleep'):
                with self.assertRaises((RuntimeError, URLError)):
                    pub.push_image('gateway', TAG)
                self.assertEqual(push.call_count, 1)


class NotesBaseTest(unittest.TestCase):
    def release(self, number, **changes):
        return {'tag_name': f'v2.0.0-rc.{number}', 'draft': False, 'prerelease': True} | changes

    def test_skips_failed_drafts_and_selects_numeric_predecessor_across_pages(self):
        pages = [[self.release(6, draft=True), self.release(4)], [self.release(5)]]
        with patch.object(pub, 'api', return_value=pages) as api:
            self.assertEqual(pub.previous_rc('test/repo', '2.0.0-rc.7'), 'v2.0.0-rc.5')
        api.assert_called_once_with('repos/test/repo/releases?per_page=100', '--paginate', '--slurp')
        with patch.object(pub, 'api', return_value=[[self.release(9), self.release(10), self.release(2)]]):
            self.assertEqual(pub.previous_rc('test/repo', '2.0.0-rc.11'), 'v2.0.0-rc.10')

    def test_wrong_series_current_future_non_rc_and_unpublished_are_not_bases(self):
        invalid = [self.release(6, draft=True), self.release(5, prerelease=False), self.release(7),
                   self.release(8), self.release(4, tag_name='v2.1.0-rc.4'),
                   self.release(4, tag_name='v2.0.0'), self.release(4, tag_name='v2.0.0-rc.04')]
        for pages in ([invalid], [[]]):
            with patch.object(pub, 'api', return_value=pages), self.assertRaisesRegex(RuntimeError, 'No published earlier RC'):
                pub.previous_rc('test/repo', '2.0.0-rc.7')

    def test_release_api_failure_is_not_a_fallback_to_older_history(self):
        with patch.object(pub, 'api', side_effect=URLError('unavailable')), self.assertRaises(URLError):
            pub.previous_rc('test/repo', '2.0.0-rc.7')


class RegistryTest(unittest.TestCase):
    def setUp(self):
        env = patch.dict(os.environ, {'GITHUB_ACTOR': 'test', 'GH_TOKEN': 'test'})
        env.start()
        self.addCleanup(env.stop)

    def response(self, raw, digest=None):
        response = io.BytesIO(raw)
        response.headers = {'Docker-Content-Digest': digest or 'sha256:' + hashlib.sha256(raw).hexdigest()}
        return response

    def error(self, status, code):
        return HTTPError('https://ghcr.io/test', status, 'test error', {},
                         io.BytesIO(json.dumps({'errors': [{'code': code}]}).encode()))

    def test_only_explicit_manifest_unknown_is_absence(self):
        for status, code in ((404, 'MANIFEST_UNKNOWN'), (404, 'DENIED'), (401, 'UNAUTHORIZED'),
                             (403, 'DENIED'), (429, 'TOOMANYREQUESTS'), (500, 'UNKNOWN')):
            with self.subTest(status=status, code=code), patch.object(pub, 'urlopen', side_effect=[
                    self.response(b'{"token":"test"}'), self.error(status, code)]):
                if status == 404 and code == 'MANIFEST_UNKNOWN':
                    self.assertIsNone(pub.manifest('gateway', TAG))
                else:
                    with self.assertRaises(HTTPError):
                        pub.manifest('gateway', TAG)

    def test_token_and_transport_errors_are_not_absence(self):
        for error in (self.error(404, 'MANIFEST_UNKNOWN'), URLError('timeout')):
            with patch.object(pub, 'urlopen', side_effect=error), self.assertRaises((HTTPError, URLError)):
                pub.manifest('gateway', TAG)

    def test_manifest_digest_is_verified(self):
        for digest in (None, 'sha256:wrong'):
            with patch.object(pub, 'urlopen', side_effect=[self.response(b'{"token":"test"}'),
                                                         self.response(b'{"schemaVersion":2}', digest)]):
                if digest:
                    with self.assertRaisesRegex(RuntimeError, 'digest mismatch'):
                        pub.manifest('gateway', TAG)
                else:
                    self.assertEqual(pub.manifest('gateway', TAG)[1], {'schemaVersion': 2})


if __name__ == '__main__':
    unittest.main()
