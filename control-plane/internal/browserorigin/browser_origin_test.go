package browserorigin

import "testing"

func TestNormalize(t *testing.T) {
	for _, test := range []struct {
		value string
		want  string
		ok    bool
	}{
		{"https://admin.example.com", "https://admin.example.com", true},
		{" https://admin.example.com ", "https://admin.example.com", true},
		{"https://admin.example.com/", "https://admin.example.com", true},
		{"HTTPS://Admin.Example.COM", "https://admin.example.com", true},
		{"https://admin.example.com:443", "https://admin.example.com", true},
		{"http://admin.example.com:80", "http://admin.example.com", true},
		{"https://admin.example.com:8443", "https://admin.example.com:8443", true},
		{"https://admin.example.com/callback", "", false},
		{"https://admin.example.com?redirect=1", "", false},
		{"https://admin.example.com#fragment", "", false},
		{"https://user@admin.example.com", "", false},
		{"ftp://admin.example.com", "", false},
		{"admin.example.com", "", false},
		{"", "", false},
	} {
		got, ok := Normalize(test.value)
		if got != test.want || ok != test.ok {
			t.Fatalf("Normalize(%q) = %q,%v; want %q,%v", test.value, got, ok, test.want, test.ok)
		}
	}
}
