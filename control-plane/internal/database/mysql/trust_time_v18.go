package mysql

func TrustConvergenceTimeSteps(engine Engine) []LongKeyStep {
	drop := "DROP CHECK "
	if engine == MariaDB {
		drop = "DROP CONSTRAINT "
	}
	return TimeColumnSteps("node_trust_convergence", []TimeColumn{{Name: "available_at"}, {Name: "locked_until", Nullable: true}, {Name: "created_at"}, {Name: "updated_at"}}, "HEX(source.node_id)",
		[]string{"DROP INDEX node_trust_convergence_pending_idx", drop + "node_trust_convergence_node_trust_convergence_check"},
		[]string{"ADD KEY node_trust_convergence_pending_idx(available_at,node_id)", "ADD CONSTRAINT node_trust_convergence_node_trust_convergence_check CHECK ((locked_by IS NULL)=(locked_until IS NULL))"})
}
