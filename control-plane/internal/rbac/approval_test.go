package rbac

import (
	"bytes"
	"testing"

	"github.com/google/uuid"
)

func TestHighPrivilegeBindingApprovalContentIsExactAndTypeAware(t *testing.T) {
	identityID := uuid.MustParse("01900000-0000-7000-8000-000000000201")
	workspaceID := uuid.MustParse("01900000-0000-7000-8000-000000000202")
	resourceID := uuid.MustParse("01900000-0000-7000-8000-000000000203")
	base, summary := BindingApprovalContent(identityID, workspaceID, "SecurityAdmin", "secret_ref", resourceID)
	if len(summary) == 0 {
		t.Fatal("approval summary is empty")
	}
	roleChanged, _ := BindingApprovalContent(identityID, workspaceID, "PlatformAdmin", "secret_ref", resourceID)
	typeChanged, _ := BindingApprovalContent(identityID, workspaceID, "SecurityAdmin", "resource", resourceID)
	targetChanged, _ := BindingApprovalContent(uuid.Must(uuid.NewV7()), workspaceID, "SecurityAdmin", "secret_ref", resourceID)
	if bytes.Equal(base, roleChanged) || bytes.Equal(base, typeChanged) || bytes.Equal(base, targetChanged) {
		t.Fatal("role elevation approval omitted role, resource type, or target identity")
	}
}
