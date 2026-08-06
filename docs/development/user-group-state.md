# User and group state

Users and groups are scoped to a node. The control plane records desired state, accepts mutations as asynchronous operations, and keeps agent observations separate. API consumers must display the returned convergence value rather than treating `202 Accepted` as remote success.

Mutations require `Idempotency-Key`, an expected desired version in `If-Match` or the request body, and a reason. Use `revision-0` only when creating a new user or group. A newer queued revision supersedes an older undispatched revision of the same node resource.

Password endpoints accept only an RSA-OAEP-SHA256 ciphertext in `sealed_password` plus the node's `secret_key_id`. Plaintext passwords are not accepted by the control plane. The root adapter accepts only its configured key identifier, decrypts with fixed `openssl` arguments and `/etc/ocservia/password-seal-private.pem`, and supplies plaintext to the fixed `ocpasswd` executable through stdin. Responses, observations, audit events, logs, and traces never contain password material or password hashes. Provision the matching public key and identifier through the node bootstrap channel; the private key must be root-readable only.

The group adapter replaces the fixed group file with a same-directory, fsynced atomic rename. Usernames, group names, and members use the ASCII pattern `^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`; callers cannot supply executable or file paths.

Automatic drift repair is disabled. An offline node retains queued work as `offline_pending`; a fresh observation exposes `converged`, `pending`, or `drifted`. Operators should inspect the linked operation before deciding whether to issue a new desired revision.

Rollback is a forward desired-state change: reapply the prior group members or desired user enabled state with the current version. Database rollback of migration `000012` is only suitable before I13 state or commands are in use because it removes the desired and observed records.
