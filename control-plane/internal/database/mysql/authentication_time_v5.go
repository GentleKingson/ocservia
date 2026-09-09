package mysql

func AuthenticationRemainingTimeSteps(engine Engine) []LongKeyStep {
	drop := "DROP CHECK "
	if engine == MariaDB {
		drop = "DROP CONSTRAINT "
	}
	steps := TimeColumnSteps("identities", []TimeColumn{{Name: "disabled_at", Nullable: true}, {Name: "created_at"}, {Name: "updated_at"}}, "HEX(source.id)", nil, nil)
	steps = append(steps, TimeColumnSteps("auth_sessions", []TimeColumn{{Name: "expires_at"}, {Name: "revoked_at", Nullable: true}, {Name: "created_at"}}, "HEX(source.id)", []string{drop + "auth_sessions_auth_sessions_check", "DROP INDEX auth_sessions_expiry_idx"}, []string{"ADD CONSTRAINT auth_sessions_auth_sessions_check CHECK (expires_at>created_at)", "ADD KEY auth_sessions_expiry_idx(expires_at)"})...)
	steps = append(steps, TimeColumnSteps("local_credentials", []TimeColumn{{Name: "created_at"}, {Name: "updated_at"}, {Name: "password_changed_at"}}, "HEX(source.identity_id)", nil, nil)...)
	steps = append(steps, TimeColumnSteps("local_auth_bootstrap", []TimeColumn{{Name: "created_at"}}, "source.singleton", nil, nil)...)
	steps = append(steps, TimeColumnSteps("break_glass_uses", []TimeColumn{{Name: "used_at"}}, "HEX(source.credential_fingerprint)", nil, nil)...)
	steps = append(steps, TimeColumnSteps("security_alerts", []TimeColumn{{Name: "created_at"}, {Name: "acknowledged_at", Nullable: true}}, "HEX(source.id)", nil, nil)...)
	return steps
}
