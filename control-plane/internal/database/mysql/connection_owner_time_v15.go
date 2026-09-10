package mysql

func ConnectionOwnerTimeSteps() []LongKeyStep {
	return TimeColumnSteps("connection_owner_fencing", []TimeColumn{{Name: "lease_until"}, {Name: "updated_at", DefaultSQL: "(TIMESTAMPDIFF(MICROSECOND,'2000-01-01',CURRENT_TIMESTAMP(6)))"}}, "HEX(source.node_id)", nil, nil)
}
