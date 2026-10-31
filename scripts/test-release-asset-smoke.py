#!/usr/bin/env python3
"""Offline orchestration checks; these never count as published-asset acceptance."""
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest

ROOT = Path(__file__).resolve().parent.parent


class ReleaseAssetSmokeTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        scripts = self.root / "scripts"
        scripts.mkdir()
        shutil.copy(ROOT / "scripts/release-asset-smoke.sh", scripts)
        self.bin = self.root / "bin"
        self.bin.mkdir()
        self.write(self.bin / "git", "#!/bin/sh\necho fixture-harness-sha\n")
        self.write(self.bin / "sudo", '#!/bin/sh\n[ "$*" = "-n true" ]\n')
        self.write(self.bin / "getent", "#!/bin/sh\nexit 2\n")
        self.write(self.bin / "dpkg-query", "#!/bin/sh\nexit 1\n")
        self.write(self.bin / "curl", r'''#!/usr/bin/env python3
import hashlib, os, pathlib, sys
args = sys.argv[1:]
assert "--fail" in args and "--location" in args
assert args[args.index("--proto") + 1] == "=https"
assert args[args.index("--proto-redir") + 1] == "=https"
url = args[-1]
assert url.startswith("https://github.com/GentleKingson/ocservia/releases/download/v")
assert "/latest/" not in url
mode = os.environ.get("FAIL_MODE", "")
if mode in ("404", "tls"):
    sys.exit(22 if mode == "404" else 35)
name = url.rsplit("/", 1)[1]
payload = b"published archive fixture"
if name.endswith(".sha256"):
    digest = "0" * 64 if mode == "checksum" else hashlib.sha256(payload).hexdigest()
    filename = "../other" if mode == "checksum-path" else name[:-7]
    payload = f"{digest}  {filename}\n".encode()
elif name.endswith(".sh"):
    tag = "v9.9.9" if mode == "version-mismatch" else url.split("/")[-2]
    payload = f'#!/bin/bash\nreadonly DEFAULT_VERSION="{tag}"\n'.encode()
if mode == "empty":
    payload = b""
pathlib.Path(args[args.index("--output") + 1]).write_bytes(payload)
''')
        self.write(scripts / "test-managed-node-install.sh", '''#!/bin/bash
set -eu
[[ "$1" == release-contracts && -x "$INSTALLER_UNDER_TEST" ]]
[[ "$TEST_RELEASE_VERSION" == "$EXPECTED_VERSION" ]]
echo contracts >>"$CALLS"
[[ "${FAIL_MODE:-}" != contracts ]]
[[ "${FAIL_MODE:-}" != skipped ]] || exit 0
echo "Managed node release contracts passed ($TEST_RELEASE_VERSION)"
''')
        self.write(scripts / "release-native-package-smoke.sh", '''#!/bin/bash
set -eu
[[ "$VERSION" == "$EXPECTED_VERSION" && "$STUB_BINARIES" == false && "$RC_LIFECYCLE" == false ]]
[[ -s "$PRODUCTS_DIR/managed-node-bootstrap.sh" ]]
echo native >>"$CALLS"
[[ "${FAIL_MODE:-}" != native ]]
''')

    def write(self, path, content):
        path.write_text(content)
        path.chmod(0o755)

    def run_smoke(self, tag="v2.0.0-rc.5", mode=""):
        env = dict(os.environ, PATH=f"{self.bin}:{os.environ['PATH']}",
                   RUN_ID="offline-test", ARTIFACT_DIR=str(self.root / "evidence"),
                   CALLS=str(self.root / "calls"), EXPECTED_VERSION=tag.removeprefix("v"),
                   FAIL_MODE=mode)
        result = subprocess.run(["bash", str(self.root / "scripts/release-asset-smoke.sh"), tag],
                                env=env, capture_output=True, text=True)
        summary = self.root / "evidence/release-assets-summary.txt"
        return result, summary.read_text() if summary.exists() else ""

    def test_published_rc_and_stable(self):
        for tag in ("v2.0.0-rc.5", "v2.0.0"):
            with self.subTest(tag=tag):
                result, summary = self.run_smoke(tag)
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertIn(f"release_tag={tag}\n", summary)
                self.assertIn("RELEASE_ASSET_DOWNLOAD=PASS\n", summary)
                self.assertEqual((self.root / "calls").read_text().splitlines()[-2:],
                                 ["contracts", "native"])
                urls = (self.root / "evidence/release-assets-downloads.txt").read_text().splitlines()
                self.assertEqual(len(urls), 5)
                self.assertTrue(all(f"/{tag}/" in url for url in urls))

    def test_reject_unpinned_or_synthetic_versions(self):
        for tag in ("latest", "main", "v0.0.0", "v2.0.0-rc.0", "v2.0.0-rc.01", "v2.0.0;false"):
            with self.subTest(tag=tag):
                result, summary = self.run_smoke(tag)
                self.assertEqual(result.returncode, 2)
                self.assertNotIn("=PASS", summary)
                self.assertFalse((self.root / "calls").exists())

    def test_failures_never_report_pass(self):
        for mode in ("404", "tls", "empty", "checksum", "checksum-path", "version-mismatch", "contracts", "skipped", "native"):
            with self.subTest(mode=mode):
                (self.root / "calls").unlink(missing_ok=True)
                result, summary = self.run_smoke(mode=mode)
                self.assertNotEqual(result.returncode, 0, result.stdout)
                self.assertIn("RELEASE_ASSET_DOWNLOAD=FAIL\n", summary)
                self.assertNotIn("RELEASE_ASSET_DOWNLOAD=PASS", summary)
                if mode not in ("contracts", "skipped", "native"):
                    self.assertFalse((self.root / "calls").exists())

    def test_existing_host_state_blocks_both_install_suites(self):
        self.write(self.bin / "sudo", "#!/bin/sh\nexit 0\n")
        result, summary = self.run_smoke()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("host already carries Agent state", result.stderr)
        self.assertIn("RELEASE_ASSET_DOWNLOAD=FAIL", summary)
        self.assertFalse((self.root / "calls").exists())


if __name__ == "__main__":
    unittest.main()
