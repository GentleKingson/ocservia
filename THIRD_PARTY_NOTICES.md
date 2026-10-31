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

The Web console includes UI component source adapted from shadcn-vue
commit `b251d9fd92aa496495e127137a7734704fb34a29`, licensed under the MIT
License. Copyright (c) 2023 unovue. The full notice is in
`web/src/components/ui/LICENSE.shadcn-vue`. The primitives were vendor-copied
on 2026-10-04 with shadcn-vue CLI 2.8.2 and are maintained as project-owned
source under `web/src/components/ui`. Local changes include explicit `as` and
`asChild` defaults for `exactOptionalPropertyTypes`, `defineModel` in place of
`useVModel` in the input components, `lucide-vue-next` imports rewritten to
`@lucide/vue`, no `tw-animate-css` dependency, and Sheet, Dialog and
DropdownMenu adaptations for accessibility and layout. The UI primitives are
purely presentational.

The generated TypeScript client uses the `modelOneOf.mustache` template from
OpenAPITools/openapi-generator
(`modules/openapi-generator/src/main/resources/typescript-fetch/modelOneOf.mustache`),
licensed under Apache-2.0. The copy in `openapi/templates/typescript-fetch`
changes only the handling of free-form object-map branches in deserialization
and serialization.

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

Quota, expiry, and related user-form behavior were reviewed against the
`mmtaee/ocserv-dashboard` repository, licensed under the MIT License. The
`v4.9` baseline is commit `b8f59026c4d879f40c1da43dc00d97e34f9790bc`, and the
reviewed `master` commit is `4d25478580d899b77460bdf0cf0a590cfdd26030`. No
upstream source file is copied verbatim. The pinned inventory and classification
are in `docs/upstream/v4.9-post1.manifest.json`, and the vendored diff is in
`docs/upstream/v4.9-post1.patch`.

The subsequent restore and write behavior review through dashboard commit
`f7e01eb3c4058635989c49dbd68bbd9a70d443f2` is recorded in
`docs/upstream/v4.9-post2.manifest.json`, also without verbatim source imports.
