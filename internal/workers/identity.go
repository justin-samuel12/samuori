package workers

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"samuori.invalid/samuori/internal/audit"
	"samuori.invalid/samuori/internal/authz"
)

// Operational states an operator can set (Revoke is separate: it ends the identity).
const (
	Cordoned    = "cordoned"
	Draining    = "draining"
	Quarantined = "quarantined"
)

var ErrRevoked = errors.New("the worker identity is revoked")

// Renew issues a new certificate for an enrolled worker from a fresh CSR and records the trust-bundle version it
// acknowledged. The previous serial stays valid only until the new one is first used, so a worker that crashes
// mid-renewal is not locked out, while a stolen old certificate stops working as soon as the worker reconnects.
func (s *Store) Renew(ctx context.Context, w Worker, csr []byte, ackVersion uint64) ([]byte, time.Time, error) {
	if w.EnrollmentState != Enrolled {
		return nil, time.Time{}, ErrRevoked
	}
	cert, err := s.auth.Issuing().SignWorker(csr, w.ID, s.CertTTL)
	if err != nil {
		return nil, time.Time{}, err
	}
	serial, expires := cert.SerialNumber.Text(16), cert.NotAfter.UTC()
	err = s.db.InTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, s.db.Rebind(`UPDATE workers SET prev_cert_serial = cert_serial, cert_serial = ?, cert_expires_at = ?, bundle_version = ? WHERE id = ? AND enrollment_state = ? AND cert_serial = ?`),
			serial, expires, ackVersion, w.ID, Enrolled, w.CertSerial)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return ErrConflict
		}
		return audit.Append(ctx, s.db, tx, audit.Event{Actor: authz.Principal{ID: w.ID, OrgID: w.OrgID, Kind: authz.Service}, Action: "worker.certificate.renew", ResourceType: "worker", ResourceID: w.ID, Outcome: "succeeded", Reason: "expires " + expires.Format(time.RFC3339)})
	})
	if err != nil {
		return nil, time.Time{}, err
	}
	return cert.Raw, expires, nil
}

// AcknowledgeBundle records the trust-bundle version a worker reported.
func (s *Store) AcknowledgeBundle(ctx context.Context, workerID string, version uint64) error {
	_, err := s.db.SQL.ExecContext(ctx, s.db.Rebind(`UPDATE workers SET bundle_version = ? WHERE id = ? AND bundle_version < ?`), version, workerID, version)
	return err
}

// SetOperational cordons, drains, quarantines or reactivates a worker. Enrollment is unaffected.
func (s *Store) SetOperational(ctx context.Context, actor authz.Principal, id, state, reason, requestID string) (Worker, error) {
	if actor.Kind != authz.System && !canOperate(actor) {
		return Worker{}, ErrForbidden
	}
	err := s.db.InTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, s.db.Rebind(`UPDATE workers SET operational_state = ? WHERE id = ? AND org_id = ? AND enrollment_state = ?`), state, id, actor.OrgID, Enrolled)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return ErrNotFound
		}
		return audit.Append(ctx, s.db, tx, audit.Event{Actor: actor, Action: "worker." + state, ResourceType: "worker", ResourceID: id, Outcome: "succeeded", Reason: reason, RequestID: requestID})
	})
	if err != nil {
		return Worker{}, err
	}
	return s.Get(ctx, actor.OrgID, id)
}

// Revoke ends a worker identity immediately: every later request is denied. compromisedSince, when set, marks the
// time after which the worker's outputs are untrusted.
func (s *Store) Revoke(ctx context.Context, actor authz.Principal, id, reason string, compromisedSince *time.Time, requestID string) (Worker, error) {
	if !actor.IsOrgAdmin() && actor.Kind != authz.System {
		return Worker{}, ErrForbidden
	}
	now := s.now().UTC()
	err := s.db.InTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, s.db.Rebind(`UPDATE workers SET enrollment_state = ?, revoked_at = ?, revoke_reason = ?, compromised_since = ? WHERE id = ? AND org_id = ? AND enrollment_state <> ?`),
			Revoked, now, reason, compromisedSince, id, actor.OrgID, Revoked)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return ErrNotFound
		}
		action := "worker.revoke"
		if compromisedSince != nil {
			action = "worker.revoke.compromised"
		}
		return audit.Append(ctx, s.db, tx, audit.Event{Actor: actor, Action: action, ResourceType: "worker", ResourceID: id, Outcome: "succeeded", Reason: reason, RequestID: requestID})
	})
	if err != nil {
		return Worker{}, err
	}
	return s.Get(ctx, actor.OrgID, id)
}

// PendingAcknowledgement lists enrolled workers with a valid certificate that have not acknowledged version.
func (s *Store) PendingAcknowledgement(ctx context.Context, version uint64) ([]Worker, error) {
	rows, err := s.db.SQL.QueryContext(ctx, s.db.Rebind(selectWorker+` WHERE enrollment_state = ? AND cert_expires_at > ? AND bundle_version < ? ORDER BY id`), Enrolled, s.now().UTC(), version)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Worker
	for rows.Next() {
		w, err := scanWorker(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// RevokeAll revokes every enrolled worker identity, for emergency CA rotation.
func (s *Store) RevokeAll(ctx context.Context, actor authz.Principal, reason, requestID string) ([]string, error) {
	rows, err := s.db.SQL.QueryContext(ctx, s.db.Rebind(`SELECT id, org_id FROM workers WHERE enrollment_state = ?`), Enrolled)
	if err != nil {
		return nil, err
	}
	type ref struct{ id, org string }
	var list []ref
	for rows.Next() {
		var r ref
		if err := rows.Scan(&r.id, &r.org); err != nil {
			rows.Close()
			return nil, err
		}
		list = append(list, r)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, r := range list {
		a := authz.Principal{ID: actor.ID, OrgID: r.org, Kind: authz.System}
		if _, err := s.Revoke(ctx, a, r.id, reason, nil, requestID); err != nil && !errors.Is(err, ErrNotFound) {
			return ids, err
		}
		ids = append(ids, r.id)
	}
	return ids, nil
}
