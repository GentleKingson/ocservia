# Configuration planning

Configuration planning is intentionally separate from configuration apply. A
plan renders a typed template for one node, validates the node's configuration
revision and advertised capability matrix, and sends the immutable candidate to
the Agent for side-effect-free validation.

Templates contain an allowlisted set of Ocserv directives. Node variables use
`${NAME}` references. Secret-bearing directives use structured `SecretRef`
objects; the API does not accept secret values or caller-selected target paths.
Equivalent inputs produce identical canonical output and a SHA-256 candidate
hash.

The matched [complete node-local TLS profile](../operations/node-local-config-tls.md)
uses separate capabilities and typed payloads. It resolves immutable protected
TLS bundles and validates the entire native configuration, without omitted
directives or partial-validation warnings. Its result includes both the logical
candidate hash and the exact materialized file hash. These and the plan ID,
previous file hash, revision and expiry are bound into approval. Older nodes
cannot accept this profile and there is no legacy fallback.

For the legacy profile, privd writes a validation candidate only to a generated file beside the fixed
Ocserv configuration, validates every directive with a bounded structural parser, and
validates the non-secret directive set with the fixed Ocserv binary. Unresolved
SecretRef lines are omitted only from the native-parser staging input and are
reported as a typed warning; no secret key is materialized. Privd then removes
the staging file. It
fingerprints the current configuration before and after validation and rejects
the result if current state changed. Planning never replaces the current file
and never reloads Ocserv.

`GET /config-plans/{plan_id}` returns the candidate hash, operation state,
validation state, warnings, and a secret-safe diff. The response never contains
SecretRef keys or current secret values. A production apply approval can be
requested only after the plan is valid and unexpired; its independent approval
record is bound to the candidate hash, node, expected revision, and expiry.
Planning and apply never turn a stale caller revision into success by rereading
and substituting the current revision; a stale expected revision fails.

Before rolling back configuration planning, stop new plan creation and reconcile
or expire every nonterminal plan command. The down migration refuses to proceed
while such work exists and retains terminal typed command history for audit and
recovery compatibility.
