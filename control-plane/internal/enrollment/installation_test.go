package enrollment

import (
	"encoding/base64"
	"strings"
	"testing"
)

func TestInstallationTokenPurposeAndOriginBinding(t *testing.T) {
	origin := "https://controller.example"
	root := installTokenPrefix + base64.RawURLEncoding.EncodeToString([]byte(origin)) + "." + base64.RawURLEncoding.EncodeToString([]byte(strings.Repeat("s", 32)))
	child, ok := installationChild(root, origin)
	if !ok || !strings.HasPrefix(child, installEnrollmentPrefix) {
		t.Fatal("root token did not derive enrollment authority")
	}
	if _, ok := bootstrapTokenDigest(child); !ok {
		t.Fatal("derived enrollment token is malformed")
	}
	for _, token := range []string{child, strings.Replace(root, "oit1_", "obt1_", 1), root + "=", root + ".extra", root[:len(root)-1]} {
		if _, ok := installationChild(token, origin); ok {
			t.Fatal("invalid or child credential accepted as root")
		}
	}
	if _, ok := installationChild(root, "https://other.example"); ok {
		t.Fatal("token accepted for another Controller origin")
	}
	for _, origin := range []string{"http://controller.example", "https://user@controller.example", "https://controller.example/path", "https://controller.example?x=1", "https://controller.example#x"} {
		if validHTTPSOrigin(origin) {
			t.Fatalf("unsafe origin accepted: %s", origin)
		}
	}
}
