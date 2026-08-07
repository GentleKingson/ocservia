# Third-party notices

ocservia uses third-party build tools and generated-code runtimes under their
respective licenses. Dependency lockfiles are the authoritative inventory for
the current source tree. CI rejects known strong-copyleft and source-available
dependency licenses unless a future change documents and reviews an exception.

The Iroh transport dependency graph includes small components under the
Unlicense and Zlib licenses and a public-root certificate data package under
CDLA-Permissive-2.0. These are permissive terms; their exact versions and
license texts are recorded by `Cargo.lock` and the generated dependency SBOM.

The project itself is available under `MIT OR Apache-2.0`.

Quota, expiry, and related user-form behavior were reviewed against
`mmtaee/ocserv-dashboard` v4.9 and commit
`4d25478580d899b77460bdf0cf0a590cfdd26030`, licensed under the MIT License.
No upstream source file is copied verbatim; provenance and the A/B/C/D decision
record are documented in `docs/upstream/v4.9-post1.md`.
