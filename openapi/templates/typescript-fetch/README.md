# oneOf Map Conversion

`modelOneOf.mustache` is the TypeScript Fetch template from the pinned
OpenAPI Generator 7.24.0 artifact. The only local changes preserve free-form
object-map branches during deserialization and serialization. The upstream
template otherwise falls through to `{}`, silently discarding immutable
approval content. Array/model branches retain their existing behavior.

Upstream: OpenAPITools/openapi-generator,
`modules/openapi-generator/src/main/resources/typescript-fetch/modelOneOf.mustache`,
Apache-2.0 (see the repository [LICENSE](../../../LICENSE)).

Regenerate with `scripts/generate.sh`. Do not edit generated TypeScript.
`web/test/approval-summary.test.ts` covers object content, typed batch arrays
and missing content. Recheck this override whenever the generator is upgraded.
