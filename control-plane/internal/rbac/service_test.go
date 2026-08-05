package rbac

import (
	"slices"
	"testing"
)

func TestBaselineRoleMatrixKeepsAdministrativeFunctionsSeparate(t *testing.T) {
	for _, role := range []string{"Viewer", "Operator", "UserManager", "ConfigManager", "Auditor", "SecurityAdmin", "PlatformAdmin"} {
		if _, ok := roleActions[role]; !ok {
			t.Fatalf("missing role %s", role)
		}
	}
	if slices.Contains(roleActions["Viewer"], "session.disconnect") || slices.Contains(roleActions["Operator"], "role_binding.manage") || slices.Contains(roleActions["Auditor"], "node.revoke") {
		t.Fatal("BFLA role separation regressed")
	}
	if !slices.Contains(roleActions["SecurityAdmin"], "approval.approve") || !slices.Contains(roleActions["PlatformAdmin"], "*") {
		t.Fatal("administrative role policy is incomplete")
	}
}
