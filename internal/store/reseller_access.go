package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

const resellerAccessRequestCooldown = 24 * time.Hour

type ResellerAccessRequestResult struct {
	Status        string     `json:"status"`
	NextRequestAt *time.Time `json:"next_request_at,omitempty"`
}

// RequestResellerAccess records a pending reseller's request and queues the
// deployment admin notification atomically. Actor row locking makes the
// cooldown effective across concurrent API processes.
func (s *Store) RequestResellerAccess(ctx context.Context, a *Actor) (*ResellerAccessRequestResult, error) {
	if a == nil || !a.Enabled || a.ID <= 0 || a.DeploymentID == "" {
		return nil, ErrForbidden
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	var role, channel, approval string
	err = tx.QueryRow(ctx, `SELECT a.role,d.channel,a.approval_status
		FROM actors a JOIN deployments d ON d.id=a.deployment_id
		WHERE a.id=$1 AND a.deployment_id=$2 AND a.enabled
		FOR UPDATE OF a`, a.ID, a.DeploymentID).Scan(&role, &channel, &approval)
	if err != nil {
		if err == pgx.ErrNoRows {
			return nil, ErrForbidden
		}
		return nil, err
	}
	if role != "reseller" || channel != "reseller" || approval != "pending" {
		return nil, ErrForbidden
	}

	var lastRequest time.Time
	err = tx.QueryRow(ctx, `SELECT created_at FROM reseller_access_requests
		WHERE deployment_id=$1 AND actor_id=$2 ORDER BY created_at DESC,id DESC LIMIT 1`, a.DeploymentID, a.ID).Scan(&lastRequest)
	if err == nil {
		var now time.Time
		if err = tx.QueryRow(ctx, `SELECT now()`).Scan(&now); err != nil {
			return nil, err
		}
		nextRequestAt := lastRequest.Add(resellerAccessRequestCooldown).UTC()
		if now.Before(nextRequestAt) {
			if err = tx.Commit(ctx); err != nil {
				return nil, err
			}
			return &ResellerAccessRequestResult{Status: "already_pending", NextRequestAt: &nextRequestAt}, nil
		}
	} else if err != pgx.ErrNoRows {
		return nil, err
	}

	var adminActorID, requestID int64
	err = tx.QueryRow(ctx, `SELECT id FROM actors
		WHERE deployment_id=$1 AND telegram_id=96937669 AND role='admin' AND approval_status='approved' AND enabled`, a.DeploymentID).Scan(&adminActorID)
	if err != nil {
		return nil, fmt.Errorf("resolve reseller deployment admin notification recipient: %w", err)
	}
	var createdAt time.Time
	if err = tx.QueryRow(ctx, `INSERT INTO reseller_access_requests(deployment_id,actor_id)
		VALUES($1,$2) RETURNING id,created_at`, a.DeploymentID, a.ID).Scan(&requestID, &createdAt); err != nil {
		return nil, err
	}
	payload, err := json.Marshal(map[string]any{
		"telegram_id": a.TelegramID,
		"request_id":  requestID,
	})
	if err != nil {
		return nil, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO outbox(deployment_id,actor_id,dedupe_key,topic,payload)
		VALUES($1,$2,$3,'reseller.access_requested',$4)
		ON CONFLICT(deployment_id,dedupe_key) DO NOTHING`, a.DeploymentID, adminActorID, fmt.Sprintf("reseller-access-request:%d", requestID), payload); err != nil {
		return nil, err
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, err
	}
	return &ResellerAccessRequestResult{Status: "submitted"}, nil
}
