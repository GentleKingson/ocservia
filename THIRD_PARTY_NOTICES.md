# Third-party notices

ocservia uses third-party build tools and generated-code runtimes under their
respective licenses. Dependency lockfiles are the authoritative inventory for
the current source tree. CI rejects known strong-copyleft and source-available
dependency licenses unless a future change documents and reviews an exception.

The Iroh transport dependency graph includes small components under the
Unlicense and Zlib licenses and a public-root certificate data package under
CDLA-Permissive-2.0. These are permissive terms; their exact versions and
license texts are recorded by `Cargo.lock` and the generated dependency SBOM.

The project itself is licensed under `Apache-2.0`.

The Controller embeds Django 5.2.6's common/breached password blocklist,
from commit `75c4403f07b8ad25893f7832dbe8fc6814b53b2d`, under BSD-3-Clause.
Copyright (c) Django Software Foundation and individual contributors.
All rights reserved.
Source: https://github.com/django/django/blob/75c4403f07b8ad25893f7832dbe8fc6814b53b2d/django/contrib/auth/common-passwords.txt.gz
The complete copyright notice, license conditions and disclaimer are in
`control-plane/internal/auth/password_blocklist/LICENSE.django`. The Controller
runtime image includes that full text at `/usr/share/doc/ocservia/LICENSE.django`
alongside this notice. Blocklist provenance and maintenance are documented in
`control-plane/internal/auth/password_blocklist/README.md`.

Quota, expiry, and related user-form behavior were reviewed against
`mmtaee/ocserv-dashboard` v4.9 and commit
`4d25478580d899b77460bdf0cf0a590cfdd26030`, licensed under the MIT License.
No upstream source file is copied verbatim; provenance and the A/B/C/D decision
record are documented in `docs/upstream/v4.9-post1.md`.
