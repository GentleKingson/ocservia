package telemetry

import (
	"context"

	"github.com/GentleKingson/ocservia/control-plane/internal/configprofile"
	"github.com/GentleKingson/ocservia/control-plane/internal/enrollment/store"
	"github.com/google/uuid"
)

// ActionAvailability is an advisory read model. Writes recheck authorization,
// capabilities, revisions, approvals and command-specific constraints.
type ActionAvailability struct {
	Allowed bool   `json:"allowed"`
	Reason  string `json:"reason"`
}

var actionCapabilities = map[string]string{
	"user.manage":                    "ocserv.users.write",
	"group.manage":                   "ocserv.groups.write",
	"config.plan":                    configprofile.PlanCapability,
	"config.apply":                   configprofile.ApplyCapability,
	"certificate.read":               "",
	"certificate.issue":              "ocserv.certificate.issue",
	"certificate.revoke":             "ocserv.certificate.revoke",
	"certificate.private_key.export": "ocserv.certificate.issue",
	"service.reload":                 "ocserv.service.reload",
}

func nodeActions(ctx context.Context, enrollment store.EnrollmentStore, id uuid.UUID, status string) (map[string]ActionAvailability, error) {
	available := status == "active" || status == "offline"
	var approved map[string]bool
	if available {
		capabilities, err := enrollment.Capabilities(ctx, id, true)
		if err != nil {
			return nil, err
		}
		approved = make(map[string]bool, len(capabilities))
		for _, capability := range capabilities {
			approved[capability] = true
		}
	}
	result := make(map[string]ActionAvailability, len(actionCapabilities))
	for action, capability := range actionCapabilities {
		switch {
		case capability == "" || approved[capability]:
			result[action] = ActionAvailability{Allowed: true, Reason: "available"}
		case !available:
			result[action] = ActionAvailability{Reason: "node_unavailable"}
		default:
			result[action] = ActionAvailability{Reason: "missing_capability"}
		}
	}
	return result, nil
}
