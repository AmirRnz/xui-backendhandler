package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"

	"example.com/xui-commerce/backend/internal/commerce"
	"github.com/jackc/pgx/v5"
)

func (s *Store) ClaimTrial(ctx context.Context, a *Actor, planID int64, key string) (*PurchaseResult, error) {
	if a == nil || !a.Enabled || !validKey(key) || planID <= 0 {
		return nil, ErrForbidden
	}
	opKey := "trial:" + key
	inputHash := hashJSON(struct{ PlanID int64 }{planID})
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	var existing PurchaseResult
	var oldHash string
	err = tx.QueryRow(ctx, `SELECT w.id,w.status,COALESCE(w.subscription_id,0),COALESCE(w.payment_intent_id,0),w.input_hash
		FROM work_items w WHERE w.deployment_id=$1 AND w.operation_key=$2`, a.DeploymentID, opKey).
		Scan(&existing.OrderID, &existing.Status, &existing.SubscriptionID, &existing.PaymentIntentID, &oldHash)
	if err == nil {
		if oldHash != inputHash {
			return nil, ErrConflict
		}
		if err = tx.Commit(ctx); err != nil {
			return nil, err
		}
		return &existing, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	if _, err = tx.Exec(ctx, `SELECT id FROM commercial_accounts WHERE id=$1 AND home_deployment_id=$2 FOR UPDATE`, a.AccountID, a.DeploymentID); err != nil {
		return nil, err
	}
	var concurrentID int64
	var concurrentHash string
	err = tx.QueryRow(ctx, `SELECT id,input_hash FROM work_items WHERE deployment_id=$1 AND operation_key=$2`, a.DeploymentID, opKey).Scan(&concurrentID, &concurrentHash)
	if err == nil {
		if concurrentHash != inputHash {
			return nil, ErrConflict
		}
		existing.OrderID = concurrentID
		existing.Status = "pending"
		if err = tx.QueryRow(ctx, `SELECT COALESCE(subscription_id,0),COALESCE(payment_intent_id,0) FROM work_items WHERE id=$1`, concurrentID).Scan(&existing.SubscriptionID, &existing.PaymentIntentID); err != nil {
			return nil, err
		}
		if err = tx.Commit(ctx); err != nil {
			return nil, err
		}
		return &existing, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	p, err := planByID(ctx, tx, a.DeploymentID, a.AccountID, planID)
	if err != nil {
		return nil, err
	}
	if !p.Enabled || p.Kind != "test" {
		return nil, ErrNotFound
	}
	var resetDays, unapprovedLimit int
	var channel string
	var approvedRequired bool
	if err = tx.QueryRow(ctx, `SELECT channel,retail_trial_reset_days,unapproved_trial_daily_limit,COALESCE((configuration->>'reseller_approved_required')::boolean,false) FROM deployments WHERE id=$1`, a.DeploymentID).Scan(&channel, &resetDays, &unapprovedLimit, &approvedRequired); err != nil {
		return nil, err
	}
	if channel == "reseller" && approvedRequired && a.ApprovalStatus != "approved" {
		return nil, ErrForbidden
	}
	if len(validInbounds(p.InboundIDs)) == 0 {
		return nil, fmt.Errorf("test plan has no valid inbound IDs")
	}
	if p.ExpireSeconds > math.MaxInt64/1000 {
		return nil, fmt.Errorf("test duration exceeds panel expiry range")
	}
	suffix, e := randomHex(4)
	if e != nil {
		return nil, e
	}
	id, e := randomHex(16)
	if e != nil {
		return nil, e
	}
	sub, e := randomHex(12)
	if e != nil {
		return nil, e
	}
	name := "test"
	if a.Role == "reseller" {
		name = "demo"
	}
	d := provisionData{Email: fmt.Sprintf("%s_%d_%s", name, a.TelegramID, suffix), UUID: formatUUID(id), ClientUUID: formatUUID(id), SubID: sub,
		DisplayName: p.Name, PlanName: p.Name, IPLimit: p.IPLimit, TrafficLimitBytes: p.MaxDataBytes, ExpiryTimeMS: -p.ExpireSeconds * 1000, Flow: p.Flow, InboundIDs: validInbounds(p.InboundIDs), TelegramID: a.TelegramID, PanelID: p.PanelID, OperationKey: opKey}
	_, workID, err := createProvisionWork(ctx, tx, a, p, d, opKey, inputHash, "test", nil)
	if err != nil {
		return nil, err
	}
	policy, period := "retail_cooldown", time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	limit := 1
	if channel == "reseller" {
		policy = "reseller_daily"
		now := time.Now().UTC()
		period = time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
		if a.ApprovalStatus == "approved" {
			limit = p.MaxPerDay
		} else {
			limit = unapprovedLimit
			if limit <= 0 {
				limit = 1
			}
		}
	}
	var usageRows int64
	if policy == "retail_cooldown" {
		err = tx.QueryRow(ctx, `INSERT INTO trial_usage(deployment_id,account_id,plan_id,policy,period_date,used_count,last_claimed_at,last_work_item_id)
			VALUES($1,$2,$3,$4,$5,1,now(),$6) ON CONFLICT(deployment_id,account_id,plan_id,policy,period_date)
			DO UPDATE SET used_count=trial_usage.used_count+1,last_claimed_at=now(),last_work_item_id=EXCLUDED.last_work_item_id
			WHERE $7::int>0 AND trial_usage.last_claimed_at<=now()-($7::int*interval '1 day') RETURNING used_count`, a.DeploymentID, a.AccountID, planID, policy, period, workID, resetDays).Scan(new(int))
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrQuotaExceeded
		}
		if err != nil {
			return nil, err
		}
		usageRows = 1
	} else {
		if limit <= 0 {
			return nil, ErrQuotaExceeded
		}
		var used int
		err = tx.QueryRow(ctx, `INSERT INTO trial_usage(deployment_id,account_id,plan_id,policy,period_date,used_count,last_claimed_at,last_work_item_id)
			VALUES($1,$2,$3,$4,$5,1,now(),$6) ON CONFLICT(deployment_id,account_id,plan_id,policy,period_date)
			DO UPDATE SET used_count=trial_usage.used_count+1,last_claimed_at=now(),last_work_item_id=EXCLUDED.last_work_item_id
			WHERE trial_usage.used_count<$7 RETURNING used_count`, a.DeploymentID, a.AccountID, planID, policy, period, workID, limit).Scan(&used)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrQuotaExceeded
		}
		if err != nil {
			return nil, err
		}
		usageRows = 1
	}
	if usageRows == 0 {
		return nil, ErrQuotaExceeded
	}
	_, err = tx.Exec(ctx, `INSERT INTO trial_claims(work_item_id,deployment_id,account_id,plan_id,policy,period_date,status) VALUES($1,$2,$3,$4,$5,$6,'reserved')`, workID, a.DeploymentID, a.AccountID, planID, policy, period)
	if err != nil {
		return nil, err
	}
	result := &PurchaseResult{Status: "pending"}
	if err = tx.QueryRow(ctx, `SELECT subscription_id FROM work_items WHERE id=$1`, workID).Scan(&result.SubscriptionID); err != nil {
		return nil, err
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, err
	}
	return result, nil
}

func (s *Store) CreateTopup(ctx context.Context, a *Actor, amount int64, key string) (int64, error) {
	if a == nil || !a.Enabled || !validKey(key) || amount <= 0 || amount > 1_000_000_000_000 {
		return 0, fmt.Errorf("invalid top-up request")
	}
	hash := hashJSON(struct{ Amount int64 }{amount})
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	var id int64
	var oldHash string
	err = tx.QueryRow(ctx, `SELECT id,input_hash FROM topup_requests WHERE deployment_id=$1 AND account_id=$2 AND operation_key=$3`, a.DeploymentID, a.AccountID, key).Scan(&id, &oldHash)
	if err == nil {
		if oldHash != hash {
			return 0, ErrConflict
		}
		if err = tx.Commit(ctx); err != nil {
			return 0, err
		}
		return id, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return 0, err
	}
	var minimum int64
	if err = tx.QueryRow(ctx, `SELECT COALESCE((configuration->>'min_topup_toman')::bigint,0) FROM deployments WHERE id=$1`, a.DeploymentID).Scan(&minimum); err != nil {
		return 0, err
	}
	if amount < minimum {
		return 0, fmt.Errorf("%w: minimum top-up amount is %d Toman", ErrTopupBelowMinimum, minimum)
	}
	err = tx.QueryRow(ctx, `INSERT INTO topup_requests(deployment_id,account_id,actor_id,amount_toman,operation_key,input_hash,status) VALUES($1,$2,$3,$4,$5,$6,'awaiting_receipt') ON CONFLICT(deployment_id,account_id,operation_key) DO NOTHING RETURNING id`, a.DeploymentID, a.AccountID, a.ID, amount, key, hash).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		err = tx.QueryRow(ctx, `SELECT id,input_hash FROM topup_requests WHERE deployment_id=$1 AND account_id=$2 AND operation_key=$3`, a.DeploymentID, a.AccountID, key).Scan(&id, &oldHash)
		if err != nil {
			return 0, err
		}
		if oldHash != hash {
			return 0, ErrConflict
		}
	} else if err != nil {
		return 0, err
	}
	return id, tx.Commit(ctx)
}

func (s *Store) SubmitTopupReceipt(ctx context.Context, a *Actor, id int64, fileID string) error {
	if a == nil || !a.Enabled || id <= 0 || fileID == "" || len(fileID) > 512 {
		return ErrForbidden
	}
	tag, err := s.DB.Exec(ctx, `UPDATE topup_requests SET telegram_file_id=$1,status='receipt_submitted',updated_at=now() WHERE id=$2 AND deployment_id=$3 AND account_id=$4 AND actor_id=$5 AND status='awaiting_receipt'`, fileID, id, a.DeploymentID, a.AccountID, a.ID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 1 {
		return nil
	}
	var old, status string
	err = s.DB.QueryRow(ctx, `SELECT telegram_file_id,status FROM topup_requests WHERE id=$1 AND deployment_id=$2 AND account_id=$3`, id, a.DeploymentID, a.AccountID).Scan(&old, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if old == fileID && status == "receipt_submitted" {
		return nil
	}
	return ErrConflict
}

func (s *Store) PendingTopups(ctx context.Context, a *Actor) ([]map[string]any, error) {
	if !isAdmin(a) {
		return nil, ErrForbidden
	}
	rows, err := s.DB.Query(ctx, `SELECT t.id,t.account_id,t.actor_id,COALESCE(actor.telegram_id,0),t.amount_toman,t.status,t.created_at,t.telegram_file_id
		FROM topup_requests t LEFT JOIN actors actor ON actor.id=t.actor_id AND actor.deployment_id=t.deployment_id AND actor.account_id=t.account_id
		WHERE t.deployment_id=$1 AND t.status='receipt_submitted' ORDER BY t.created_at`, a.DeploymentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id, account, actor, telegramID, amount int64
		var status string
		var receipt string
		var at time.Time
		if err = rows.Scan(&id, &account, &actor, &telegramID, &amount, &status, &at, &receipt); err != nil {
			return nil, err
		}
		out = append(out, map[string]any{"id": id, "account_id": account, "actor_id": actor, "telegram_id": telegramID, "amount_toman": amount, "status": status, "created_at": at, "telegram_file_id": receipt})
	}
	return out, rows.Err()
}

// ActiveTopups returns a bounded page of this actor's resumable wallet top-up requests.
func (s *Store) ActiveTopups(ctx context.Context, a *Actor, beforeID int64) ([]map[string]any, *int64, error) {
	if a == nil || !a.Enabled {
		return nil, nil, ErrForbidden
	}
	if beforeID < 0 {
		return nil, nil, ErrForbidden
	}
	rows, err := s.DB.Query(ctx, `SELECT id,amount_toman,status,created_at FROM topup_requests
		WHERE deployment_id=$1 AND account_id=$2 AND actor_id=$3 AND status IN ('awaiting_receipt','receipt_submitted') AND ($4=0 OR id<$4)
		ORDER BY id DESC LIMIT $5`, a.DeploymentID, a.AccountID, a.ID, beforeID, activeRequestPageSize+1)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	out := make([]map[string]any, 0, activeRequestPageSize)
	for rows.Next() {
		var id, amount int64
		var status string
		var created time.Time
		if err = rows.Scan(&id, &amount, &status, &created); err != nil {
			return nil, nil, err
		}
		out = append(out, map[string]any{"id": id, "status": status, "amount_toman": amount, "created_at": created})
	}
	if err = rows.Err(); err != nil {
		return nil, nil, err
	}
	var next *int64
	if len(out) > activeRequestPageSize {
		cursor := out[activeRequestPageSize-1]["id"].(int64)
		next = &cursor
		out = out[:activeRequestPageSize]
	}
	return out, next, nil
}

// RejectTopup records one audited decision without creating a wallet credit.
func (s *Store) RejectTopup(ctx context.Context, a *Actor, topupID int64) (bool, error) {
	if !isAdmin(a) || topupID <= 0 {
		return false, ErrForbidden
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	var accountID, requesterID int64
	var status string
	err = tx.QueryRow(ctx, `SELECT account_id,actor_id,status FROM topup_requests WHERE id=$1 AND deployment_id=$2 FOR UPDATE`, topupID, a.DeploymentID).Scan(&accountID, &requesterID, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, ErrNotFound
	}
	if err != nil {
		return false, err
	}
	if status == "rejected" {
		if err = tx.Commit(ctx); err != nil {
			return false, err
		}
		return true, nil
	}
	if status != "receipt_submitted" {
		return false, ErrConflict
	}
	if _, err = tx.Exec(ctx, `UPDATE topup_requests SET status='rejected',reviewed_by=$1,review_note='receipt rejected by administrator',updated_at=now() WHERE id=$2`, a.ID, topupID); err != nil {
		return false, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO admin_configuration_audit(deployment_id,actor_id,action,subject_ref) VALUES($1,$2,'topup_rejected',$3)`, a.DeploymentID, a.ID, fmt.Sprintf("topup:%d", topupID)); err != nil {
		return false, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO outbox(deployment_id,account_id,actor_id,dedupe_key,topic,payload) VALUES($1,$2,$3,$4,'topup.rejected','{}'::jsonb) ON CONFLICT(deployment_id,dedupe_key) DO NOTHING`, a.DeploymentID, accountID, requesterID, fmt.Sprintf("topup-rejected:%d", topupID)); err != nil {
		return false, err
	}
	if err = tx.Commit(ctx); err != nil {
		return false, err
	}
	return false, nil
}

func (s *Store) ApproveTopup(ctx context.Context, a *Actor, topupID int64) (int64, error) {
	if !isAdmin(a) || topupID <= 0 {
		return 0, ErrForbidden
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	var account, owner, amount int64
	var status, key string
	err = tx.QueryRow(ctx, `SELECT account_id,actor_id,amount_toman,status,operation_key FROM topup_requests WHERE id=$1 AND deployment_id=$2 FOR UPDATE`, topupID, a.DeploymentID).Scan(&account, &owner, &amount, &status, &key)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, err
	}
	if status == "approved" {
		var bal int64
		err = tx.QueryRow(ctx, `SELECT wallet_balance_toman FROM commercial_accounts WHERE id=$1`, account).Scan(&bal)
		if err == nil {
			err = tx.Commit(ctx)
		}
		return bal, err
	}
	if status != "receipt_submitted" {
		return 0, ErrConflict
	}
	_, err = tx.Exec(ctx, `INSERT INTO wallet_credit_approvals(deployment_id,account_id,topup_request_id,amount_toman,approved_by,operation_key) VALUES($1,$2,$3,$4,$5,$6)`, a.DeploymentID, account, topupID, amount, a.ID, "topup-approval:"+key)
	if err != nil {
		return 0, err
	}
	var balance int64
	err = tx.QueryRow(ctx, `UPDATE commercial_accounts SET wallet_balance_toman=wallet_balance_toman+$1,updated_at=now() WHERE id=$2 AND home_deployment_id=$3 AND wallet_balance_toman<=9223372036854775807-$1 RETURNING wallet_balance_toman`, amount, account, a.DeploymentID).Scan(&balance)
	if err != nil {
		return 0, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO wallet_ledger(deployment_id,account_id,actor_id,amount_toman,balance_after_toman,entry_type,description,operation_key,input_hash,reference_type,reference_id)
		VALUES($1,$2,$3,$4,$5,'credit',$6,$7,$8,'topup_request',$9)`, a.DeploymentID, account, owner, amount, balance, "approved topup", "wallet:topup:"+key, hashJSON(struct {
		Amount int64
		ID     int64
	}{amount, topupID}), topupID)
	if err != nil {
		return 0, err
	}
	if _, err = tx.Exec(ctx, `UPDATE topup_requests SET status='approved',reviewed_by=$1,updated_at=now() WHERE id=$2`, a.ID, topupID); err != nil {
		return 0, err
	}
	if err = tx.Commit(ctx); err != nil {
		return 0, err
	}
	return balance, nil
}

func (s *Store) WalletLedger(ctx context.Context, a *Actor) ([]map[string]any, error) {
	if a == nil || !a.Enabled {
		return nil, ErrForbidden
	}
	rows, err := s.DB.Query(ctx, `SELECT id,amount_toman,balance_after_toman,entry_type,description,operation_key,created_at FROM wallet_ledger WHERE deployment_id=$1 AND account_id=$2 ORDER BY id DESC LIMIT 100`, a.DeploymentID, a.AccountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id, amount, bal int64
		var typ, desc, key string
		var at time.Time
		if err = rows.Scan(&id, &amount, &bal, &typ, &desc, &key, &at); err != nil {
			return nil, err
		}
		out = append(out, map[string]any{"id": id, "amount_toman": amount, "balance_after_toman": bal, "type": typ, "description": desc, "operation_key": key, "created_at": at})
	}
	return out, rows.Err()
}

func marshalJSON(v any) []byte { b, _ := json.Marshal(v); return b }

var _ = commerce.GiB
