package telemetry

import (
	"context"

	"github.com/GentleKingson/ocservia/control-plane/internal/configprofile"
	"github.com/GentleKingson/ocservia/control-plane/internal/operations/store"
	"github.com/google/uuid"
)

// ActionAvailability is an advisory read model. Writes recheck authorization,
// capabilities, revisions, approvals and command-specific constraints.
type ActionAvailability struct {
	Allowed bool   `json:"allowed"`
	Reason  string `json:"reason"`
}

func nodeActions(ctx context.Context, operations store.Store, id uuid.UUID, status string) (map[string]ActionAvailability, error) {
	capabilities := map[string][]string{
		"user.manage":                    {"ocserv.users.write"},
		"group.manage":                   {"ocserv.groups.write"},
		"config.plan":                    {"ocserv.config.plan", configprofile.PlanCapability},
		"config.apply":                   {"ocserv.config.apply", configprofile.ApplyCapability},
		"certificate.read":               {},
		"certificate.issue":              {"ocserv.certificate.issue"},
		"certificate.revoke":             {"ocserv.certificate.revoke"},
		"certificate.private_key.export": {"ocserv.certificate.issue"},
		"service.reload":                 {"ocserv.service.reload"},
	}
	result := make(map[string]ActionAvailability, len(capabilities))
	for action, options := range capabilities {
		if status != "active" && status != "offline" && len(options) > 0 {
			result[action] = ActionAvailability{Reason: "node_unavailable"}
			continue
		}
		allowed := len(options) == 0
		for _, capability := range options {
			approved, err := operations.HasCapability(ctx, id, capability)
			if err != nil {
				return nil, err
			}
			allowed = allowed || approved
		}
		reason := "missing_capability"
		if allowed {
			reason = "available"
		}
		result[action] = ActionAvailability{Allowed: allowed, Reason: reason}
	}
	return result, nil
}
