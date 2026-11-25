#!/usr/bin/env python3
"""Check the release fixture against the actual X.509 issuance window."""

import ssl
import subprocess
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

from release_business_scenarios import prepare_signer_authority, signer_ttl_seconds, verify_signer_window


class SignerWindow(unittest.TestCase):
    def test_issuance_deadline_must_precede_expiry(self):
        for duration in ('2160h', '24h30m'):
            with self.subTest(duration=duration), tempfile.TemporaryDirectory() as directory:
                work = Path(directory)
                (work / 'private').mkdir()
                prepare_signer_authority(work, duration)
                ttl = signer_ttl_seconds(duration)
                end = subprocess.check_output(['openssl', 'x509', '-in', work / 'issuer.pem',
                                               '-noout', '-enddate'], text=True).strip()
                expiry = ssl.cert_time_to_seconds(end.split('=', 1)[1])
                for remaining in (ttl, ttl - 1):
                    with patch('release_business_scenarios.time.time', return_value=expiry - remaining):
                        with self.assertRaisesRegex(RuntimeError, 'issuer.pem does not cover'):
                            verify_signer_window(work, ttl)


if __name__ == '__main__':
    unittest.main()
