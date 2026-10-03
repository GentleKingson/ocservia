package migrations

import "testing"

func TestPostgreSQLMajorVersion(t *testing.T) {
	for _, version := range []int{0, 160012, 170010, 190000} {
		if err := validatePostgreSQLVersion(version); err == nil {
			t.Fatalf("unsupported server version %d accepted", version)
		}
	}
	for _, version := range []int{180000, 180006, 180099} {
		if err := validatePostgreSQLVersion(version); err != nil {
			t.Fatal(err)
		}
	}
}

func TestPostgreSQLStableRelease(t *testing.T) {
	for _, release := range []string{"18beta1", "18rc1", "18devel"} {
		if err := validatePostgreSQLRelease(180000, release); err == nil {
			t.Fatalf("prerelease accepted: %s", release)
		}
	}
	for _, release := range []string{"18.0", "18.6 (Debian 18.6-1.pgdg13+1)"} {
		if err := validatePostgreSQLRelease(180006, release); err != nil {
			t.Fatal(err)
		}
	}
}
