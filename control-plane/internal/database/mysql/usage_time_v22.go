package mysql

func UsageTimeSteps() []LongKeyStep {
	steps := TimeColumnSteps("user_usage_cursors", []TimeColumn{{Name: "connected_at"}, {Name: "observed_at"}},
		"CONCAT(HEX(source.node_id),':',HEX(source.session_id),':',source.connected_at)",
		[]string{"DROP PRIMARY KEY"}, []string{"ADD PRIMARY KEY(node_id,session_id,connected_at)"})
	return append(steps, TimeColumnSteps("observed_user_usage", []TimeColumn{{Name: "period_start"}, {Name: "observed_at"}},
		"CONCAT(HEX(source.node_id),':',HEX(source.username),':',HEX(source.period),':',source.period_start)",
		[]string{"DROP PRIMARY KEY"}, []string{"ADD PRIMARY KEY(node_id,username,period,period_start)"})...)
}
