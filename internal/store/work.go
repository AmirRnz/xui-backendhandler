package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

type WorkItem struct {
	ID              int64
	DeploymentID    string
	AccountID       int64
	ActorID         int64
	PanelID         string
	SubscriptionID  int64
	PaymentIntentID int64
	OperationKey    string
	Kind            string
	Desired         provisionData
	Phase           string
	Attempts        int
}

func (s *Store) ClaimWork(ctx context.Context) (*WorkItem, error) {
	w := &WorkItem{}
	var payload []byte
	var account, actor, sub, intent int64
	err := s.DB.QueryRow(ctx, `WITH candidate AS (
		SELECT w.id FROM work_items w JOIN deployments d ON d.id=w.deployment_id AND d.enabled AND NOT d.transfer_frozen WHERE NOT w.restore_quarantined AND ((w.status='pending' AND w.next_attempt_at<=now()) OR (w.status='running' AND w.lease_until<now()))
		ORDER BY w.id FOR UPDATE OF w SKIP LOCKED LIMIT 1
	) UPDATE work_items w SET status='running',attempts=attempts+1,lease_until=now()+interval '60 seconds',updated_at=now()
	FROM candidate c WHERE w.id=c.id RETURNING w.id,w.deployment_id,COALESCE(w.account_id,0),COALESCE(w.actor_id,0),w.panel_id,COALESCE(w.subscription_id,0),COALESCE(w.payment_intent_id,0),w.operation_key,w.kind,w.desired_state,w.phase,w.attempts`,
	).Scan(&w.ID, &w.DeploymentID, &account, &actor, &w.PanelID, &sub, &intent, &w.OperationKey, &w.Kind, &payload, &w.Phase, &w.Attempts)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	w.AccountID = account
	w.ActorID = actor
	w.SubscriptionID = sub
	w.PaymentIntentID = intent
	if err = json.Unmarshal(payload, &w.Desired); err != nil {
		return nil, fmt.Errorf("decode durable work item %d: %w", w.ID, err)
	}
	return w, nil
}

func (s *Store) MarkCreateAttempted(ctx context.Context, id int64) error {
	tag, err := s.DB.Exec(ctx, `UPDATE work_items SET phase='create_attempted',updated_at=now() WHERE id=$1 AND status='running' AND phase='ready'`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrConflict
	}
	return nil
}

func (s *Store) RetryWork(ctx context.Context, id int64, reason string, definitiveNoWrite bool) error {
	if len(reason) > 2000 {
		reason = reason[:2000]
	}
	phase := "create_attempted"
	if definitiveNoWrite {
		phase = "ready"
	}
	_, err := s.DB.Exec(ctx, `UPDATE work_items SET status='pending',phase=$2,last_error=$3,lease_until=NULL,next_attempt_at=now()+interval '15 seconds',updated_at=now() WHERE id=$1 AND status='running'`, id, phase, reason)
	return err
}

func (s *Store) ManualReviewWork(ctx context.Context, id int64, reason string, observed any) error {
	if len(reason) > 2000 {
		reason = reason[:2000]
	}
	payload, _ := json.Marshal(observed)
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var subscription, orderID int64
	err = tx.QueryRow(ctx, `UPDATE work_items SET status='manual_review',phase='manual_review',last_error=$2,observed_state=$3,lease_until=NULL,updated_at=now()
		WHERE id=$1 AND status='running' RETURNING COALESCE(subscription_id,0)`, id, reason, payload).Scan(&subscription)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrConflict
	}
	if err != nil {
		return err
	}
	if subscription > 0 {
		if _, err = tx.Exec(ctx, `UPDATE subscriptions SET status='manual_review',updated_at=now() WHERE id=$1`, subscription); err != nil {
			return err
		}
		lookupErr := tx.QueryRow(ctx, `SELECT id FROM orders WHERE subscription_id=$1`, subscription).Scan(&orderID)
		if lookupErr == nil && orderID > 0 {
			if _, err = tx.Exec(ctx, `UPDATE orders SET status='manual_review',updated_at=now() WHERE id=$1`, orderID); err != nil {
				return err
			}
		} else if lookupErr != nil && !errors.Is(lookupErr, pgx.ErrNoRows) {
			return lookupErr
		}
	}
	return tx.Commit(ctx)
}

func (s *Store) SucceedWork(ctx context.Context, w *WorkItem, links []string) error {
	if w == nil {
		return fmt.Errorf("work item is required")
	}
	linkJSON, _ := json.Marshal(links)
	if links == nil {
		linkJSON = []byte("[]")
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var sid int64
	var subStatus, topic string
	subStatus = "active"
	topic = "subscription.ready"
	if w.Kind == "subscription_delete" {
		subStatus = "cancelled"
		topic = "subscription.cancelled"
	}
	err = tx.QueryRow(ctx, `UPDATE work_items SET status='succeeded',observed_state=jsonb_build_object('verified',true,'links',$2::jsonb),last_error='',lease_until=NULL,updated_at=now()
		WHERE id=$1 AND status='running' RETURNING COALESCE(subscription_id,0)`, w.ID, linkJSON).Scan(&sid)
	if err != nil {
		return err
	}
	if sid > 0 {
		if _, err = tx.Exec(ctx, `UPDATE subscriptions SET status=$2,subscription_links=$3,updated_at=now() WHERE id=$1`, sid, subStatus, linkJSON); err != nil {
			return err
		}
		if w.Kind != "subscription_delete" {
			if _, err = tx.Exec(ctx, `UPDATE orders SET status='completed',updated_at=now() WHERE subscription_id=$1`, sid); err != nil {
				return err
			}
		}
		if _, err = tx.Exec(ctx, `UPDATE trial_claims SET status='committed' WHERE work_item_id=$1 AND status='reserved'`, w.ID); err != nil {
			return err
		}
		var deployment string
		var account, actor int64
		var email string
		if err = tx.QueryRow(ctx, `SELECT w.deployment_id,COALESCE(w.account_id,0),COALESCE(w.actor_id,0),s.client_email FROM work_items w JOIN subscriptions s ON s.id=w.subscription_id WHERE w.id=$1`, w.ID).Scan(&deployment, &account, &actor, &email); err != nil {
			return err
		}
		payload, _ := json.Marshal(map[string]any{"subscription_id": sid, "email": email, "links": links, "status": subStatus})
		if _, err = tx.Exec(ctx, `INSERT INTO outbox(deployment_id,account_id,actor_id,dedupe_key,topic,payload) VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT(deployment_id,dedupe_key) DO NOTHING`, deployment, account, actor, topic+":"+fmt.Sprint(sid), topic, payload); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func (s *Store) CancelSubscription(ctx context.Context, a *Actor, id int64, key string) (int64, error) {
	if a == nil || !a.Enabled || id <= 0 || !validKey(key) {
		return 0, ErrForbidden
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	op := "cancel:" + key
	inputHash := hashJSON(struct{ SubscriptionID int64 }{id})
	var priorID int64
	var priorHash string
	err = tx.QueryRow(ctx, `SELECT id,input_hash FROM work_items WHERE deployment_id=$1 AND account_id=$2 AND operation_key=$3`, a.DeploymentID, a.AccountID, op).Scan(&priorID, &priorHash)
	if err == nil {
		if priorHash != inputHash {
			return 0, ErrConflict
		}
		if err = tx.Commit(ctx); err != nil {
			return 0, err
		}
		return priorID, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return 0, err
	}
	var email, uuid, subid, panel, dep, status string
	var account int64
	err = tx.QueryRow(ctx, `SELECT client_email,client_uuid,sub_id,panel_id,deployment_id,account_id,status
		FROM subscriptions WHERE id=$1 AND deployment_id=$2 AND account_id=$3 FOR UPDATE`, id, a.DeploymentID, a.AccountID).
		Scan(&email, &uuid, &subid, &panel, &dep, &account, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, err
	}
	if status != "active" && status != "expired" {
		var workID int64
		var oldHash string
		lookupErr := tx.QueryRow(ctx, `SELECT id,input_hash FROM work_items WHERE deployment_id=$1 AND account_id=$2 AND operation_key=$3`, a.DeploymentID, a.AccountID, op).Scan(&workID, &oldHash)
		if lookupErr == nil && oldHash == inputHash {
			if err = tx.Commit(ctx); err != nil {
				return 0, err
			}
			return workID, nil
		}
		if lookupErr != nil && !errors.Is(lookupErr, pgx.ErrNoRows) {
			return 0, lookupErr
		}
		return 0, ErrConflict
	}
	var workID int64
	err = tx.QueryRow(ctx, `INSERT INTO work_items(deployment_id,account_id,actor_id,panel_id,subscription_id,operation_key,input_hash,kind,desired_state)
		VALUES($1,$2,$3,$4,$5,$6,$7,'subscription_delete',$8) ON CONFLICT(deployment_id,operation_key) DO NOTHING RETURNING id`, dep, account, a.ID, panel, id, op, inputHash, marshalJSON(map[string]any{"email": email, "uuid": uuid, "sub_id": subid, "client_email": email})).Scan(&workID)
	if errors.Is(err, pgx.ErrNoRows) {
		var oldHash string
		err = tx.QueryRow(ctx, `SELECT id,input_hash FROM work_items WHERE deployment_id=$1 AND operation_key=$2`, dep, op).Scan(&workID, &oldHash)
		if err != nil {
			return 0, err
		}
		if oldHash != inputHash {
			return 0, ErrConflict
		}
	} else if err != nil {
		return 0, err
	}
	if _, err = tx.Exec(ctx, `UPDATE subscriptions SET status='cancel_requested',updated_at=now() WHERE id=$1`, id); err != nil {
		return 0, err
	}
	if err = tx.Commit(ctx); err != nil {
		return 0, err
	}
	return workID, nil
}

func (s *Store) OutboxPending(ctx context.Context, limit int) ([]map[string]any, error) {
	if limit < 1 || limit > 500 {
		limit = 100
	}
	rows, err := s.DB.Query(ctx, `WITH picked AS (SELECT o.id FROM outbox o JOIN deployments d ON d.id=o.deployment_id AND d.enabled AND d.telegram_notifications_enabled AND NOT d.transfer_frozen WHERE NOT o.restore_quarantined AND ((o.status='pending' AND o.next_attempt_at<=now()) OR (o.status='sending' AND o.lease_until<now())) ORDER BY o.id FOR UPDATE OF o SKIP LOCKED LIMIT $1)
		UPDATE outbox o SET status='sending',lease_until=now()+interval '60 seconds',attempts=attempts+1 FROM picked p WHERE o.id=p.id
		RETURNING o.id,o.deployment_id,COALESCE((SELECT telegram_id FROM actors WHERE id=o.actor_id),0),o.topic,o.payload::text`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id, chatID int64
		var deployment, topic, payload string
		if err = rows.Scan(&id, &deployment, &chatID, &topic, &payload); err != nil {
			return nil, err
		}
		var v any
		if err = json.Unmarshal([]byte(payload), &v); err != nil {
			return nil, err
		}
		out = append(out, map[string]any{"id": id, "deployment_id": deployment, "chat_id": chatID, "topic": topic, "payload": v})
	}
	return out, rows.Err()
}

func (s *Store) MarkOutbox(ctx context.Context, id int64, sent bool) error {
	status := "pending"
	if sent {
		status = "sent"
	}
	_, err := s.DB.Exec(ctx, `UPDATE outbox SET status=$2,lease_until=NULL,next_attempt_at=now()+LEAST(interval '6 hours',interval '15 seconds'*power(2,LEAST(attempts,10))) WHERE id=$1`, id, status)
	return err
}

func (s *Store) RecentWork(ctx context.Context, a *Actor) ([]map[string]any, error) {
	if !isAdmin(a) {
		return nil, ErrForbidden
	}
	rows, err := s.DB.Query(ctx, `SELECT id,operation_key,kind,status,phase,attempts,last_error,created_at,updated_at FROM work_items WHERE deployment_id=$1 ORDER BY id DESC LIMIT 100`, a.DeploymentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id int64
		var key, kind, status, phase, message string
		var attempts int
		var created, updated time.Time
		if err = rows.Scan(&id, &key, &kind, &status, &phase, &attempts, &message, &created, &updated); err != nil {
			return nil, err
		}
		out = append(out, map[string]any{"id": id, "operation_key": key, "kind": kind, "status": status, "phase": phase, "attempts": attempts, "last_error": message, "created_at": created, "updated_at": updated})
	}
	return out, rows.Err()
}
