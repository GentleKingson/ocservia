package mysql

import (
	"context"

	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	operationstore "github.com/GentleKingson/ocservia/control-plane/internal/operations/store"
	"github.com/google/uuid"
)

func (s operationStore) LockNode(ctx context.Context, id uuid.UUID) (v operationstore.NodeState, err error) {
	err = s.QueryRow(ctx, `SELECT workspace_id,version,authorization_revision,status FROM nodes WHERE id=? FOR UPDATE`, UUIDBytes(id)).Scan(&v.WorkspaceID, &v.Version, &v.AuthorizationRevision, &v.Status)
	return
}

func (s operationStore) HasCapability(ctx context.Context, node uuid.UUID, capability string) (v bool, err error) {
	err = s.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM node_capabilities WHERE node_id=? AND BINARY capability=? AND approved=true)`, UUIDBytes(node), capability).Scan(&v)
	return
}

func (s operationStore) AttestationReady(ctx context.Context, node uuid.UUID) (v bool, err error) {
	now, err := database.TransactionTime(ctx, s.Tx)
	if err != nil {
		return false, err
	}
	err = s.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM node_capabilities WHERE node_id=? AND capability='privd_result_attestation_v1' AND approved=true)
		AND EXISTS(SELECT 1 FROM node_privd_attestation_keys WHERE node_id=? AND state='active' AND (valid_until IS NULL OR valid_until>?))`, UUIDBytes(node), UUIDBytes(node), now).Scan(&v)
	return
}

func (s operationStore) HasSession(ctx context.Context, node uuid.UUID, session, boot string) (v bool, err error) {
	err = s.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM node_sessions s JOIN node_observed_snapshots o ON o.node_id=s.node_id WHERE s.node_id=? AND BINARY s.session_id=? AND BINARY o.boot_id=?)`, UUIDBytes(node), session, boot).Scan(&v)
	return
}

func (s operationStore) HasIPBan(ctx context.Context, node uuid.UUID, ip string) (v bool, err error) {
	encoded, err := telemetryIP(ip)
	if err != nil {
		return false, err
	}
	err = s.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM node_ip_bans WHERE node_id=? AND ip=?)`, UUIDBytes(node), encoded).Scan(&v)
	return
}
