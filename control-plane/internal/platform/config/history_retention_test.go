package config

import (
	"github.com/GentleKingson/ocservia/control-plane/internal/historyretention"
	"strconv"
	"testing"
)

func TestHistoryRetentionConfiguration(t *testing.T) {
	load := func(key, value string) (Config, error) {
		return Load(nil, func(name string) (string, bool) {
			if name == "OCSERV_DATABASE_URL" {
				return "postgres://db/test", true
			}
			if name == key {
				return value, true
			}
			return "", false
		})
	}
	cfg, err := load("", "")
	if err != nil || cfg.HistoryRetention != historyretention.DefaultPolicy() {
		t.Fatal(cfg.HistoryRetention, err)
	}
	for _, item := range []struct {
		name             string
		minimum, maximum int
	}{{"OCSERV_RETIRED_NODE_RETENTION_DAYS", 30, 730}, {"OCSERV_COMMAND_DETAIL_RETENTION_DAYS", 30, 365}, {"OCSERV_AUDIT_RETENTION_DAYS", 90, 2555}} {
		for _, days := range []int{-1, 0, item.minimum - 1, item.minimum, item.maximum, item.maximum + 1} {
			_, err := load(item.name, strconv.Itoa(days))
			valid := days >= item.minimum && days <= item.maximum
			if (err == nil) != valid {
				t.Fatalf("%s=%d: %v", item.name, days, err)
			}
		}
		if _, err := load(item.name, "not-days"); err == nil {
			t.Fatal("invalid days accepted")
		}
	}
}
