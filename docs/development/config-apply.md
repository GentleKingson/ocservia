# Configuration apply and rollback

Configuration apply accepts only an unexpired, remotely validated plan and an
independent approval bound to that plan's candidate hash. The controller locks
the node, rechecks the plan revision, validation fingerprint, capability, and
automation lock, consumes the approval, and persists the operation, typed
command, outbox event, audit intent, and apply record in one transaction.

Privd accepts no path or executable from the Agent. It serializes planning and
apply work for the fixed Ocserv configuration, verifies the current fingerprint,
creates same-directory backup and staging files with preserved mode and ownership,
fsyncs them, atomically renames the candidate, fsyncs the directory, reloads the
fixed service, and checks the fixed parser, systemd state, and an `occtl` session query. Successful backups
are retained with a maximum of ten per node.

If reload or health validation fails, privd atomically restores the backup,
reloads the prior configuration, and verifies its fingerprint and health. The
result is `rolled_back`. If restore or rollback health validation fails, the
result is `failed_critical`; the controller locks further configuration
automation for the node and emits a critical security alert. Operators must
repair and verify the node locally before clearing that lock in a later recovery
workflow.

Before migration rollback, stop new apply requests and reconcile every
nonterminal `config_apply` command. Migration rollback refuses active work and
retains terminal typed command history for audit and compatibility.
