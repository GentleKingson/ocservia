package releasecatalog

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func writeManifest(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "agent-releases.json")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadAcceptsExactReleaseIdentities(t *testing.T) {
	path := writeManifest(t, `{"releases":[
		{"version":"0.2.0","architecture":"amd64","package_sha256":"`+digestHex("a")+`"},
		{"version":"0.2.0","architecture":"arm64","package_sha256":"`+digestHex("b")+`"}
	]}`)
	catalog, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	digest, ok := catalog.Lookup("0.2.0", "amd64")
	if !ok || digest != digestValue("a") {
		t.Fatalf("amd64 lookup mismatch: %v %v", digest, ok)
	}
	if _, ok := catalog.Lookup("0.2.0", "arm64"); !ok {
		t.Fatal("arm64 release missing")
	}
	if _, ok := catalog.Lookup("0.3.0", "amd64"); ok {
		t.Fatal("unreleased version must not resolve")
	}
	if _, ok := catalog.Lookup("0.2.0", "riscv64"); ok {
		t.Fatal("unknown architecture must not resolve")
	}
}

func TestLoadFailsClosed(t *testing.T) {
	cases := map[string]string{
		"missing file":    "",
		"empty releases":  `{"releases":[]}`,
		"unknown field":   `{"releases":[{"version":"0.2.0","architecture":"amd64","package_sha256":"` + digestHex("a") + `"}],"extra":1}`,
		"bad version":     `{"releases":[{"version":"latest","architecture":"amd64","package_sha256":"` + digestHex("a") + `"}]}`,
		"bad arch":        `{"releases":[{"version":"0.2.0","architecture":"x86","package_sha256":"` + digestHex("a") + `"}]}`,
		"short digest":    `{"releases":[{"version":"0.2.0","architecture":"amd64","package_sha256":"abcd"}]}`,
		"uppercase hash":  `{"releases":[{"version":"0.2.0","architecture":"amd64","package_sha256":"` + digestHexUpper("a") + `"}]}`,
		"duplicate pair":  `{"releases":[{"version":"0.2.0","architecture":"amd64","package_sha256":"` + digestHex("a") + `"},{"version":"0.2.0","architecture":"amd64","package_sha256":"` + digestHex("b") + `"}]}`,
		"trailing rocket": `{"releases":[{"version":"0.2.0","architecture":"amd64","package_sha256":"` + digestHex("a") + `"}]} trailing`,
	}
	for name, contents := range cases {
		t.Run(name, func(t *testing.T) {
			path := writeManifest(t, contents)
			if name == "missing file" {
				path = filepath.Join(t.TempDir(), "absent.json")
			}
			catalog, err := Load(path)
			if !errors.Is(err, ErrInvalidManifest) || catalog != nil {
				t.Fatalf("expected fail-closed load, got %v %v", catalog, err)
			}
		})
	}
}

func digestHex(seed string) string {
	return hexOf(digestValue(seed))
}

func digestHexUpper(seed string) string {
	value := digestHex(seed)
	upper := []byte(value)
	for index := 0; index < len(upper); index++ {
		if upper[index] >= 'a' && upper[index] <= 'f' {
			upper[index] -= 'a' - 'A'
		}
	}
	return string(upper)
}

func digestValue(seed string) [32]byte {
	var digest [32]byte
	for index := range digest {
		digest[index] = seed[0] + byte(index%16)
	}
	return digest
}

func hexOf(digest [32]byte) string {
	const table = "0123456789abcdef"
	out := make([]byte, 0, 64)
	for _, value := range digest {
		out = append(out, table[value>>4], table[value&0x0f])
	}
	return string(out)
}
