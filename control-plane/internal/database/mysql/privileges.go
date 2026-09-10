package mysql

import (
	"context"
	"fmt"
	"regexp"
	"strings"
)

// Account creation/password rotation belongs to the provisioning administrator,
// not the migration connection. This applies only to fresh, non-inheriting
// accounts; it intentionally does not revoke an existing account's privileges.
// The acceptance harness creates and checks those accounts independently.
var runtimePrivileges = []struct{ privileges, tables string }{
	{"SELECT", "backend_migrations,backend_migration_steps,backend_schema_revisions,backend_schema_revision_steps,controller_schema_compatibility,roles,upstream_sync_records,telemetry_legacy_migration"},
	{"SELECT,INSERT,UPDATE,DELETE", "workspaces,nodes,operations,local_auth_attempts,local_slice_jobs,commands,outbox_events,command_attempts,node_command_leases,operation_events,telemetry_rollups_5m,telemetry_rollups_1h,node_sessions"},
	{"SELECT,INSERT,UPDATE", "enrollment_tokens,node_endpoint_keys,node_capabilities,node_bootstrap_tokens,node_trust_convergence,identities,auth_sessions,local_credentials,approval_requests,security_alerts,privd_attestation_enrollment_credentials,node_privd_attestation_keys,transport_event_cursor,node_observed_snapshots,desired_users,desired_groups,desired_user_policies,user_policy_mutations,observed_user_usage,user_usage_cursors,scheduler_leases,user_policy_enforcements,batch_operations,batch_operation_items,scheduler_leadership,connection_owner_fencing,node_config_state,config_apply_operations,agent_upgrade_operations,agent_rollouts,agent_rollout_nodes,certificates,artifact_operations,secret_provider_refs"},
	{"SELECT,INSERT", "node_sealing_keys,audit_events,local_auth_bootstrap,audit_checkpoints,break_glass_uses,agent_command_results,transport_events,transport_event_quarantine,telemetry_ingest_batches,config_plans,node_agent_upgrade_results,telemetry_security_events,telemetry_samples,approval_authority_resources,approval_batch_items,business_locks"},
	{"SELECT,INSERT,DELETE", "role_bindings,node_ip_bans,observed_users,observed_groups"},
	{"UPDATE(completion_pending,completed_at,approver_identity_id)", "local_auth_bootstrap"},
	{"UPDATE(transport_cursor_valid)", "transport_events"},
	{"UPDATE(lock_key)", "business_locks"},
	{"SELECT,UPDATE(key_name)", "exact_key_guards"},
	// MySQL requires a write privilege for current locking reads. The
	// unconditional audit_events_reject_update trigger rejects even no-ops.
	{"UPDATE(event_hash)", "audit_events"},
}

// GrantTestPrivileges is restricted to the fixed development accounts. The
// caller must be the owner (with schema-scoped GRANT OPTION) or administrator.
// No wildcard grants, metadata writes or DDL go to runtime. Audit mutations
// remain prohibited by immutable triggers, including the lock-read column.
func (b *Backend) GrantTestPrivileges(ctx context.Context) error {
	if err := b.GrantRuntimePrivileges(ctx, "ocservia_app@%"); err != nil {
		return err
	}
	var name string
	if err := b.QueryRow(ctx, "SELECT DATABASE()").Scan(&name); err != nil {
		return err
	}
	if !identifier.MatchString(name) {
		return fmt.Errorf("experimental database: invalid schema identifier")
	}
	// Retention is restricted to telemetry; no audit, authorization, or
	// migration writes and no arbitrary DDL are granted to maintenance.
	for _, table := range []string{"telemetry_samples", "telemetry_rollups_5m", "telemetry_rollups_1h"} {
		if _, err := b.Exec(ctx, "GRANT SELECT,DELETE ON `"+name+"`.`"+table+"` TO 'ocservia_maintenance'@'%'"); err != nil {
			return err
		}
	}
	return b.grantTelemetryPrivileges(ctx, "'ocservia_maintenance'@'%'", false)
}

var accountUser = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,32}$`)
var accountHost = regexp.MustCompile(`^[A-Za-z0-9.%:_-]{1,255}$`)

// An explicit user@host names one pre-provisioned account. Neither SQL quoting
// nor driver DSN syntax is accepted here; GRANT cannot bind identifiers.
func quotedRuntimeAccount(account string) (string, error) {
	user, host, ok := strings.Cut(account, "@")
	if !ok || !accountUser.MatchString(user) || !accountHost.MatchString(host) {
		return "", fmt.Errorf("database runtime account must be an explicit user@host without SQL quoting")
	}
	return "'" + user + "'@'" + host + "'", nil
}

// GrantRuntimePrivileges grants only named runtime objects to an existing
// account. It neither creates accounts nor grants owner/maintenance privileges.
func (b *Backend) GrantRuntimePrivileges(ctx context.Context, account string) error {
	quoted, err := quotedRuntimeAccount(account)
	if err != nil {
		return err
	}
	var name string
	if err := b.QueryRow(ctx, "SELECT DATABASE()").Scan(&name); err != nil {
		return err
	}
	if !identifier.MatchString(name) {
		return fmt.Errorf("experimental database: invalid schema identifier")
	}
	for _, g := range runtimePrivileges {
		for _, table := range strings.Split(g.tables, ",") {
			if _, err := b.Exec(ctx, "GRANT "+g.privileges+" ON `"+name+"`.`"+table+"` TO "+quoted); err != nil {
				return err
			}
		}
	}
	return b.grantTelemetryPrivileges(ctx, quoted, true)
}
