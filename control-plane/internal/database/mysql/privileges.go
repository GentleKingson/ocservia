package mysql

import (
	"context"
	"fmt"
	"strings"
)

// Account creation/password rotation belongs to the provisioning administrator,
// not the migration connection. This applies only to fresh, non-inheriting
// accounts; it intentionally does not revoke an existing account's privileges.
// The acceptance harness creates and checks those accounts independently.
var runtimePrivileges = []struct{ privileges, tables string }{
	{"SELECT", "backend_migrations,backend_migration_steps,backend_schema_revisions,backend_schema_revision_steps,controller_schema_compatibility,roles,upstream_sync_records"},
	{"SELECT,INSERT,UPDATE,DELETE", "workspaces,nodes,operations,local_auth_attempts,local_slice_jobs,commands,outbox_events,command_attempts,node_command_leases,operation_events,telemetry_rollups_5m,telemetry_rollups_1h,node_sessions"},
	{"SELECT,INSERT,UPDATE", "enrollment_tokens,node_endpoint_keys,node_capabilities,node_bootstrap_tokens,node_trust_convergence,identities,auth_sessions,local_credentials,approval_requests,security_alerts,privd_attestation_enrollment_credentials,node_privd_attestation_keys,transport_event_cursor,node_observed_snapshots,desired_users,desired_groups,desired_user_policies,user_policy_mutations,observed_user_usage,user_usage_cursors,scheduler_leases,user_policy_enforcements,batch_operations,batch_operation_items,scheduler_leadership,connection_owner_fencing,node_config_state,config_apply_operations,agent_upgrade_operations,agent_rollouts,agent_rollout_nodes,certificates,artifact_operations,secret_provider_refs"},
	{"SELECT,INSERT", "node_sealing_keys,audit_events,local_auth_bootstrap,audit_checkpoints,break_glass_uses,agent_command_results,transport_events,transport_event_quarantine,telemetry_ingest_batches,config_plans,node_agent_upgrade_results,telemetry_security_events,telemetry_samples,approval_authority_resources,approval_batch_items,business_locks"},
	{"SELECT,INSERT,DELETE", "role_bindings,node_ip_bans,observed_users,observed_groups"},
	{"UPDATE(completion_pending,completed_at,approver_identity_id)", "local_auth_bootstrap"},
	{"UPDATE(transport_cursor_valid)", "transport_events"},
	{"UPDATE(lock_key)", "business_locks"},
	{"SELECT,UPDATE(key_name)", "exact_key_guards"},
}

// GrantTestPrivileges is restricted to the fixed development accounts. The
// caller must be the owner (with schema-scoped GRANT OPTION) or administrator.
// No wildcard grants, metadata writes, audit updates or DDL go to runtime.
func (b *Backend) GrantTestPrivileges(ctx context.Context) error {
	var name string
	if err := b.QueryRow(ctx, "SELECT DATABASE()").Scan(&name); err != nil {
		return err
	}
	if !identifier.MatchString(name) {
		return fmt.Errorf("experimental database: invalid schema identifier")
	}
	for _, g := range runtimePrivileges {
		for _, table := range strings.Split(g.tables, ",") {
			if _, err := b.Exec(ctx, "GRANT "+g.privileges+" ON `"+name+"`.`"+table+"` TO 'ocservia_app'@'%'"); err != nil {
				return err
			}
		}
	}
	// Retention is restricted to telemetry; no audit, authorization, or
	// migration writes and no arbitrary DDL are granted to maintenance.
	for _, table := range []string{"telemetry_samples", "telemetry_rollups_5m", "telemetry_rollups_1h"} {
		if _, err := b.Exec(ctx, "GRANT SELECT,DELETE ON `"+name+"`.`"+table+"` TO 'ocservia_maintenance'@'%'"); err != nil {
			return err
		}
	}
	return b.GrantTelemetryTestPrivileges(ctx)
}
