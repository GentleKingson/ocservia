package auth

import (
	"context"
	"errors"

	"github.com/GentleKingson/ocservia/control-plane/internal/approvals"
	"github.com/GentleKingson/ocservia/control-plane/internal/audit"
	"github.com/GentleKingson/ocservia/control-plane/internal/authstore"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/google/uuid"
)

var (
	ErrLocalInitialized      = errors.New("Local authentication is already initialized")
	ErrLocalInvalid          = errors.New("invalid Local user request")
	ErrLocalDuplicate        = errors.New("Local username already exists")
	ErrLocalProtected        = errors.New("retain an active Local PlatformAdmin and a distinct Local approver in the management workspace")
	ErrLocalWorkspaceMissing = errors.New("management workspace does not exist; provision it through the protected administrative database connection before initialization")
)

// BootstrapLocalAdmin is an explicit one-shot, never part of normal startup.
func (s *Service) BootstrapLocalAdmin(ctx context.Context, username, password string, workspaceID uuid.UUID, approverUsername, approverPassword string) (uuid.UUID, error) {
	return s.initializeLocal(ctx, username, password, workspaceID, approverUsername, approverPassword, false)
}

// CompleteLocalBootstrap only completes an eligible pre-R4 marker; it never
// replaces a credential or grants a caller-selected role or workspace.
func (s *Service) CompleteLocalBootstrap(ctx context.Context, username, password string, workspaceID uuid.UUID, approverUsername, approverPassword string) (uuid.UUID, error) {
	return s.initializeLocal(ctx, username, password, workspaceID, approverUsername, approverPassword, true)
}

func (s *Service) initializeLocal(ctx context.Context, username, password string, workspaceID uuid.UUID, approverUsername, approverPassword string, complete bool) (uuid.UUID, error) {
	if !s.localEnabled {
		return uuid.Nil, ErrLocalDisabled
	}
	username, err := normalizeLocalUsername(username)
	if err != nil || workspaceID == uuid.Nil {
		return uuid.Nil, ErrLocalInvalid
	}
	approverUsername, err = normalizeLocalUsername(approverUsername)
	if err != nil || approverUsername == username || password == approverPassword {
		return uuid.Nil, ErrLocalInvalid
	}
	approverHash, err := hashPassword(approverPassword)
	if err != nil {
		return uuid.Nil, err
	}
	var hash string
	var credential localCredential
	if complete {
		lease, err := s.reserveLocalAttempt(ctx, username)
		if err != nil {
			return uuid.Nil, err
		}
		failed := false
		defer func() { _ = s.finishLocalAttempt(username, lease, failed) }()
		credential, err = s.localCredential(ctx, username)
		if err != nil {
			return uuid.Nil, ErrUnauthenticated
		}
		valid, err := verifyPassword(credential.passwordHash, password)
		if err != nil {
			return uuid.Nil, err
		}
		if !valid {
			failed = true
			return uuid.Nil, ErrUnauthenticated
		}
		if credential.disabled {
			return uuid.Nil, ErrUnauthenticated
		}
		credential.attemptLease = lease
	} else {
		hash, err = hashPassword(password)
		if err != nil {
			return uuid.Nil, err
		}
	}
	var id uuid.UUID
	err = s.withAuthentication(ctx, func(tx database.Tx, store authstore.Store) error {
		// Serialize even when the singleton row does not yet exist.
		if err := store.LockManagement(ctx); err != nil {
			return err
		}
		if err := store.WorkspaceExists(ctx, workspaceID); err != nil {
			if errors.Is(err, database.ErrNotFound) {
				return ErrLocalWorkspaceMissing
			}
			return err
		}
		if complete {
			bootstrap, err := store.Bootstrap(ctx)
			if err != nil {
				return err
			}
			id = bootstrap.IdentityID
			if !bootstrap.Pending || bootstrap.WorkspaceID != workspaceID || id != credential.identityID {
				return ErrLocalInitialized
			}
			if err := store.ValidatePendingCredential(ctx, id, workspaceID, credential.passwordHash); err != nil {
				return ErrLocalInitialized
			}
			cleared, err := store.ClearAttempt(ctx, username, credential.attemptLease)
			if err != nil {
				return err
			}
			if !cleared {
				return ErrUnauthenticated
			}
		} else {
			initialized, err := store.Initialized(ctx)
			if err != nil {
				return err
			}
			if initialized {
				return ErrLocalInitialized
			}
			id = uuid.Must(uuid.NewV7())
			if err := store.InsertCredential(ctx, id, username, hash, s.now()); err != nil {
				return err
			}
			now := s.now()
			if err := store.BindRole(ctx, uuid.Must(uuid.NewV7()), id, workspaceID, "PlatformAdmin", now); err != nil {
				return err
			}
			if err := store.InsertBootstrap(ctx, id, workspaceID, now); err != nil {
				return err
			}
			if err := audit.AppendChainTx(ctx, tx, audit.ChainRecord{WorkspaceID: workspaceID, ActorType: "controller", ActorID: "local-bootstrap", Action: "local_user.bootstrap", ResourceType: "local_user", ResourceID: id, RequestID: uuid.NewString(), Result: "succeeded", Reason: "initial Local administrator", At: now}); err != nil {
				return err
			}
		}
		approverID := uuid.Must(uuid.NewV7())
		if err := store.InsertCredential(ctx, approverID, approverUsername, approverHash, s.now()); err != nil {
			return err
		}
		now := s.now()
		if err := store.BindRole(ctx, uuid.Must(uuid.NewV7()), approverID, workspaceID, "SecurityAdmin", now); err != nil {
			return err
		}
		if err := store.CompleteBootstrap(ctx, approverID, now); err != nil {
			return err
		}
		return audit.AppendChainTx(ctx, tx, audit.ChainRecord{WorkspaceID: workspaceID, ActorType: "controller", ActorID: "local-bootstrap", Action: "local_user.bootstrap-approver", ResourceType: "local_user", ResourceID: approverID, RequestID: uuid.NewString(), Result: "succeeded", Reason: "independent Local SecurityAdmin initialization", At: now})
	})
	if err != nil {
		return uuid.Nil, err
	}
	return id, nil
}

// LocalManagementWorkspace fixes the authority scope; caller headers cannot
// select another workspace to gain control over shared platform identities.
func (s *Service) LocalManagementWorkspace(ctx context.Context) (uuid.UUID, error) {
	if !s.localEnabled {
		return uuid.Nil, ErrLocalDisabled
	}
	var id uuid.UUID
	err := s.withAuthentication(ctx, func(_ database.Tx, store authstore.Store) error {
		var err error
		id, err = store.ManagementWorkspace(ctx)
		return err
	})
	return id, err
}

type LocalUserMutation struct {
	IdentityID, WorkspaceID, ActorID, SessionID uuid.UUID
	ApprovalID                                  uuid.UUID
	Action, Username, Password, RequestID       string
}

// MutateLocalUser commits credential changes, revocation and audit together.
// Authorization is performed by the API using the existing RBAC service.
func (s *Service) MutateLocalUser(ctx context.Context, request LocalUserMutation) (uuid.UUID, error) {
	if !s.localEnabled {
		return uuid.Nil, ErrLocalDisabled
	}
	if request.ActorID == uuid.Nil || request.SessionID == uuid.Nil || request.RequestID == "" {
		return uuid.Nil, ErrLocalInvalid
	}
	var hash string
	var err error
	switch request.Action {
	case "create":
		request.Username, err = normalizeLocalUsername(request.Username)
		if err != nil {
			return uuid.Nil, ErrLocalInvalid
		}
	case "disable", "reset-password":
		if request.IdentityID == uuid.Nil {
			return uuid.Nil, ErrLocalInvalid
		}
	default:
		return uuid.Nil, ErrLocalInvalid
	}
	if request.Action != "disable" {
		hash, err = hashPassword(request.Password)
		if err != nil {
			return uuid.Nil, err
		}
	}
	id := request.IdentityID
	err = s.withAuthentication(ctx, func(tx database.Tx, store authstore.Store) error {
		// Serialize management mutations before locking any identity. This also
		// makes concurrent mutual disable observe the preceding committed change.
		if err := store.LockManagement(ctx); err != nil {
			return err
		}
		workspaceID, err := store.ManagementWorkspace(ctx)
		if err != nil {
			return err
		}
		if workspaceID != request.WorkspaceID {
			return ErrLocalInvalid
		}
		if err := store.LockActiveSession(ctx, request.ActorID, request.SessionID, false); err != nil {
			return ErrUnauthenticated
		}
		allowed, err := store.MayManage(ctx, request.ActorID, workspaceID, request.SessionID)
		if err != nil {
			return err
		}
		if !allowed {
			return ErrUnauthenticated
		}
		now := s.now()
		if request.Action == "create" {
			id = uuid.Must(uuid.NewV7())
			if err := store.InsertCredential(ctx, id, request.Username, hash, s.now()); err != nil {
				if errors.Is(err, database.ErrUnique) {
					return ErrLocalDuplicate
				}
				return err
			}
		} else {
			// Same lock order as createSession prevents a concurrent verified login
			// from issuing a usable session after this transaction commits.
			if err := store.LockLocalIdentity(ctx, id); err != nil {
				return err
			}
			if request.Action == "disable" {
				protected, checkErr := store.Protected(ctx, workspaceID, id)
				if checkErr != nil {
					return checkErr
				}
				if protected {
					return ErrLocalProtected
				}
				err = store.Disable(ctx, id, now)
			} else {
				// Password reset can take over existing elevated bindings. Require
				// the normal independent, one-use approval even for PlatformAdmin.
				approvalHash, _ := approvals.GenericBinding("local_user.reset-password", "local_user", id)
				if err := approvals.ConsumeBoundTx(ctx, tx, request.ApprovalID, workspaceID, request.ActorID, "local_user.reset-password", "local_user", id, approvalHash); err != nil {
					return err
				}
				err = store.SetPassword(ctx, id, hash, now)
			}
			if err != nil {
				return err
			}
			if err := store.RevokeIdentitySessions(ctx, id, now); err != nil {
				return err
			}
			// Fence old verification completions and start a fresh credential epoch.
			if err := store.DeleteIdentityAttempts(ctx, id); err != nil {
				return err
			}
		}
		var approvalID *uuid.UUID
		if request.ApprovalID != uuid.Nil {
			approvalID = &request.ApprovalID
		}
		return audit.AppendChainTx(ctx, tx, audit.ChainRecord{WorkspaceID: workspaceID, ActorType: "user", ActorID: request.ActorID.String(), SessionID: &request.SessionID, ApprovalID: approvalID, Action: "local_user." + request.Action, ResourceType: "local_user", ResourceID: id, RequestID: request.RequestID, Result: "succeeded", At: now})
	})
	if err != nil {
		return uuid.Nil, err
	}
	return id, nil
}
