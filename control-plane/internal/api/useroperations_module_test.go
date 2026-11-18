package api

import (
	"context"
	"net/http"
	"testing"

	"github.com/GentleKingson/ocservia/control-plane/internal/useroperations"
	"github.com/google/uuid"
)

type blockedUserOperations struct {
	*blockedRequest
}

func (p *blockedUserOperations) GetPolicy(ctx context.Context, _ uuid.UUID, _ string) (useroperations.Policy, error) {
	return useroperations.Policy{}, p.wait(ctx)
}
func (p *blockedUserOperations) SetPolicy(ctx context.Context, _ useroperations.PolicyRequest) (useroperations.Policy, bool, error) {
	return useroperations.Policy{}, false, p.wait(ctx)
}

func TestUserOperationsHTTPContextLifetime(t *testing.T) {
	for _, route := range []struct{ method, path, body string }{
		{"PUT", "/api/v1/nodes/" + baselineID + "/users/alice/policy", `{}`},
		{"GET", "/api/v1/nodes/" + baselineID + "/users/alice/policy", ""},
	} {
		checkRequestLifetime(t, route.method+route.path+"/", lifetimeJSONRequest(route.method, route.path, route.body), func(b *blockedRequest) Modules {
			return Modules{UserOperations: &blockedUserOperations{blockedRequest: b}}
		}, Authorization{})
	}
}
