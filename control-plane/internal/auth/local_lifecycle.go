package auth

import (
	"context"
	"errors"

	"github.com/GentleKingson/ocservia/control-plane/internal/approvals"
	"github.com/GentleKingson/ocservia/control-plane/internal/audit"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
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
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return uuid.Nil, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	// Serialize even when the singleton row does not yet exist.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(734821032)`); err != nil {
		return uuid.Nil, err
	}
	var existingWorkspace uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT id FROM workspaces WHERE id=$1 FOR KEY SHARE`, workspaceID).Scan(&existingWorkspace); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return uuid.Nil, ErrLocalWorkspaceMissing
		}
		return uuid.Nil, err
	}
	var id uuid.UUID
	if complete {
		var pending bool
		var fixedWorkspace uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT identity_id,workspace_id,completion_pending FROM local_auth_bootstrap WHERE singleton FOR UPDATE`).Scan(&id, &fixedWorkspace, &pending); err != nil {
			return uuid.Nil, err
		}
		if !pending || fixedWorkspace != workspaceID || id != credential.identityID {
			return uuid.Nil, ErrLocalInitialized
		}
		if err := tx.QueryRow(ctx, `SELECT i.id FROM identities i JOIN local_credentials c ON c.identity_id=i.id
			WHERE i.id=$1 AND i.issuer='local' AND i.subject=c.username AND i.disabled_at IS NULL AND c.password_hash=$2
			AND EXISTS(SELECT 1 FROM role_bindings WHERE identity_id=i.id AND workspace_id=$3 AND resource_type='workspace' AND role_name='PlatformAdmin')
			AND NOT EXISTS(SELECT 1 FROM role_bindings WHERE workspace_id=$3 AND identity_id<>i.id AND role_name IN ('SecurityAdmin','PlatformAdmin'))
			FOR UPDATE OF i,c`, id, credential.passwordHash, workspaceID).Scan(&id); err != nil {
			return uuid.Nil, ErrLocalInitialized
		}
		if err := clearLocalAttempt(ctx, tx, username, credential.attemptLease); err != nil {
			return uuid.Nil, err
		}
	} else {
		var initialized bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM local_auth_bootstrap) OR EXISTS(SELECT 1 FROM identities i JOIN role_bindings b ON b.identity_id=i.id WHERE i.issuer='local' AND b.role_name IN ('SecurityAdmin','PlatformAdmin'))`).Scan(&initialized); err != nil {
			return uuid.Nil, err
		}
		if initialized {
			return uuid.Nil, ErrLocalInitialized
		}
		id, err = s.insertLocalCredential(ctx, tx, username, hash)
		if err != nil {
			return uuid.Nil, err
		}
		now := s.now()
		if _, err := tx.Exec(ctx, `INSERT INTO role_bindings(id,identity_id,workspace_id,role_name,resource_type,created_at) VALUES($1,$2,$3,'PlatformAdmin','workspace',$4)`, uuid.Must(uuid.NewV7()), id, workspaceID, now); err != nil {
			return uuid.Nil, err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO local_auth_bootstrap(singleton,identity_id,workspace_id,created_at,completed_at) VALUES(true,$1,$2,$3,$3)`, id, workspaceID, now); err != nil {
			return uuid.Nil, err
		}
		if err := audit.AppendChain(ctx, tx, audit.ChainRecord{WorkspaceID: workspaceID, ActorType: "controller", ActorID: "local-bootstrap", Action: "local_user.bootstrap", ResourceType: "local_user", ResourceID: id, RequestID: uuid.NewString(), Result: "succeeded", Reason: "initial Local administrator", At: now}); err != nil {
			return uuid.Nil, err
		}
	}
	approverID, err := s.insertLocalCredential(ctx, tx, approverUsername, approverHash)
	if err != nil {
		return uuid.Nil, err
	}
	now := s.now()
	if _, err := tx.Exec(ctx, `INSERT INTO role_bindings(id,identity_id,workspace_id,role_name,resource_type,created_at) VALUES($1,$2,$3,'SecurityAdmin','workspace',$4)`, uuid.Must(uuid.NewV7()), approverID, workspaceID, now); err != nil {
		return uuid.Nil, err
	}
	if _, err := tx.Exec(ctx, `UPDATE local_auth_bootstrap SET completion_pending=false,completed_at=$1,approver_identity_id=$2 WHERE singleton`, now, approverID); err != nil {
		return uuid.Nil, err
	}
	if err := audit.AppendChain(ctx, tx, audit.ChainRecord{WorkspaceID: workspaceID, ActorType: "controller", ActorID: "local-bootstrap", Action: "local_user.bootstrap-approver", ResourceType: "local_user", ResourceID: approverID, RequestID: uuid.NewString(), Result: "succeeded", Reason: "independent Local SecurityAdmin initialization", At: now}); err != nil {
		return uuid.Nil, err
	}
	if err := tx.Commit(ctx); err != nil {
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
	err := s.pool.QueryRow(ctx, `SELECT workspace_id FROM local_auth_bootstrap WHERE singleton`).Scan(&id)
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
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return uuid.Nil, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	// Serialize management mutations before locking any identity. This also
	// makes concurrent mutual disable observe the preceding committed change.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(734821032)`); err != nil {
		return uuid.Nil, err
	}
	var workspaceID uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT workspace_id FROM local_auth_bootstrap WHERE singleton`).Scan(&workspaceID); err != nil {
		return uuid.Nil, err
	}
	if workspaceID != request.WorkspaceID {
		return uuid.Nil, ErrLocalInvalid
	}
	if err := lockActiveSession(ctx, tx, request.ActorID, request.SessionID, false); err != nil {
		return uuid.Nil, err
	}
	var allowed bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM role_bindings WHERE identity_id=$1 AND workspace_id=$2 AND role_name='PlatformAdmin' AND resource_type='workspace') OR EXISTS(SELECT 1 FROM auth_sessions WHERE id=$3 AND break_glass)`, request.ActorID, workspaceID, request.SessionID).Scan(&allowed); err != nil {
		return uuid.Nil, err
	}
	if !allowed {
		return uuid.Nil, ErrUnauthenticated
	}
	id := request.IdentityID
	now := s.now()
	if request.Action == "create" {
		id, err = s.insertLocalCredential(ctx, tx, request.Username, hash)
		if err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "23505" {
				return uuid.Nil, ErrLocalDuplicate
			}
			return uuid.Nil, err
		}
	} else {
		// Same lock order as createSession prevents a concurrent verified login
		// from issuing a usable session after this transaction commits.
		if err := tx.QueryRow(ctx, `SELECT i.id FROM identities i JOIN local_credentials c ON c.identity_id=i.id WHERE i.id=$1 AND i.issuer='local' AND i.subject=c.username FOR UPDATE OF i,c`, id).Scan(&id); err != nil {
			return uuid.Nil, err
		}
		if request.Action == "disable" {
			var protected bool
			if err := tx.QueryRow(ctx, `WITH effective AS (
				SELECT DISTINCT i.id,b.role_name FROM identities i JOIN local_credentials c ON c.identity_id=i.id JOIN role_bindings b ON b.identity_id=i.id
				WHERE i.disabled_at IS NULL AND i.issuer='local' AND i.subject=c.username AND b.workspace_id=$1 AND b.resource_type='workspace' AND b.role_name IN ('PlatformAdmin','SecurityAdmin')
			) SELECT EXISTS(SELECT 1 FROM effective WHERE id=$2) AND (
				NOT EXISTS(SELECT 1 FROM effective WHERE id<>$2 AND role_name='PlatformAdmin') OR
				(SELECT count(DISTINCT id) FROM effective WHERE id<>$2)<2)`, workspaceID, id).Scan(&protected); err != nil {
				return uuid.Nil, err
			}
			if protected {
				return uuid.Nil, ErrLocalProtected
			}
			_, err = tx.Exec(ctx, `UPDATE identities SET disabled_at=COALESCE(disabled_at,$2),updated_at=$2 WHERE id=$1`, id, now)
		} else {
			// Password reset can take over existing elevated bindings. Require
			// the normal independent, one-use approval even for PlatformAdmin.
			approvalHash, _ := approvals.GenericBinding("local_user.reset-password", "local_user", id)
			if err := approvals.ConsumeBound(ctx, tx, request.ApprovalID, workspaceID, request.ActorID, "local_user.reset-password", "local_user", id, approvalHash); err != nil {
				return uuid.Nil, err
			}
			_, err = tx.Exec(ctx, `UPDATE local_credentials SET password_hash=$2,password_changed_at=$3,updated_at=$3 WHERE identity_id=$1`, id, hash, now)
		}
		if err != nil {
			return uuid.Nil, err
		}
		if _, err := tx.Exec(ctx, `UPDATE auth_sessions SET revoked_at=$2 WHERE identity_id=$1 AND revoked_at IS NULL`, id, now); err != nil {
			return uuid.Nil, err
		}
		// Fence old verification completions and start a fresh credential epoch.
		if _, err := tx.Exec(ctx, `DELETE FROM local_auth_attempts WHERE username=(SELECT username FROM local_credentials WHERE identity_id=$1)`, id); err != nil {
			return uuid.Nil, err
		}
	}
	var approvalID *uuid.UUID
	if request.ApprovalID != uuid.Nil {
		approvalID = &request.ApprovalID
	}
	if err := audit.AppendChain(ctx, tx, audit.ChainRecord{WorkspaceID: workspaceID, ActorType: "user", ActorID: request.ActorID.String(), SessionID: &request.SessionID, ApprovalID: approvalID, Action: "local_user." + request.Action, ResourceType: "local_user", ResourceID: id, RequestID: request.RequestID, Result: "succeeded", At: now}); err != nil {
		return uuid.Nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return uuid.Nil, err
	}
	return id, nil
}

// Identity first, then session: the same order as login and password mutation.
func lockActiveSession(ctx context.Context, tx pgx.Tx, identityID, sessionID uuid.UUID, localOnly bool) error {
	var id uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT id FROM identities WHERE id=$1 AND disabled_at IS NULL AND (NOT $2 OR issuer='local') FOR UPDATE`, identityID, localOnly).Scan(&id); err != nil {
		return ErrUnauthenticated
	}
	if err := tx.QueryRow(ctx, `SELECT id FROM auth_sessions WHERE id=$1 AND identity_id=$2 AND revoked_at IS NULL AND expires_at>clock_timestamp() AND (NOT $3 OR NOT break_glass) FOR UPDATE`, sessionID, identityID, localOnly).Scan(&id); err != nil {
		return ErrUnauthenticated
	}
	return nil
}
