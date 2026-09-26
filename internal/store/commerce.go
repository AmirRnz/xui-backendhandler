package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strings"
	"time"

	"example.com/xui-commerce/backend/internal/commerce"
	"github.com/jackc/pgx/v5"
)

var cleanDisplayName = regexp.MustCompile(`[^A-Za-z0-9-]+`)

type provisionData struct {
	Email                string `json:"email"`
	UUID                 string `json:"uuid"`
	SubID                string `json:"sub_id"`
	DisplayName          string `json:"display_name"`
	PlanName             string `json:"plan_name"`
	Months               int    `json:"months"`
	IPLimit              int    `json:"ip_limit"`
	TrafficLimitBytes    int64  `json:"traffic_limit_bytes"`
	ExpiryTimeMS         int64  `json:"expiry_time_ms"`
	Flow                 string `json:"flow"`
	InboundIDs           []int  `json:"inbound_ids"`
	TelegramID           int64  `json:"telegram_id"`
	ClientUUID           string `json:"client_uuid"`
	PanelID              string `json:"panel_id"`
	OperationKey         string `json:"operation_key"`
	MutationAction       string `json:"mutation_action,omitempty"`
	ExpectedIPLimit      int    `json:"expected_ip_limit,omitempty"`
	ExpectedExpiryTimeMS int64  `json:"expected_expiry_time_ms,omitempty"`
}

func (s *Store) Balance(ctx context.Context, a *Actor) (int64, error) {
	if a == nil || !a.Enabled {
		return 0, ErrForbidden
	}
	var bal int64
	err := s.DB.QueryRow(ctx, `SELECT wallet_balance_toman FROM commercial_accounts WHERE id=$1 AND home_deployment_id=$2 AND status='active'`, a.AccountID, a.DeploymentID).Scan(&bal)
	return bal, err
}

func (s *Store) CreatePurchase(ctx context.Context, a *Actor, quoteID int64, method, opKey, displayName string) (*PurchaseResult, error) {
	if a == nil || !a.Enabled || !validKey(opKey) || quoteID <= 0 {
		return nil, ErrForbidden
	}
	if method != "wallet" && method != "direct" {
		return nil, fmt.Errorf("payment_method must be wallet or direct")
	}
	displayName = sanitizeDisplayName(displayName)
	hash := hashJSON(struct {
		Quote        int64
		Method, Name string
	}{quoteID, method, displayName})
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	var existing PurchaseResult
	var existingHash string
	err = tx.QueryRow(ctx, `SELECT id,status,COALESCE(payment_intent_id,0),COALESCE(subscription_id,0),input_hash
		FROM orders WHERE deployment_id=$1 AND account_id=$2 AND operation_key=$3`, a.DeploymentID, a.AccountID, opKey).
		Scan(&existing.OrderID, &existing.Status, &existing.PaymentIntentID, &existing.SubscriptionID, &existingHash)
	if err == nil {
		if existingHash != hash {
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
	var termsRaw, planRaw []byte
	var amount int64
	var planID int64
	var qMonths, qIP, qData int
	err = tx.QueryRow(ctx, `SELECT terms,plan_snapshot,final_price_toman,plan_id,months,ip_limit,data_gb FROM purchase_quotes
		WHERE id=$1 AND deployment_id=$2 AND account_id=$3 AND actor_id=$4 FOR UPDATE`, quoteID, a.DeploymentID, a.AccountID, a.ID).
		Scan(&termsRaw, &planRaw, &amount, &planID, &qMonths, &qIP, &qData)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	var plan commerce.Plan
	var quote commerce.Quote
	if err = json.Unmarshal(planRaw, &plan); err != nil {
		return nil, fmt.Errorf("decode immutable plan snapshot: %w", err)
	}
	if err = json.Unmarshal(termsRaw, &quote); err != nil {
		return nil, fmt.Errorf("decode immutable quote: %w", err)
	}
	if amount <= 0 || len(plan.InboundIDs) == 0 {
		return nil, fmt.Errorf("quote has no positive price or provisioning inbound")
	}
	provision, err := newProvisionData(plan, quote, a, displayName, opKey)
	if err != nil {
		return nil, err
	}
	var orderID int64
	status := "provisioning"
	if method == "direct" {
		status = "awaiting_payment"
	}
	err = tx.QueryRow(ctx, `INSERT INTO orders(deployment_id,account_id,actor_id,quote_id,operation_key,input_hash,payment_method,status)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8) ON CONFLICT(deployment_id,account_id,operation_key) DO NOTHING RETURNING id`, a.DeploymentID, a.AccountID, a.ID, quoteID, opKey, hash, method, status).Scan(&orderID)
	if errors.Is(err, pgx.ErrNoRows) {
		var prior PurchaseResult
		var priorHash string
		err = tx.QueryRow(ctx, `SELECT id,status,COALESCE(payment_intent_id,0),COALESCE(subscription_id,0),input_hash FROM orders WHERE deployment_id=$1 AND account_id=$2 AND operation_key=$3`, a.DeploymentID, a.AccountID, opKey).Scan(&prior.OrderID, &prior.Status, &prior.PaymentIntentID, &prior.SubscriptionID, &priorHash)
		if err != nil {
			return nil, err
		}
		if priorHash != hash {
			return nil, ErrConflict
		}
		if err = tx.Commit(ctx); err != nil {
			return nil, err
		}
		return &prior, nil
	}
	if err != nil {
		return nil, err
	}
	result := &PurchaseResult{OrderID: orderID, Status: status, AmountToman: amount}
	if method == "direct" {
		terms, _ := json.Marshal(struct {
			Quote     commerce.Quote `json:"quote"`
			Plan      commerce.Plan  `json:"plan"`
			Provision provisionData  `json:"provisioning"`
		}{quote, plan, provision})
		var intentID int64
		err = tx.QueryRow(ctx, `INSERT INTO payment_intents(deployment_id,account_id,actor_id,quote_id,intent_key,input_hash,amount_toman,terms,status)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8,'awaiting_receipt') RETURNING id`, a.DeploymentID, a.AccountID, a.ID, quoteID, opKey, hash, amount, terms).Scan(&intentID)
		if err != nil {
			return nil, err
		}
		if _, err = tx.Exec(ctx, `UPDATE orders SET payment_intent_id=$1 WHERE id=$2`, intentID, orderID); err != nil {
			return nil, err
		}
		result.PaymentIntentID = intentID
		if err = tx.Commit(ctx); err != nil {
			return nil, err
		}
		return result, nil
	}
	if err = chargeWallet(ctx, tx, a, amount, "subscription purchase: "+provision.Email, "wallet:purchase:"+opKey, hash, "order", orderID); err != nil {
		return nil, err
	}
	subID, workID, err := createProvisionWork(ctx, tx, a, plan, provision, "purchase:"+opKey, hash, "paid", nil)
	if err != nil {
		return nil, err
	}
	if _, err = tx.Exec(ctx, `UPDATE orders SET subscription_id=$1 WHERE id=$2`, subID, orderID); err != nil {
		return nil, err
	}
	_ = workID
	result.SubscriptionID = subID
	if err = tx.Commit(ctx); err != nil {
		return nil, err
	}
	return result, nil
}

func (s *Store) SubmitReceipt(ctx context.Context, a *Actor, intentID int64, fileID string) error {
	if a == nil || !a.Enabled || intentID <= 0 || strings.TrimSpace(fileID) == "" || len(fileID) > 512 {
		return ErrForbidden
	}
	tag, err := s.DB.Exec(ctx, `UPDATE payment_intents SET telegram_file_id=$1,status='receipt_submitted',updated_at=now()
		WHERE id=$2 AND deployment_id=$3 AND account_id=$4 AND actor_id=$5 AND status='awaiting_receipt'`, fileID, intentID, a.DeploymentID, a.AccountID, a.ID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 1 {
		return nil
	}
	var existing string
	var status string
	err = s.DB.QueryRow(ctx, `SELECT telegram_file_id,status FROM payment_intents WHERE id=$1 AND deployment_id=$2 AND account_id=$3 AND actor_id=$4`, intentID, a.DeploymentID, a.AccountID, a.ID).Scan(&existing, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if status == "receipt_submitted" && existing == fileID {
		return nil
	}
	return ErrConflict
}

func (s *Store) PendingPayments(ctx context.Context, a *Actor) ([]map[string]any, error) {
	if !isAdmin(a) {
		return nil, ErrForbidden
	}
	rows, err := s.DB.Query(ctx, `SELECT p.id,p.account_id,p.actor_id,COALESCE(actor.telegram_id,0),p.amount_toman,p.status,p.created_at,p.telegram_file_id
		FROM payment_intents p LEFT JOIN actors actor ON actor.id=p.actor_id AND actor.deployment_id=p.deployment_id AND actor.account_id=p.account_id
		WHERE p.deployment_id=$1 AND p.status='receipt_submitted' ORDER BY p.created_at`, a.DeploymentID)
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

const activeRequestPageSize = 100

// ActivePaymentIntents returns a bounded page of this actor's resumable direct-payment requests.
func (s *Store) ActivePaymentIntents(ctx context.Context, a *Actor, beforeID int64) ([]map[string]any, *int64, error) {
	if a == nil || !a.Enabled {
		return nil, nil, ErrForbidden
	}
	if beforeID < 0 {
		return nil, nil, ErrForbidden
	}
	rows, err := s.DB.Query(ctx, `SELECT id,amount_toman,status,created_at FROM payment_intents
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

// RejectPayment closes an unpaid order and records one audited decision. It never settles or provisions it.
func (s *Store) RejectPayment(ctx context.Context, a *Actor, intentID int64) (bool, error) {
	if !isAdmin(a) || intentID <= 0 {
		return false, ErrForbidden
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	var accountID, requesterID, orderID int64
	var amount int64
	var status, orderStatus string
	err = tx.QueryRow(ctx, `SELECT p.account_id,p.actor_id,p.amount_toman,p.status,o.id,o.status FROM payment_intents p JOIN orders o ON o.payment_intent_id=p.id
		WHERE p.id=$1 AND p.deployment_id=$2 FOR UPDATE OF p,o`, intentID, a.DeploymentID).Scan(&accountID, &requesterID, &amount, &status, &orderID, &orderStatus)
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
	if orderStatus != "awaiting_payment" {
		return false, ErrConflict
	}
	if _, err = tx.Exec(ctx, `UPDATE payment_intents SET status='rejected',reviewed_by=$1,review_note='receipt rejected by administrator',updated_at=now() WHERE id=$2`, a.ID, intentID); err != nil {
		return false, err
	}
	orderTag, err := tx.Exec(ctx, `UPDATE orders SET status='cancelled',updated_at=now() WHERE id=$1 AND status='awaiting_payment'`, orderID)
	if err != nil {
		return false, err
	}
	if orderTag.RowsAffected() != 1 {
		return false, ErrConflict
	}
	if _, err = tx.Exec(ctx, `INSERT INTO admin_configuration_audit(deployment_id,actor_id,action,subject_ref) VALUES($1,$2,'payment_rejected',$3)`, a.DeploymentID, a.ID, fmt.Sprintf("payment_intent:%d", intentID)); err != nil {
		return false, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO outbox(deployment_id,account_id,actor_id,dedupe_key,topic,payload) VALUES($1,$2,$3,$4,'payment.rejected',jsonb_build_object('amount_toman',$5::bigint)) ON CONFLICT(deployment_id,dedupe_key) DO NOTHING`, a.DeploymentID, accountID, requesterID, fmt.Sprintf("payment-rejected:%d", intentID), amount); err != nil {
		return false, err
	}
	if err = tx.Commit(ctx); err != nil {
		return false, err
	}
	return false, nil
}

func (s *Store) ApprovePayment(ctx context.Context, a *Actor, intentID int64) (*PurchaseResult, error) {
	if !isAdmin(a) || intentID <= 0 {
		return nil, ErrForbidden
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	var accountID, customerActor, quoteID, orderID, amount int64
	var status string
	var termsRaw []byte
	var intentKey string
	err = tx.QueryRow(ctx, `SELECT p.account_id,p.actor_id,p.quote_id,p.amount_toman,p.status,p.terms,p.intent_key,o.id
		FROM payment_intents p JOIN orders o ON o.payment_intent_id=p.id
		WHERE p.id=$1 AND p.deployment_id=$2 FOR UPDATE OF p,o`, intentID, a.DeploymentID).
		Scan(&accountID, &customerActor, &quoteID, &amount, &status, &termsRaw, &intentKey, &orderID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if status == "approved" {
		var subID int64
		var orderStatus string
		_ = tx.QueryRow(ctx, `SELECT COALESCE(subscription_id,0),status FROM orders WHERE id=$1`, orderID).Scan(&subID, &orderStatus)
		if err = tx.Commit(ctx); err != nil {
			return nil, err
		}
		return &PurchaseResult{OrderID: orderID, Status: orderStatus, SubscriptionID: subID, AmountToman: amount}, nil
	}
	if status != "receipt_submitted" {
		return nil, ErrConflict
	}
	var snapshot struct {
		Action               string         `json:"action"`
		SubscriptionID       int64          `json:"subscription_id"`
		ExpectedStatus       string         `json:"expected_status"`
		ExpectedIPLimit      int            `json:"expected_ip_limit"`
		ExpectedExpiryTimeMS int64          `json:"expected_expiry_time_ms"`
		Quote                commerce.Quote `json:"quote"`
		Plan                 commerce.Plan  `json:"plan"`
		Provision            provisionData  `json:"provisioning"`
	}
	if err = json.Unmarshal(termsRaw, &snapshot); err != nil {
		return nil, err
	}
	customer := &Actor{ID: customerActor, DeploymentID: a.DeploymentID, AccountID: accountID}
	if snapshot.Provision.Email == "" {
		return nil, fmt.Errorf("payment snapshot lacks provisioning data")
	}
	if snapshot.Action == "" && len(snapshot.Plan.InboundIDs) == 0 {
		return nil, fmt.Errorf("payment snapshot lacks provisioning inbounds")
	}
	if snapshot.Action != "" {
		if (snapshot.Action != "extend" && snapshot.Action != "upgrade_ip") || snapshot.SubscriptionID <= 0 ||
			snapshot.Provision.MutationAction != snapshot.Action || snapshot.Quote.FinalPriceToman != amount {
			return nil, fmt.Errorf("payment snapshot has unsupported subscription mutation")
		}
		var currentStatus string
		var currentIP int
		var currentExpiry int64
		err = tx.QueryRow(ctx, `SELECT status,ip_limit,expiry_time_ms FROM subscriptions WHERE id=$1 AND deployment_id=$2 AND account_id=$3 FOR UPDATE`,
			snapshot.SubscriptionID, a.DeploymentID, accountID).Scan(&currentStatus, &currentIP, &currentExpiry)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		if err != nil {
			return nil, err
		}
		if currentStatus != snapshot.ExpectedStatus || currentIP != snapshot.ExpectedIPLimit || currentExpiry != snapshot.ExpectedExpiryTimeMS {
			return nil, ErrConflict
		}
		if snapshot.Action == "upgrade_ip" && (currentStatus != "active" || currentExpiry == 0 ||
			(currentExpiry > 0 && currentExpiry <= time.Now().UTC().UnixMilli()) || snapshot.Provision.IPLimit <= currentIP) {
			return nil, ErrConflict
		}
		if snapshot.Action == "extend" && (snapshot.Provision.Months < 1 || snapshot.Provision.Months > 120 || snapshot.Provision.IPLimit != currentIP) {
			return nil, ErrConflict
		}
		if snapshot.Action == "extend" {
			snapshot.Provision.ExpiryTimeMS, err = extendedExpiry(currentExpiry, snapshot.Provision.Months, time.Now().UTC())
			if err != nil {
				return nil, err
			}
		}
	}
	if _, err = tx.Exec(ctx, `INSERT INTO payment_settlements(deployment_id,account_id,intent_id,amount_toman,status,approved_by,operation_key)
		VALUES($1,$2,$3,$4,'approved',$5,$6) ON CONFLICT(intent_id) DO NOTHING`, a.DeploymentID, accountID, intentID, amount, a.ID, "payment-approval:"+intentKey); err != nil {
		return nil, err
	}
	if _, err = tx.Exec(ctx, `UPDATE payment_intents SET status='approved',reviewed_by=$1,approved_at=now(),updated_at=now() WHERE id=$2`, a.ID, intentID); err != nil {
		return nil, err
	}
	if _, err = tx.Exec(ctx, `UPDATE orders SET status='provisioning',updated_at=now() WHERE id=$1`, orderID); err != nil {
		return nil, err
	}
	if snapshot.Action != "" {
		if _, err = tx.Exec(ctx, `UPDATE subscriptions SET status='provisioning',updated_at=now() WHERE id=$1 AND status=$2`, snapshot.SubscriptionID, snapshot.ExpectedStatus); err != nil {
			return nil, err
		}
		if err = createSubscriptionMutationWork(ctx, tx, customer, snapshot.Provision.PanelID, snapshot.SubscriptionID, intentKey, hashJSON(snapshot), snapshot.Provision, &intentID); err != nil {
			return nil, err
		}
	} else {
		_, workID, workErr := createProvisionWork(ctx, tx, customer, snapshot.Plan, snapshot.Provision, "purchase:"+intentKey, hashJSON(snapshot), "paid", &intentID)
		if workErr != nil {
			return nil, workErr
		}
		_ = workID
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, err
	}
	return &PurchaseResult{OrderID: orderID, Status: "provisioning", AmountToman: amount}, nil
}

func (s *Store) Subscriptions(ctx context.Context, a *Actor) ([]Subscription, error) {
	if a == nil || !a.Enabled {
		return nil, ErrForbidden
	}
	rows, err := s.DB.Query(ctx, `SELECT id,client_email,display_name,COALESCE(plan_id,0),status,source_kind,ip_limit,traffic_limit_bytes,expiry_time_ms,subscription_links::text,created_at
		FROM subscriptions WHERE deployment_id=$1 AND account_id=$2 ORDER BY created_at DESC LIMIT 100`, a.DeploymentID, a.AccountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Subscription{}
	for rows.Next() {
		var x Subscription
		var links string
		if err = rows.Scan(&x.ID, &x.Email, &x.DisplayName, &x.PlanID, &x.Status, &x.Kind, &x.IPLimit, &x.TrafficLimitBytes, &x.ExpiryTimeMS, &links, &x.CreatedAt); err != nil {
			return nil, err
		}
		if err = json.Unmarshal([]byte(links), &x.Links); err != nil {
			return nil, err
		}
		if x.Links == nil {
			x.Links = []string{}
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

func newProvisionData(p commerce.Plan, q commerce.Quote, a *Actor, name, key string) (provisionData, error) {
	clean := sanitizeDisplayName(name)
	if clean == "" {
		clean = "service"
	}
	suffix, err := randomHex(3)
	if err != nil {
		return provisionData{}, err
	}
	id, err := randomHex(16)
	if err != nil {
		return provisionData{}, err
	}
	sub, err := randomHex(12)
	if err != nil {
		return provisionData{}, err
	}
	months := q.Months
	if months <= 0 {
		return provisionData{}, fmt.Errorf("invalid duration")
	}
	ms, err := multiply(int64(months), int64(30*24*60*60*1000))
	if err != nil || ms == math.MinInt64 {
		return provisionData{}, fmt.Errorf("expiry duration overflow")
	}
	bytes := int64(0)
	if p.IsLimited {
		bytes, err = multiply(int64(q.DataGB), commerce.GiB)
		if err != nil {
			return provisionData{}, err
		}
	}
	return provisionData{Email: clean + "_" + suffix, UUID: formatUUID(id), SubID: sub, DisplayName: clean, PlanName: p.Name, Months: months, IPLimit: q.IPLimit, TrafficLimitBytes: bytes,
		ExpiryTimeMS: -ms, Flow: strings.TrimSpace(p.Flow), InboundIDs: validInbounds(p.InboundIDs), TelegramID: a.TelegramID, ClientUUID: formatUUID(id), PanelID: p.PanelID, OperationKey: key}, nil
}

func sanitizeDisplayName(s string) string {
	s = cleanDisplayName.ReplaceAllString(strings.TrimSpace(s), "-")
	s = strings.Trim(s, "-")
	if len(s) > 48 {
		s = s[:48]
	}
	return s
}
func multiply(a, b int64) (int64, error) {
	if a < 0 || b < 0 || a != 0 && b > math.MaxInt64/a {
		return 0, fmt.Errorf("integer limit overflow")
	}
	return a * b, nil
}
func validInbounds(ids []int) []int {
	seen := map[int]bool{}
	out := []int{}
	for _, id := range ids {
		if id > 0 && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}
func formatUUID(s string) string {
	return s[:8] + "-" + s[8:12] + "-4" + s[13:16] + "-a" + s[17:20] + "-" + s[20:]
}

func chargeWallet(ctx context.Context, tx pgx.Tx, a *Actor, amount int64, description, key, inputHash, refType string, refID int64) error {
	if amount <= 0 || !validKey(key) {
		return fmt.Errorf("invalid wallet debit")
	}
	var balance int64
	err := tx.QueryRow(ctx, `UPDATE commercial_accounts SET wallet_balance_toman=wallet_balance_toman-$1,updated_at=now()
		WHERE id=$2 AND home_deployment_id=$3 AND wallet_balance_toman >= $1 RETURNING wallet_balance_toman`, amount, a.AccountID, a.DeploymentID).Scan(&balance)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrInsufficientFunds
	}
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO wallet_ledger(deployment_id,account_id,actor_id,amount_toman,balance_after_toman,entry_type,description,operation_key,input_hash,reference_type,reference_id)
		VALUES($1,$2,$3,$4,$5,'debit',$6,$7,$8,$9,$10)`, a.DeploymentID, a.AccountID, a.ID, -amount, balance, description, key, inputHash, refType, refID)
	return err
}

func createProvisionWork(ctx context.Context, tx pgx.Tx, a *Actor, p commerce.Plan, d provisionData, opKey, inputHash, kind string, intentID *int64) (int64, int64, error) {
	ins := validInbounds(d.InboundIDs)
	if len(ins) == 0 {
		return 0, 0, fmt.Errorf("plan has no valid inbound IDs")
	}
	d.InboundIDs = ins
	var subID, workID int64
	err := tx.QueryRow(ctx, `INSERT INTO subscriptions(deployment_id,account_id,actor_id,panel_id,plan_id,source_kind,status,client_email,client_uuid,sub_id,display_name,ip_limit,traffic_limit_bytes,expiry_time_ms,flow,inbound_ids)
		VALUES($1,$2,$3,$4,$5,$6,'provisioning',$7,$8,$9,$10,$11,$12,$13,$14,$15) RETURNING id`, a.DeploymentID, a.AccountID, a.ID, d.PanelID, p.ID, kind, d.Email, d.ClientUUID, d.SubID, d.DisplayName, d.IPLimit, d.TrafficLimitBytes, d.ExpiryTimeMS, d.Flow, ins).Scan(&subID)
	if err != nil {
		return 0, 0, err
	}
	payload, _ := json.Marshal(d)
	err = tx.QueryRow(ctx, `INSERT INTO work_items(deployment_id,account_id,actor_id,panel_id,subscription_id,payment_intent_id,operation_key,input_hash,kind,desired_state)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,'provision_add',$9) RETURNING id`, a.DeploymentID, a.AccountID, a.ID, d.PanelID, subID, intentID, opKey, inputHash, payload).Scan(&workID)
	if err != nil {
		return 0, 0, err
	}
	return subID, workID, nil
}

func isAdmin(a *Actor) bool {
	return a != nil && a.Enabled && (a.Role == "admin" || a.Role == "operator")
}
