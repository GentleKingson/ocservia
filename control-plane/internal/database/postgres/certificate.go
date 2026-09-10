package postgres

import (
	"context"
	"time"

	certificatestore "github.com/GentleKingson/ocservia/control-plane/internal/certificates/store"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/google/uuid"
)

type certificateStore struct{ database.Tx }

func (t *transaction) CertificateStore() certificatestore.Store { return certificateStore{t} }

func (s certificateStore) Maintain(ctx context.Context, at time.Time) error {
	for _, state := range []string{"expired", "expiring"} {
		predicate := `state IN ('issued','expiring') AND not_after<=now()`
		if state == "expiring" {
			predicate = `state='issued' AND not_after>now() AND not_after<=now()+interval '30 days'`
		}
		rows, err := s.Query(ctx, `WITH due AS (
			SELECT id FROM certificates WHERE `+predicate+` ORDER BY not_after,id LIMIT 100 FOR UPDATE SKIP LOCKED
		) UPDATE certificates c SET state=$1,version=version+1,updated_at=now() FROM due WHERE c.id=due.id RETURNING c.id,c.workspace_id,c.node_id`, state)
		if err != nil {
			return err
		}
		type alert struct{ id, workspace, node uuid.UUID }
		var alerts []alert
		for rows.Next() {
			var a alert
			if err := rows.Scan(&a.id, &a.workspace, &a.node); err != nil {
				rows.Close()
				return err
			}
			alerts = append(alerts, a)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		for _, a := range alerts {
			if _, err := s.Exec(ctx, `INSERT INTO security_alerts(id,workspace_id,severity,kind,node_id,resource_type,resource_id,created_at) VALUES($1,$2,'high',$3,$4,'certificate',$5,$6)`, uuid.Must(uuid.NewV7()), a.workspace, "certificate."+state, a.node, a.id, at); err != nil {
				return err
			}
		}
		if state == "expired" {
			if _, err := s.Exec(ctx, `UPDATE artifact_operations a SET state='expired',lease_until=NULL,updated_at=now() FROM certificates c WHERE c.id=a.certificate_id AND c.state='expired' AND a.state IN ('pending','ready','leased','consuming')`); err != nil {
				return err
			}
		}
	}
	_, err := s.Exec(ctx, `UPDATE artifact_operations SET state='expired',lease_until=NULL,updated_at=now() WHERE state IN ('pending','ready','leased') AND expires_at<=now()`)
	return err
}
