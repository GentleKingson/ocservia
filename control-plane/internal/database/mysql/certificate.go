package mysql

import (
	"context"
	"time"

	certificatestore "github.com/GentleKingson/ocservia/control-plane/internal/certificates/store"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	"github.com/google/uuid"
)

type certificateStore struct{ database.Tx }

func (t *transaction) CertificateStore() certificatestore.Store { return certificateStore{t} }

func (s certificateStore) Maintain(ctx context.Context, at time.Time) error {
	alertAt, err := value.FromTime(at)
	if err != nil {
		return err
	}
	now, err := database.TransactionTime(ctx, s.Tx)
	if err != nil {
		return err
	}
	finiteNow, err := now.Time()
	if err != nil {
		return err
	}
	soon, err := value.FromTime(finiteNow.AddDate(0, 0, 30))
	if err != nil {
		return err
	}
	for _, state := range []string{"expired", "expiring"} {
		predicate := `state IN ('issued','expiring') AND not_after<=?`
		args := []any{now}
		if state == "expiring" {
			predicate = `state='issued' AND not_after>? AND not_after<=?`
			args = append(args, soon)
		}
		rows, err := s.Query(ctx, `SELECT id,workspace_id,node_id FROM certificates WHERE `+predicate+` ORDER BY not_after,id LIMIT 100 FOR UPDATE SKIP LOCKED`, args...)
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
			if _, err := s.Exec(ctx, `UPDATE certificates SET state=?,version=version+1,updated_at=? WHERE id=?`, state, now, UUIDBytes(a.id)); err != nil {
				return err
			}
			if _, err := s.Exec(ctx, `INSERT INTO security_alerts(id,workspace_id,severity,kind,node_id,resource_type,resource_id,created_at) VALUES(?,?,'high',?,?,'certificate',?,?)`, UUIDBytes(uuid.Must(uuid.NewV7())), UUIDBytes(a.workspace), "certificate."+state, UUIDBytes(a.node), UUIDBytes(a.id), alertAt); err != nil {
				return err
			}
		}
		if state == "expired" {
			if _, err := s.Exec(ctx, `UPDATE artifact_operations a SET state='expired',lease_until=NULL,updated_at=? WHERE a.state IN ('pending','ready','leased','consuming') AND EXISTS (SELECT 1 FROM certificates c WHERE c.id=a.certificate_id AND c.state='expired')`, now); err != nil {
				return err
			}
		}
	}
	_, err = s.Exec(ctx, `UPDATE artifact_operations SET state='expired',lease_until=NULL,updated_at=? WHERE state IN ('pending','ready','leased') AND expires_at<=?`, now, now)
	return err
}
