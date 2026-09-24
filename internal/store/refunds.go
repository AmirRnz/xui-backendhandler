package store

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
)

type RefundRequest struct {
	ID                   int64  `json:"id"`
	SubscriptionID       int64  `json:"subscription_id"`
	Status               string `json:"status"`
	SuggestedAmountToman int64  `json:"suggested_amount_toman"`
	RefundableCapToman   int64  `json:"refundable_cap_toman"`
	ApprovedAmountToman  int64  `json:"approved_amount_toman,omitempty"`
	Reason               string `json:"reason"`
}

func (s *Store) RequestRefund(ctx context.Context, a *Actor, subscriptionID int64, key, reason string) (*RefundRequest, error) {
	if a == nil || !a.Enabled || subscriptionID <= 0 || !validKey(key) {
		return nil, ErrForbidden
	}
	reason = strings.TrimSpace(reason)
	if len(reason) > 500 {
		reason = reason[:500]
	}
	inputHash := hashJSON(struct {
		SubscriptionID int64
		Reason         string
	}{subscriptionID, reason})
	opKey := "refund-request:" + key
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	var old RefundRequest
	var oldHash string
	err = tx.QueryRow(ctx, `SELECT id,subscription_id,status,suggested_amount_toman,refundable_cap_toman,COALESCE(approved_amount_toman,0),reason,input_hash FROM refund_requests WHERE deployment_id=$1 AND account_id=$2 AND operation_key=$3`, a.DeploymentID, a.AccountID, opKey).
		Scan(&old.ID, &old.SubscriptionID, &old.Status, &old.SuggestedAmountToman, &old.RefundableCapToman, &old.ApprovedAmountToman, &old.Reason, &oldHash)
	if err == nil {
		if oldHash != inputHash {
			return nil, ErrConflict
		}
		if err = tx.Commit(ctx); err != nil {
			return nil, err
		}
		return &old, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	var status, sourceKind string
	err = tx.QueryRow(ctx, `SELECT status,source_kind FROM subscriptions WHERE id=$1 AND deployment_id=$2 AND account_id=$3 FOR UPDATE`, subscriptionID, a.DeploymentID, a.AccountID).Scan(&status, &sourceKind)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if sourceKind == "test" {
		return nil, fmt.Errorf("test subscriptions are not refundable")
	}
	var cap int64
	err = tx.QueryRow(ctx, `SELECT COALESCE(max(q.final_price_toman),0) FROM orders o JOIN purchase_quotes q ON q.id=o.quote_id WHERE o.subscription_id=$1 AND o.account_id=$2 AND o.status='completed'`, subscriptionID, a.AccountID).Scan(&cap)
	if err != nil {
		return nil, err
	}
	var out RefundRequest
	out.SubscriptionID = subscriptionID
	out.Status = "pending"
	out.RefundableCapToman = cap
	out.SuggestedAmountToman = cap
	out.Reason = reason
	err = tx.QueryRow(ctx, `INSERT INTO refund_requests(deployment_id,account_id,actor_id,subscription_id,operation_key,input_hash,suggested_amount_toman,refundable_cap_toman,reason)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9) RETURNING id`, a.DeploymentID, a.AccountID, a.ID, subscriptionID, opKey, inputHash, cap, cap, reason).Scan(&out.ID)
	if err != nil {
		if strings.Contains(err.Error(), "refund_requests_subscription_id_key") {
			return nil, ErrConflict
		}
		return nil, err
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, err
	}
	return &out, nil
}

func (s *Store) PendingRefunds(ctx context.Context, a *Actor) ([]RefundRequest, error) {
	if !isAdmin(a) {
		return nil, ErrForbidden
	}
	rows, err := s.DB.Query(ctx, `SELECT id,subscription_id,status,suggested_amount_toman,refundable_cap_toman,reason FROM refund_requests WHERE deployment_id=$1 AND status='pending' ORDER BY created_at`, a.DeploymentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []RefundRequest{}
	for rows.Next() {
		var r RefundRequest
		if err = rows.Scan(&r.ID, &r.SubscriptionID, &r.Status, &r.SuggestedAmountToman, &r.RefundableCapToman, &r.Reason); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// RejectRefund closes a pending refund request without restoring a subscription or crediting a wallet.
func (s *Store) RejectRefund(ctx context.Context, a *Actor, requestID int64) (bool, error) {
	if !isAdmin(a) || requestID <= 0 {
		return false, ErrForbidden
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	var accountID, requesterID int64
	var status string
	err = tx.QueryRow(ctx, `SELECT account_id,actor_id,status FROM refund_requests WHERE id=$1 AND deployment_id=$2 FOR UPDATE`, requestID, a.DeploymentID).Scan(&accountID, &requesterID, &status)
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
	if status != "pending" {
		return false, ErrConflict
	}
	if _, err = tx.Exec(ctx, `UPDATE refund_requests SET status='rejected',audit_note='refund request rejected by administrator',updated_at=now() WHERE id=$1`, requestID); err != nil {
		return false, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO admin_configuration_audit(deployment_id,actor_id,action,subject_ref) VALUES($1,$2,'refund_rejected',$3)`, a.DeploymentID, a.ID, fmt.Sprintf("refund:%d", requestID)); err != nil {
		return false, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO outbox(deployment_id,account_id,actor_id,dedupe_key,topic,payload) VALUES($1,$2,$3,$4,'refund.rejected','{}'::jsonb) ON CONFLICT(deployment_id,dedupe_key) DO NOTHING`, a.DeploymentID, accountID, requesterID, fmt.Sprintf("refund-rejected:%d", requestID)); err != nil {
		return false, err
	}
	if err = tx.Commit(ctx); err != nil {
		return false, err
	}
	return false, nil
}

func (s *Store) ApproveRefund(ctx context.Context, a *Actor, requestID, amount int64, auditNote, key string, manualOverride bool) (int64, error) {
	if !isAdmin(a) || requestID <= 0 || amount <= 0 || !validKey(key) {
		return 0, ErrForbidden
	}
	auditNote = strings.TrimSpace(auditNote)
	if len(auditNote) < 8 || len(auditNote) > 500 {
		return 0, fmt.Errorf("refund audit note must be 8 to 500 characters")
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	var account, subID, cap int64
	var status string
	opKey := "refund:" + key
	inputHash := hashJSON(struct {
		RequestID, Amount int64
		Note              string
		Override          bool
	}{requestID, amount, auditNote, manualOverride})
	err = tx.QueryRow(ctx, `SELECT account_id,subscription_id,refundable_cap_toman,status
		FROM refund_requests WHERE id=$1 AND deployment_id=$2 FOR UPDATE`, requestID, a.DeploymentID).Scan(&account, &subID, &cap, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, err
	}
	if status == "approved" {
		var oldAmount int64
		var oldNote, oldKey, oldHash string
		var oldOverride bool
		if err = tx.QueryRow(ctx, `SELECT amount_toman,audit_note,operation_key,input_hash,manual_override FROM refund_approvals WHERE request_id=$1`, requestID).Scan(&oldAmount, &oldNote, &oldKey, &oldHash, &oldOverride); err != nil {
			return 0, err
		}
		if oldAmount != amount || oldNote != auditNote || oldOverride != manualOverride {
			return 0, ErrConflict
		}
		if oldKey == opKey && oldHash != inputHash {
			return 0, ErrConflict
		}
		var bal int64
		err = tx.QueryRow(ctx, `SELECT wallet_balance_toman FROM commercial_accounts WHERE id=$1`, account).Scan(&bal)
		if err == nil {
			err = tx.Commit(ctx)
		}
		return bal, err
	}
	if status != "pending" {
		return 0, ErrConflict
	}
	if cap > 0 && amount > cap {
		return 0, fmt.Errorf("approved refund exceeds immutable paid amount")
	}
	if cap == 0 && (!manualOverride || len(auditNote) < 8) {
		return 0, fmt.Errorf("historical refund has no immutable quote; explicit manual override and audit note are required")
	}
	if cap > 0 && manualOverride {
		return 0, fmt.Errorf("manual override is only valid when legacy paid terms are unavailable")
	}
	var subStatus string
	if err = tx.QueryRow(ctx, `SELECT status FROM subscriptions WHERE id=$1 AND account_id=$2 FOR UPDATE`, subID, account).Scan(&subStatus); err != nil {
		return 0, err
	}
	if subStatus != "cancelled" {
		return 0, fmt.Errorf("refund approval requires verified subscription cancellation")
	}
	var existingAmount int64
	err = tx.QueryRow(ctx, `SELECT amount_toman FROM wallet_ledger WHERE deployment_id=$1 AND account_id=$2 AND operation_key=$3`, a.DeploymentID, account, opKey).Scan(&existingAmount)
	if err == nil {
		return 0, ErrConflict
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return 0, err
	}
	var balance int64
	err = tx.QueryRow(ctx, `UPDATE commercial_accounts SET wallet_balance_toman=wallet_balance_toman+$1,updated_at=now() WHERE id=$2 AND home_deployment_id=$3 AND wallet_balance_toman<=9223372036854775807-$1 RETURNING wallet_balance_toman`, amount, account, a.DeploymentID).Scan(&balance)
	if err != nil {
		return 0, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO wallet_ledger(deployment_id,account_id,actor_id,amount_toman,balance_after_toman,entry_type,description,operation_key,input_hash,reference_type,reference_id)
		VALUES($1,$2,$3,$4,$5,'credit',$6,$7,$8,'refund_request',$9)`, a.DeploymentID, account, a.ID, amount, balance, "approved refund for subscription "+fmt.Sprint(subID), opKey, inputHash, requestID)
	if err != nil {
		return 0, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO refund_approvals(request_id,deployment_id,account_id,amount_toman,approved_by,audit_note,manual_override,operation_key,input_hash) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`, requestID, a.DeploymentID, account, amount, a.ID, auditNote, manualOverride, opKey, inputHash)
	if err != nil {
		return 0, err
	}
	_, err = tx.Exec(ctx, `UPDATE refund_requests SET status='approved',approved_amount_toman=$1,approved_by=$2,audit_note=$3,manual_override=$4,updated_at=now() WHERE id=$5`, amount, a.ID, auditNote, manualOverride, requestID)
	if err != nil {
		return 0, err
	}
	var actorID int64
	if err = tx.QueryRow(ctx, `SELECT actor_id FROM refund_requests WHERE id=$1`, requestID).Scan(&actorID); err != nil {
		return 0, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO outbox(deployment_id,account_id,actor_id,dedupe_key,topic,payload) VALUES($1,$2,$3,$4,'refund.approved',jsonb_build_object('amount_toman',$5::bigint)) ON CONFLICT(deployment_id,dedupe_key) DO NOTHING`, a.DeploymentID, account, actorID, "refund-approved:"+fmt.Sprint(requestID), amount); err != nil {
		return 0, err
	}
	if err = tx.Commit(ctx); err != nil {
		return 0, err
	}
	return balance, nil
}
