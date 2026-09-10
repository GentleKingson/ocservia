package postgres

import (
	"context"

	configurationstore "github.com/GentleKingson/ocservia/control-plane/internal/configplan/store"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	"github.com/google/uuid"
)

type configurationStore struct{ database.Tx }

func (t *transaction) ConfigurationStore() configurationstore.Store { return configurationStore{t} }

func (s configurationStore) Node(ctx context.Context, id uuid.UUID) (v configurationstore.Node, err error) {
	err = s.QueryRow(ctx, `SELECT n.workspace_id,n.version,COALESCE((SELECT o.ocserv_version FROM node_observed_snapshots o WHERE o.node_id=n.id),'') FROM nodes n WHERE n.id=$1 AND n.status IN('active','offline')`, id).Scan(&v.WorkspaceID, &v.Version, &v.OcservVersion)
	return
}

func (s configurationStore) Capabilities(ctx context.Context, id uuid.UUID) ([]string, error) {
	rows, err := s.Query(ctx, `SELECT capability FROM node_capabilities WHERE node_id=$1 AND approved=true AND (capability='ocserv.config.plan' OR capability LIKE 'config.%') ORDER BY capability`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var values []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		values = append(values, v)
	}
	return values, rows.Err()
}

func (s configurationStore) Get(ctx context.Context, id uuid.UUID) (v configurationstore.Plan, err error) {
	err = s.QueryRow(ctx, `SELECT p.id,p.workspace_id,p.node_id,p.operation_id,p.template_name,p.expected_revision,p.candidate_hash,
		o.state,p.candidate_redacted,p.warnings,p.expires_at,p.created_at,
		COALESCE((SELECT r.result FROM agent_command_results r WHERE r.command_id=o.command_id AND r.receipt_verification_status='verified' ORDER BY r.created_at DESC LIMIT 1),''::bytea),a.id,COALESCE(a.status,'')
		FROM config_plans p JOIN operations o ON o.id=p.operation_id
		LEFT JOIN LATERAL (SELECT id,status FROM approval_requests WHERE resource_type='config_plan' AND resource_id=p.id AND action='config.apply' ORDER BY created_at DESC LIMIT 1) a ON true WHERE p.id=$1`, id).
		Scan(&v.ID, &v.WorkspaceID, &v.NodeID, &v.OperationID, &v.TemplateName, &v.ExpectedRevision, &v.CandidateHash, &v.State, &v.CandidateRedacted, &v.Warnings, &v.ExpiresAt, &v.CreatedAt, &v.Result, &v.ApprovalID, &v.ApprovalStatus)
	return
}

func (s configurationStore) ApplyInput(ctx context.Context, id uuid.UUID) (v configurationstore.ApplyInput, err error) {
	err = s.QueryRow(ctx, `SELECT n.version,COALESCE(state.desired_revision,0),c.envelope FROM nodes n JOIN config_plans p ON p.node_id=n.id JOIN commands c ON c.operation_id=p.operation_id LEFT JOIN node_config_state state ON state.node_id=n.id WHERE p.id=$1`, id).Scan(&v.NodeVersion, &v.DesiredRevision, &v.Envelope)
	return
}

func (s configurationStore) Resource(ctx context.Context, id uuid.UUID) (workspace, node uuid.UUID, err error) {
	err = s.QueryRow(ctx, `SELECT workspace_id,node_id FROM config_plans WHERE id=$1`, id).Scan(&workspace, &node)
	return
}

func (s configurationStore) State(ctx context.Context, node uuid.UUID) (v configurationstore.State, err error) {
	err = s.QueryRow(ctx, `SELECT COALESCE((SELECT revision FROM node_config_state WHERE node_id=$1),0),COALESCE((SELECT desired_revision FROM node_config_state WHERE node_id=$1),0),COALESCE((SELECT automation_locked FROM node_config_state WHERE node_id=$1),false),COALESCE((SELECT ocserv_version FROM node_observed_snapshots WHERE node_id=$1),'')`, node).Scan(&v.Revision, &v.DesiredRevision, &v.Locked, &v.OcservVersion)
	return
}

func (s configurationStore) Proof(ctx context.Context, id uuid.UUID) (v configurationstore.Proof, err error) {
	err = s.QueryRow(ctx, `SELECT p.workspace_id,p.node_id,p.expected_revision,p.candidate_hash,p.expires_at,o.state,
		COALESCE((SELECT r.result FROM agent_command_results r WHERE r.command_id=o.command_id AND r.state='succeeded' AND r.receipt_verification_status='verified' ORDER BY r.created_at DESC LIMIT 1),''::bytea)
		FROM config_plans p JOIN operations o ON o.id=p.operation_id WHERE p.id=$1`, id).Scan(&v.WorkspaceID, &v.NodeID, &v.ExpectedRevision, &v.CandidateHash, &v.ExpiresAt, &v.State, &v.Result)
	return
}

func (s configurationStore) HasActiveApply(ctx context.Context, node uuid.UUID) (v bool, err error) {
	err = s.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM config_apply_operations WHERE node_id=$1 AND state IN('queued','dispatched','accepted','running','unknown'))`, node).Scan(&v)
	return
}

func (s configurationStore) InsertPlan(ctx context.Context, v configurationstore.PendingPlan) error {
	_, err := s.Exec(ctx, `INSERT INTO config_plans(id,workspace_id,node_id,operation_id,template_name,expected_revision,candidate_hash,candidate_redacted,warnings,expires_at,created_by,created_at)VALUES($1,$2,$3,$1,$4,$5,$6,$7,$8,$9,$10,$11)`, v.ID, v.WorkspaceID, v.NodeID, v.TemplateName, v.ExpectedRevision, v.CandidateHash, v.CandidateRedacted, v.Warnings, v.ExpiresAt, v.CreatedBy, v.CreatedAt)
	return err
}

func (s configurationStore) InsertApply(ctx context.Context, v configurationstore.PendingApply) error {
	_, err := s.Exec(ctx, `INSERT INTO config_apply_operations(operation_id,workspace_id,node_id,plan_id,approval_id,expected_revision,desired_revision,candidate_hash,previous_hash,state,created_at,updated_at)VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,'queued',$10,$10)`, v.ID, v.WorkspaceID, v.NodeID, v.PlanID, v.ApprovalID, v.ExpectedRevision, v.DesiredRevision, v.CandidateHash, v.PreviousHash, v.CreatedAt)
	return err
}

func (s configurationStore) AdvanceDesiredRevision(ctx context.Context, node uuid.UUID, revision uint64, at value.Timestamp) (bool, error) {
	count, err := s.Exec(ctx, `INSERT INTO node_config_state(node_id,revision,desired_revision,redacted_config,updated_at)VALUES($1,0,$2,'',$3)
		ON CONFLICT(node_id) DO UPDATE SET desired_revision=EXCLUDED.desired_revision,updated_at=EXCLUDED.updated_at WHERE node_config_state.desired_revision=$2-1`, node, revision, at)
	return count == 1, err
}
