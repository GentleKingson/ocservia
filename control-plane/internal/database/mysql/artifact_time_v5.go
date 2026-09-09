package mysql

// ArtifactTimeSteps preserves the download transaction's nullable timestamps
// and expiry ordering without imposing MySQL's calendar range on the domain.
func ArtifactTimeSteps(engine Engine) []LongKeyStep {
	dropCheck := "DROP CHECK "
	if engine == MariaDB {
		dropCheck = "DROP CONSTRAINT "
	}
	steps := TimeColumnSteps("artifact_operations", []TimeColumn{
		{Name: "expires_at"}, {Name: "lease_until", Nullable: true},
		{Name: "consumed_at", Nullable: true}, {Name: "created_at"},
		{Name: "updated_at"}, {Name: "active_grant_expires_at", Nullable: true},
	}, "source.id", []string{
		"DROP INDEX artifact_operations_expiry_idx",
		dropCheck + "artifact_operations_artifact_operations_grant_check",
	}, []string{
		"ADD KEY artifact_operations_expiry_idx(expires_at)",
		"ADD CONSTRAINT artifact_operations_artifact_operations_grant_check CHECK ((state='leased' AND active_grant_id IS NOT NULL AND active_grant_subject IS NOT NULL AND active_grant_expires_at IS NOT NULL) OR state<>'leased')",
	})
	return append(steps, TimeColumnSteps("certificates", []TimeColumn{{Name: "not_after", Nullable: true}}, "source.id",
		[]string{"DROP INDEX certificates_node_expiry_idx"}, []string{"ADD KEY certificates_node_expiry_idx(node_id,not_after)"})...)
}
