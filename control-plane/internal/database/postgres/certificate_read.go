package postgres

import (
	"context"

	certificatestore "github.com/GentleKingson/ocservia/control-plane/internal/certificates/store"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/google/uuid"
)

const certificateColumns = `id,workspace_id,node_id,operation_id,common_name,dns_names,key_bits,state,version,public_key_sha256,COALESCE(serial_number,''),not_before,not_after,revoked_at,created_at,updated_at`

func scanCertificate(row database.Row) (v certificatestore.Certificate, err error) {
	err = row.Scan(&v.ID, &v.WorkspaceID, &v.NodeID, &v.OperationID, &v.CommonName, &v.DNSNames, &v.KeyBits, &v.State, &v.Version, &v.PublicKeySHA256, &v.SerialNumber, &v.NotBefore, &v.NotAfter, &v.RevokedAt, &v.CreatedAt, &v.UpdatedAt)
	return
}

func (s certificateStore) Get(ctx context.Context, id uuid.UUID) (certificatestore.Certificate, error) {
	return scanCertificate(s.QueryRow(ctx, `SELECT `+certificateColumns+` FROM certificates WHERE id=$1`, id))
}

func (s certificateStore) GetByOperation(ctx context.Context, id uuid.UUID) (certificatestore.Certificate, error) {
	return scanCertificate(s.QueryRow(ctx, `SELECT `+certificateColumns+` FROM certificates WHERE operation_id=$1`, id))
}

func (s certificateStore) ListNode(ctx context.Context, id uuid.UUID) ([]certificatestore.Certificate, error) {
	rows, err := s.Query(ctx, `SELECT `+certificateColumns+` FROM certificates WHERE node_id=$1 ORDER BY created_at DESC,id DESC LIMIT 100`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	values := make([]certificatestore.Certificate, 0)
	for rows.Next() {
		v, err := scanCertificate(rows)
		if err != nil {
			return nil, err
		}
		values = append(values, v)
	}
	return values, rows.Err()
}

func (s certificateStore) Resource(ctx context.Context, id uuid.UUID) (workspace, node uuid.UUID, err error) {
	err = s.QueryRow(ctx, `SELECT workspace_id,node_id FROM certificates WHERE id=$1`, id).Scan(&workspace, &node)
	return
}
