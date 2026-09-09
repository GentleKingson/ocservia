package mysql

func AuthenticationTimeSteps() []LongKeyStep {
	return TimeColumnSteps("local_auth_attempts", []TimeColumn{
		{Name: "window_until"},
		{Name: "blocked_until", DefaultSQL: "-9223372036854775808", LegacyInfinity: true},
		{Name: "lease_until", DefaultSQL: "-9223372036854775808", LegacyInfinity: true},
		{Name: "expires_at"},
	}, "source.username", []string{"DROP INDEX local_auth_attempts_expiry"}, []string{"ADD KEY local_auth_attempts_expiry(expires_at)"})
}
