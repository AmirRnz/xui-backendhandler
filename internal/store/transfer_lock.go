package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// DeploymentRequestLease holds a deployment-scoped transaction advisory lock
// until Release. Request handlers take shared leases; a transfer takes an
// exclusive lease after setting transfer_frozen, then snapshots only after all
// requests admitted before the freeze have finished.
type DeploymentRequestLease struct {
	conn *pgxpool.Conn
	tx   pgx.Tx
}

func (l *DeploymentRequestLease) IsEnabled(ctx context.Context, requireBot bool, deployment string) (bool, error) {
	if l == nil || l.tx == nil {
		return false, errors.New("deployment lock is not active")
	}
	query := `SELECT enabled AND NOT transfer_frozen FROM deployments WHERE id=$1`
	if requireBot {
		query = `SELECT enabled AND bot_instance_active AND NOT transfer_frozen FROM deployments WHERE id=$1`
	}
	var enabled bool
	err := l.tx.QueryRow(ctx, query, deployment).Scan(&enabled)
	return enabled, err
}

func (l *DeploymentRequestLease) Release(ctx context.Context) error {
	if l == nil || l.conn == nil || l.tx == nil {
		return nil
	}
	err := l.tx.Rollback(ctx)
	l.conn.Release()
	l.conn = nil
	l.tx = nil
	if errors.Is(err, pgx.ErrTxClosed) {
		return nil
	}
	return err
}

func (s *Store) AcquireDeploymentRequestLease(ctx context.Context, deployment string) (*DeploymentRequestLease, error) {
	return s.acquireDeploymentLease(ctx, deployment, true)
}

func (s *Store) AcquireDeploymentTransferLease(ctx context.Context, deployment string) (*DeploymentRequestLease, error) {
	return s.acquireDeploymentLease(ctx, deployment, false)
}

// RefreezeRestoredDeployment is the checked rollback for a failed instance
// activation. It verifies both the archive checkpoint and persisted frozen
// state before callers report recovery as safe.
func (s *Store) RefreezeRestoredDeployment(ctx context.Context, deployment, fingerprint string) error {
	if deployment == "" || fingerprint == "" {
		return errors.New("deployment and restore fingerprint are required")
	}
	updateCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	tag, err := s.DB.Exec(updateCtx, `UPDATE deployments SET transfer_frozen=true WHERE id=$1 AND restore_fingerprint=$2`, deployment, fingerprint)
	if err != nil {
		return errors.New("transfer freeze update failed")
	}
	if tag.RowsAffected() != 1 {
		return errors.New("matching restored deployment was not found")
	}
	var frozen bool
	if err = s.DB.QueryRow(updateCtx, `SELECT transfer_frozen FROM deployments WHERE id=$1 AND restore_fingerprint=$2`, deployment, fingerprint).Scan(&frozen); err != nil || !frozen {
		return errors.New("transfer freeze state could not be verified")
	}
	return nil
}

func (s *Store) acquireDeploymentLease(ctx context.Context, deployment string, shared bool) (*DeploymentRequestLease, error) {
	if deployment == "" {
		return nil, errors.New("deployment ID is required")
	}
	conn, err := s.DB.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	tx, err := conn.Begin(ctx)
	if err != nil {
		conn.Release()
		return nil, err
	}
	lock := `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`
	if shared {
		lock = `SELECT pg_advisory_xact_lock_shared(hashtextextended($1,0))`
	}
	if _, err = tx.Exec(ctx, lock, deployment); err != nil {
		_ = tx.Rollback(context.Background())
		conn.Release()
		return nil, err
	}
	return &DeploymentRequestLease{conn: conn, tx: tx}, nil
}
