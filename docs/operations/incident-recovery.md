# Production incident recovery

For an OIDC outage, keep existing bounded sessions until their normal expiry, reject new authentication when the issuer cannot be verified, and never enable a local password fallback. Restore issuer connectivity and verify discovery, signing-key refresh, and callback behavior before closing the incident.

For a relay outage, leave the healthy dedicated relay configured, repair the failed relay, and verify an Agent can reconnect through each URL independently. Do not switch production to public relays.

For a deployment rollback, stop new writes, reconcile every Unknown operation, and follow the public migration down guard. Restore the previous signed Agent or service artifact only when its protocol and schema compatibility includes the current release. Restore PostgreSQL from a verified backup when a forward-compatible rollback is impossible. Record the exact release, migration, backup, and audit checkpoint used.

Never guess an Unknown remote-write outcome. Preserve logs and scoped diagnostics, rotate exposed credentials, and involve the external OIDC, PKI/HSM, relay, or telemetry owner when the failing trust boundary is outside Ocservia.
