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

Privd writes the candidate only to a generated file beside the fixed Ocserv
configuration, validates it with bounded parsing and the fixed Ocserv binary
when no unresolved SecretRef remains, then removes the staging file. It
fingerprints the current configuration before and after validation and rejects
the result if current state changed. Planning never replaces the current file
and never reloads Ocserv.

`GET /config-plans/{plan_id}` returns the candidate hash, operation state,
validation state, warnings, and a secret-safe diff. The response never contains
SecretRef keys or current secret values. A production apply approval can be
requested only after the plan is valid and unexpired; its independent approval
record is bound to the candidate hash, node, expected revision, and expiry.
