package eventstream

import (
	"errors"
	"fmt"
	"sync"
	"testing"
)

func TestAdmissionLimitsAndCleanup(t *testing.T) {
	config := DefaultConfig()
	config.GlobalStreams = 5
	config.IdentityStreams = 4
	config.SessionStreams = 3
	config.WorkspaceStreams = 2
	config.ResourceStreams = 1
	config.Watchers = 5
	manager, err := NewManager(config)
	if err != nil {
		t.Fatal(err)
	}
	key := AdmissionKey{Identity: "identity-a", Session: "session-a", Workspace: "workspace-a", Resource: "resource-a"}
	lease, err := manager.Acquire(key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Acquire(key); !errors.Is(err, ErrResourceLimit) {
		t.Fatalf("second resource stream error = %v", err)
	}
	lease.Release()
	lease.Release()
	if snapshot := manager.Snapshot(); snapshot.Active != 0 || snapshot.IdentityEntries != 0 || snapshot.SessionEntries != 0 || snapshot.WorkspaceEntries != 0 || snapshot.ResourceEntries != 0 {
		t.Fatalf("admission state leaked after release: %+v", snapshot)
	}
}

func TestAdmissionGlobalAndIdentityAreIndependent(t *testing.T) {
	config := DefaultConfig()
	config.GlobalStreams = 3
	config.IdentityStreams = 2
	config.SessionStreams = 2
	config.WorkspaceStreams = 3
	config.ResourceStreams = 3
	config.Watchers = 3
	manager, err := NewManager(config)
	if err != nil {
		t.Fatal(err)
	}
	var leases []*Lease
	for index := 0; index < 2; index++ {
		lease, err := manager.Acquire(AdmissionKey{Identity: "identity-a", Session: fmt.Sprintf("session-%d", index), Workspace: fmt.Sprintf("workspace-%d", index), Resource: fmt.Sprintf("resource-%d", index)})
		if err != nil {
			t.Fatal(err)
		}
		leases = append(leases, lease)
	}
	if _, err := manager.Acquire(AdmissionKey{Identity: "identity-a", Session: "session-new", Workspace: "workspace-new", Resource: "resource-new"}); !errors.Is(err, ErrIdentityLimit) {
		t.Fatalf("identity limit error = %v", err)
	}
	third, err := manager.Acquire(AdmissionKey{Identity: "identity-b", Session: "session-b", Workspace: "workspace-b", Resource: "resource-b"})
	if err != nil {
		t.Fatal(err)
	}
	leases = append(leases, third)
	if _, err := manager.Acquire(AdmissionKey{Identity: "identity-c", Session: "session-c", Workspace: "workspace-c", Resource: "resource-c"}); !errors.Is(err, ErrGlobalLimit) {
		t.Fatalf("global limit error = %v", err)
	}
	for _, lease := range leases {
		lease.Release()
	}
}

func TestAdmissionConcurrentReleaseDoesNotLeak(t *testing.T) {
	config := DefaultConfig()
	manager, err := NewManager(config)
	if err != nil {
		t.Fatal(err)
	}
	var wait sync.WaitGroup
	for index := 0; index < config.GlobalStreams; index++ {
		key := AdmissionKey{
			Identity: fmt.Sprintf("identity-%d", index), Session: fmt.Sprintf("session-%d", index),
			Workspace: fmt.Sprintf("workspace-%d", index), Resource: fmt.Sprintf("resource-%d", index),
		}
		lease, err := manager.Acquire(key)
		if err != nil {
			t.Fatal(err)
		}
		wait.Add(1)
		go func() {
			defer wait.Done()
			lease.Release()
			lease.Release()
		}()
	}
	wait.Wait()
	if snapshot := manager.Snapshot(); snapshot.Active != 0 || snapshot.IdentityEntries+snapshot.SessionEntries+snapshot.WorkspaceEntries+snapshot.ResourceEntries != 0 {
		t.Fatalf("concurrent release leaked state: %+v", snapshot)
	}
}

func TestWorkspaceAndResourceLimitsAreIndependent(t *testing.T) {
	config := DefaultConfig()
	config.GlobalStreams = 8
	config.IdentityStreams = 8
	config.SessionStreams = 8
	config.WorkspaceStreams = 2
	config.ResourceStreams = 1
	config.Watchers = 8
	manager, err := NewManager(config)
	if err != nil {
		t.Fatal(err)
	}
	first, err := manager.Acquire(AdmissionKey{Identity: "identity-a", Session: "session-a", Workspace: "workspace-a", Resource: "operation-a"})
	if err != nil {
		t.Fatal(err)
	}
	defer first.Release()
	if _, err := manager.Acquire(AdmissionKey{Identity: "identity-b", Session: "session-b", Workspace: "workspace-b", Resource: "operation-a"}); !errors.Is(err, ErrResourceLimit) {
		t.Fatalf("shared operation resource error = %v", err)
	}
	second, err := manager.Acquire(AdmissionKey{Identity: "identity-b", Session: "session-b", Workspace: "workspace-a", Resource: "operation-b"})
	if err != nil {
		t.Fatal(err)
	}
	defer second.Release()
	if _, err := manager.Acquire(AdmissionKey{Identity: "identity-c", Session: "session-c", Workspace: "workspace-a", Resource: "operation-c"}); !errors.Is(err, ErrWorkspaceLimit) {
		t.Fatalf("workspace aggregate error = %v", err)
	}
}
