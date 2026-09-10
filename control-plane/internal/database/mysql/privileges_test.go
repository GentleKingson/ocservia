package mysql

import (
	"strings"
	"testing"
)

func TestQuotedRuntimeAccount(t *testing.T) {
	for _, account := range []string{"controller@%", "controller@localhost", "controller@127.0.0.1", "controller-v2@2001:db8::1"} {
		quoted, err := quotedRuntimeAccount(account)
		user, host, _ := strings.Cut(account, "@")
		if err != nil || quoted != "'"+user+"'@'"+host+"'" {
			t.Fatalf("quote %q: %q, %v", account, quoted, err)
		}
	}
	for _, account := range []string{"", "controller", "@%", "controller@", "controller@@%", "'controller'@'%'", "controller@%; DROP TABLE nodes", "controller@host\\x", "controller@host\n", strings.Repeat("x", 33) + "@%"} {
		if _, err := quotedRuntimeAccount(account); err == nil {
			t.Fatalf("accepted runtime account %q", account)
		}
	}
}
