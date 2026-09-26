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

const mutationMonthMillis int64 = 30 * 24 * 60 * 60 * 1000

type SubscriptionMutationQuote struct {
	commerce.Quote
	Action            string `json:"action"`
	SubscriptionID    int64  `json:"subscription_id"`
	CurrentIPLimit    int    `json:"current_ip_limit"`
	CurrentExpiryTime int64  `json:"current_expiry_time_ms"`
	DesiredExpiryTime int64  `json:"desired_expiry_time_ms"`
}

type mutationQuoteTerms struct {
	Action               string         `json:"action"`
	SubscriptionID       int64          `json:"subscription_id"`
	Months               int            `json:"months"`
	IPLimit              int            `json:"ip_limit"`
	RequestedMonths      int            `json:"requested_months"`
	RequestedIPLimit     int            `json:"requested_ip_limit"`
	ExpectedStatus       string         `json:"expected_status"`
	ExpectedIPLimit      int            `json:"expected_ip_limit"`
	ExpectedExpiryTimeMS int64          `json:"expected_expiry_time_ms"`
	DesiredExpiryTimeMS  int64          `json:"desired_expiry_time_ms"`
	Quote                commerce.Quote `json:"quote"`
	Plan                 commerce.Plan  `json:"plan"`
}

type mutationPaymentSnapshot struct {
	Action               string         `json:"action"`
	SubscriptionID       int64          `json:"subscription_id"`
	ExpectedStatus       string         `json:"expected_status"`
	ExpectedIPLimit      int            `json:"expected_ip_limit"`
	ExpectedExpiryTimeMS int64          `json:"expected_expiry_time_ms"`
	Quote                commerce.Quote `json:"quote"`
	Plan                 commerce.Plan  `json:"plan"`
	Provision            provisionData  `json:"provisioning"`
}

type ownedSubscriptionMutation struct {
	ID                int64
	PlanID            int64
	PanelID           string
	SourceKind        string
	Status            string
	Email             string
	UUID              string
	SubID             string
	DisplayName       string
	IPLimit           int
	TrafficLimitBytes int64
	ExpiryTimeMS      int64
	Flow              string
	InboundIDs        []int
}

// CreateSubscriptionMutationQuote stores an immutable, account-scoped quote
// for renewing a service or raising its fake device cap.
func (s *Store) CreateSubscriptionMutationQuote(ctx context.Context, a *Actor, subscriptionID int64, action string, months, ipLimit int, key string) (*SubscriptionMutationQuote, error) {
	if a == nil || !a.Enabled || subscriptionID <= 0 || !validKey(key) || len(key) > 140 {
		return nil, ErrForbidden
	}
	if action != "extend" && action != "upgrade_ip" {
		return nil, fmt.Errorf("unsupported subscription mutation")
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	var existingID, existingActor int64
	var existingTerms []byte
	err = tx.QueryRow(ctx, `SELECT id,actor_id,terms FROM purchase_quotes WHERE deployment_id=$1 AND account_id=$2 AND quote_key=$3`, a.DeploymentID, a.AccountID, "mutation-quote:"+key).Scan(&existingID, &existingActor, &existingTerms)
	if err == nil {
		if existingActor != a.ID {
			return nil, ErrNotFound
		}
		var terms mutationQuoteTerms
		if err = json.Unmarshal(existingTerms, &terms); err != nil {
			return nil, err
		}
		if terms.Action != action || terms.SubscriptionID != subscriptionID || terms.RequestedMonths != months || terms.RequestedIPLimit != ipLimit {
			return nil, ErrConflict
		}
		if err = tx.Commit(ctx); err != nil {
			return nil, err
		}
		return mutationQuoteResult(existingID, terms), nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	sub, err := ownedSubscriptionForMutation(ctx, tx, a, subscriptionID, true)
	if err != nil {
		return nil, err
	}
	// If another identical request held this row lock first, reuse its quote
	// instead of treating the uniqueness collision as a fresh failure.
	err = tx.QueryRow(ctx, `SELECT id,actor_id,terms FROM purchase_quotes WHERE deployment_id=$1 AND account_id=$2 AND quote_key=$3`, a.DeploymentID, a.AccountID, "mutation-quote:"+key).Scan(&existingID, &existingActor, &existingTerms)
	if err == nil {
		if existingActor != a.ID {
			return nil, ErrNotFound
		}
		var terms mutationQuoteTerms
		if err = json.Unmarshal(existingTerms, &terms); err != nil {
			return nil, err
		}
		if terms.Action != action || terms.SubscriptionID != subscriptionID || terms.RequestedMonths != months || terms.RequestedIPLimit != ipLimit {
			return nil, ErrConflict
		}
		if err = tx.Commit(ctx); err != nil {
			return nil, err
		}
		return mutationQuoteResult(existingID, terms), nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	if sub.SourceKind != "paid" || (sub.Status != "active" && sub.Status != "expired") {
		return nil, ErrConflict
	}
	plan, err := planByID(ctx, tx, a.DeploymentID, a.AccountID, sub.PlanID)
	if err != nil {
		return nil, err
	}
	if plan.Kind != "paid" || !plan.Enabled || plan.PanelID != sub.PanelID {
		return nil, ErrConflict
	}
	inbounds := validInbounds(sub.InboundIDs)
	if len(inbounds) == 0 {
		return nil, fmt.Errorf("subscription has no valid inbounds")
	}
	plan.InboundIDs = inbounds
	dataGB, err := mutationDataGB(plan, sub.TrafficLimitBytes)
	if err != nil {
		return nil, err
	}
	terms := mutationQuoteTerms{
		Action: action, SubscriptionID: sub.ID, Months: months, IPLimit: ipLimit,
		RequestedMonths: months, RequestedIPLimit: ipLimit,
		ExpectedStatus: sub.Status, ExpectedIPLimit: sub.IPLimit,
		ExpectedExpiryTimeMS: sub.ExpiryTimeMS, Plan: plan,
	}
	var q commerce.Quote
	switch action {
	case "extend":
		if months < 1 || months > 120 || ipLimit != sub.IPLimit {
			return nil, fmt.Errorf("invalid extension terms")
		}
		if sub.ExpiryTimeMS == 0 {
			return nil, fmt.Errorf("an unlimited-duration service cannot be renewed")
		}
		q, err = commerce.CalculateQuote(plan, months, sub.IPLimit, dataGB)
		if err == nil {
			terms.DesiredExpiryTimeMS, err = extendedExpiry(sub.ExpiryTimeMS, months, time.Now().UTC())
		}
	case "upgrade_ip":
		if months != 0 || ipLimit <= sub.IPLimit || sub.Status != "active" || sub.ExpiryTimeMS == 0 ||
			(sub.ExpiryTimeMS > 0 && sub.ExpiryTimeMS <= time.Now().UTC().UnixMilli()) ||
			plan.MaxIPLimit <= 0 || ipLimit > plan.MaxIPLimit {
			return nil, fmt.Errorf("invalid device-limit upgrade terms")
		}
		periodMonths := remainingMonths(sub.ExpiryTimeMS, time.Now().UTC())
		oldQuote, oldErr := commerce.CalculateQuote(plan, periodMonths, sub.IPLimit, dataGB)
		newQuote, newErr := commerce.CalculateQuote(plan, periodMonths, ipLimit, dataGB)
		if oldErr != nil {
			err = oldErr
		} else if newErr != nil {
			err = newErr
		} else {
			amount := newQuote.FinalPriceToman - oldQuote.FinalPriceToman
			if amount <= 0 {
				err = fmt.Errorf("device-limit upgrade has no positive price")
			} else {
				q = newQuote
				q.Months = periodMonths
				q.DurationDays = periodMonths * 30
				q.BasePriceToman = 0
				q.ExtraIPPriceToman = amount
				q.ExtraMonthPriceToman = 0
				q.TrafficPriceToman = 0
				q.DiscountToman = 0
				q.FinalPriceToman = amount
				terms.Months = periodMonths
				terms.DesiredExpiryTimeMS = sub.ExpiryTimeMS
			}
		}
	}
	if err != nil {
		return nil, err
	}
	if q.FinalPriceToman <= 0 {
		return nil, fmt.Errorf("mutation price must be positive")
	}
	terms.Quote = q
	termsJSON, err := json.Marshal(terms)
	if err != nil {
		return nil, err
	}
	planJSON, err := json.Marshal(plan)
	if err != nil {
		return nil, err
	}
	var id int64
	err = tx.QueryRow(ctx, `INSERT INTO purchase_quotes(deployment_id,account_id,actor_id,quote_key,plan_id,plan_snapshot,terms,months,ip_limit,data_gb,final_price_toman)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11) ON CONFLICT(deployment_id,account_id,quote_key) DO NOTHING RETURNING id`,
		a.DeploymentID, a.AccountID, a.ID, "mutation-quote:"+key, sub.PlanID, planJSON, termsJSON, terms.Months, ipLimit, dataGB, q.FinalPriceToman).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrConflict
	}
	if err != nil {
		return nil, err
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, err
	}
	return mutationQuoteResult(id, terms), nil
}

func mutationQuoteResult(id int64, terms mutationQuoteTerms) *SubscriptionMutationQuote {
	q := terms.Quote
	q.ID = id
	return &SubscriptionMutationQuote{Quote: q, Action: terms.Action, SubscriptionID: terms.SubscriptionID,
		CurrentIPLimit: terms.ExpectedIPLimit, CurrentExpiryTime: terms.ExpectedExpiryTimeMS, DesiredExpiryTime: terms.DesiredExpiryTimeMS}
}

func mutationDataGB(plan commerce.Plan, trafficBytes int64) (int, error) {
	if plan.IsLimited {
		if trafficBytes <= 0 || trafficBytes%commerce.GiB != 0 {
			return 0, fmt.Errorf("service traffic limit does not match its plan")
		}
		gb := int(trafficBytes / commerce.GiB)
		if gb < plan.MinDataGB || (plan.MaxDataBytes > 0 && trafficBytes > plan.MaxDataBytes) {
			return 0, fmt.Errorf("service traffic limit is outside its plan")
		}
		return gb, nil
	}
	if trafficBytes != 0 {
		return 0, fmt.Errorf("service traffic limit does not match its plan")
	}
	return 0, nil
}

func extendedExpiry(expiryMS int64, months int, now time.Time) (int64, error) {
	if expiryMS == 0 || months < 1 || months > 120 || int64(months) > math.MaxInt64/mutationMonthMillis {
		return 0, fmt.Errorf("invalid service expiry or extension duration")
	}
	added := int64(months) * mutationMonthMillis
	if expiryMS < 0 {
		if expiryMS < math.MinInt64+added {
			return 0, fmt.Errorf("service expiry overflow")
		}
		return expiryMS - added, nil
	}
	base := expiryMS
	if nowMS := now.UnixMilli(); base < nowMS {
		base = nowMS
	}
	if base > math.MaxInt64-added {
		return 0, fmt.Errorf("service expiry overflow")
	}
	return base + added, nil
}

func remainingMonths(expiryMS int64, now time.Time) int {
	const cap = 120
	remaining := expiryMS - now.UnixMilli()
	if remaining <= 0 {
		return 1
	}
	if remaining > cap*mutationMonthMillis {
		return cap
	}
	months := (remaining + mutationMonthMillis - 1) / mutationMonthMillis
	if months > cap {
		return cap
	}
	return int(months)
}

func ownedSubscriptionForMutation(ctx context.Context, tx pgx.Tx, a *Actor, id int64, lock bool) (*ownedSubscriptionMutation, error) {
	query := `SELECT COALESCE(plan_id,0),panel_id,source_kind,status,client_email,client_uuid,sub_id,display_name,ip_limit,traffic_limit_bytes,expiry_time_ms,flow,inbound_ids
		FROM subscriptions WHERE id=$1 AND deployment_id=$2 AND account_id=$3`
	if lock {
		query += ` FOR UPDATE`
	}
	sub := &ownedSubscriptionMutation{}
	err := tx.QueryRow(ctx, query, id, a.DeploymentID, a.AccountID).Scan(&sub.PlanID, &sub.PanelID, &sub.SourceKind, &sub.Status,
		&sub.Email, &sub.UUID, &sub.SubID, &sub.DisplayName, &sub.IPLimit, &sub.TrafficLimitBytes, &sub.ExpiryTimeMS, &sub.Flow, &sub.InboundIDs)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	sub.ID = id
	return sub, nil
}

func existingSubscriptionMutationOrder(ctx context.Context, tx pgx.Tx, a *Actor, operationKey string) (*PurchaseResult, string, error) {
	var result PurchaseResult
	var hash string
	err := tx.QueryRow(ctx, `SELECT o.id,o.status,COALESCE(o.payment_intent_id,0),COALESCE(o.subscription_id,0),o.input_hash,q.final_price_toman
		FROM orders o JOIN purchase_quotes q ON q.id=o.quote_id WHERE o.deployment_id=$1 AND o.account_id=$2 AND o.actor_id=$3 AND o.operation_key=$4`,
		a.DeploymentID, a.AccountID, a.ID, operationKey).Scan(&result.OrderID, &result.Status, &result.PaymentIntentID, &result.SubscriptionID, &hash, &result.AmountToman)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, "", nil
	}
	if err != nil {
		return nil, "", err
	}
	return &result, hash, nil
}

// CreateSubscriptionMutation takes payment and durably records the requested
// full-row panel update. The panel is never called from this transaction/API.
func (s *Store) CreateSubscriptionMutation(ctx context.Context, a *Actor, subscriptionID, quoteID int64, method, key string) (*PurchaseResult, error) {
	if a == nil || !a.Enabled || subscriptionID <= 0 || quoteID <= 0 || !validKey(key) || len(key) > 140 {
		return nil, ErrForbidden
	}
	if method != "wallet" && method != "direct" {
		return nil, fmt.Errorf("payment_method must be wallet or direct")
	}
	operationKey := "subscription-mutation:" + key
	hash := hashJSON(struct {
		SubscriptionID, QuoteID int64
		Method                  string
	}{subscriptionID, quoteID, method})
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	existing, existingHash, err := existingSubscriptionMutationOrder(ctx, tx, a, operationKey)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		if existingHash != hash {
			return nil, ErrConflict
		}
		if err = tx.Commit(ctx); err != nil {
			return nil, err
		}
		return existing, nil
	}
	sub, err := ownedSubscriptionForMutation(ctx, tx, a, subscriptionID, true)
	if err != nil {
		return nil, err
	}
	// A concurrent retry may have waited for this row lock while the first call
	// committed. Re-read its order after the lock to preserve idempotent replay.
	existing, existingHash, err = existingSubscriptionMutationOrder(ctx, tx, a, operationKey)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		if existingHash != hash {
			return nil, ErrConflict
		}
		if err = tx.Commit(ctx); err != nil {
			return nil, err
		}
		return existing, nil
	}
	if sub.SourceKind != "paid" || (sub.Status != "active" && sub.Status != "expired") {
		return nil, ErrConflict
	}
	var pending bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM orders WHERE deployment_id=$1 AND account_id=$2 AND subscription_id=$3 AND status IN ('awaiting_payment','provisioning','manual_review'))`, a.DeploymentID, a.AccountID, subscriptionID).Scan(&pending); err != nil {
		return nil, err
	}
	if pending {
		return nil, ErrConflict
	}
	var quoteRaw, planRaw []byte
	var amount int64
	var quoteActor int64
	err = tx.QueryRow(ctx, `SELECT q.terms,q.plan_snapshot,q.final_price_toman,q.actor_id FROM purchase_quotes q
		WHERE q.id=$1 AND q.deployment_id=$2 AND q.account_id=$3 FOR UPDATE`, quoteID, a.DeploymentID, a.AccountID).Scan(&quoteRaw, &planRaw, &amount, &quoteActor)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if quoteActor != a.ID {
		return nil, ErrNotFound
	}
	var terms mutationQuoteTerms
	if err = json.Unmarshal(quoteRaw, &terms); err != nil {
		return nil, fmt.Errorf("decode subscription mutation quote: %w", err)
	}
	if terms.SubscriptionID != subscriptionID || (terms.Action != "extend" && terms.Action != "upgrade_ip") ||
		terms.ExpectedStatus != sub.Status || terms.ExpectedIPLimit != sub.IPLimit || terms.ExpectedExpiryTimeMS != sub.ExpiryTimeMS ||
		terms.Quote.FinalPriceToman != amount {
		return nil, ErrConflict
	}
	if terms.Action == "upgrade_ip" && (sub.Status != "active" || sub.ExpiryTimeMS == 0 ||
		(sub.ExpiryTimeMS > 0 && sub.ExpiryTimeMS <= time.Now().UTC().UnixMilli()) || terms.IPLimit <= sub.IPLimit) {
		return nil, ErrConflict
	}
	if terms.Action == "extend" && (terms.Months < 1 || terms.Months > 120 || terms.IPLimit != sub.IPLimit) {
		return nil, ErrConflict
	}
	var plan commerce.Plan
	if err = json.Unmarshal(planRaw, &plan); err != nil {
		return nil, err
	}
	if amount <= 0 || len(validInbounds(sub.InboundIDs)) == 0 {
		return nil, fmt.Errorf("mutation quote has invalid price or subscription inbounds")
	}
	data := provisionData{
		Email: sub.Email, UUID: sub.UUID, SubID: sub.SubID, DisplayName: sub.DisplayName,
		PlanName: plan.Name, Months: terms.Months, IPLimit: terms.IPLimit,
		TrafficLimitBytes: sub.TrafficLimitBytes, ExpiryTimeMS: terms.DesiredExpiryTimeMS,
		Flow: sub.Flow, InboundIDs: validInbounds(sub.InboundIDs), TelegramID: a.TelegramID,
		ClientUUID: sub.UUID, PanelID: sub.PanelID, OperationKey: operationKey,
		MutationAction: terms.Action, ExpectedIPLimit: sub.IPLimit, ExpectedExpiryTimeMS: sub.ExpiryTimeMS,
	}
	if terms.Action == "extend" {
		// Start a renewal from the later of the current expiry or payment time.
		// The quote fixes its price and duration; this avoids consuming term while
		// the customer is confirming payment or an admin reviews a receipt.
		data.ExpiryTimeMS, err = extendedExpiry(sub.ExpiryTimeMS, terms.Months, time.Now().UTC())
		if err != nil {
			return nil, err
		}
	}
	result := &PurchaseResult{Status: "provisioning", SubscriptionID: subscriptionID, AmountToman: amount}
	orderStatus := "provisioning"
	if method == "direct" {
		orderStatus = "awaiting_payment"
	}
	err = tx.QueryRow(ctx, `INSERT INTO orders(deployment_id,account_id,actor_id,quote_id,operation_key,input_hash,payment_method,status,subscription_id)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9) ON CONFLICT(deployment_id,account_id,operation_key) DO NOTHING RETURNING id`,
		a.DeploymentID, a.AccountID, a.ID, quoteID, operationKey, hash, method, orderStatus, subscriptionID).Scan(&result.OrderID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrConflict
	}
	if err != nil {
		return nil, err
	}
	if method == "direct" {
		snapshot := mutationPaymentSnapshot{Action: terms.Action, SubscriptionID: subscriptionID, ExpectedStatus: sub.Status,
			ExpectedIPLimit: sub.IPLimit, ExpectedExpiryTimeMS: sub.ExpiryTimeMS, Quote: terms.Quote, Plan: plan, Provision: data}
		termsJSON, marshalErr := json.Marshal(snapshot)
		if marshalErr != nil {
			return nil, marshalErr
		}
		var intentID int64
		err = tx.QueryRow(ctx, `INSERT INTO payment_intents(deployment_id,account_id,actor_id,quote_id,intent_key,input_hash,amount_toman,terms,status)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8,'awaiting_receipt') RETURNING id`,
			a.DeploymentID, a.AccountID, a.ID, quoteID, operationKey, hash, amount, termsJSON).Scan(&intentID)
		if err != nil {
			return nil, err
		}
		if _, err = tx.Exec(ctx, `UPDATE orders SET payment_intent_id=$1 WHERE id=$2`, intentID, result.OrderID); err != nil {
			return nil, err
		}
		result.PaymentIntentID = intentID
		result.Status = "awaiting_payment"
		if err = tx.Commit(ctx); err != nil {
			return nil, err
		}
		return result, nil
	}
	if err = chargeWallet(ctx, tx, a, amount, "subscription "+terms.Action+": "+sub.Email, "wallet:"+operationKey, hash, "subscription", subscriptionID); err != nil {
		return nil, err
	}
	payload, err := json.Marshal(data)
	if err != nil {
		return nil, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO work_items(deployment_id,account_id,actor_id,panel_id,subscription_id,operation_key,input_hash,kind,desired_state)
		VALUES($1,$2,$3,$4,$5,$6,$7,'subscription_update',$8)`,
		a.DeploymentID, a.AccountID, a.ID, sub.PanelID, subscriptionID, operationKey, hash, payload); err != nil {
		return nil, err
	}
	if _, err = tx.Exec(ctx, `UPDATE subscriptions SET status='provisioning',updated_at=now() WHERE id=$1 AND status=$2`, subscriptionID, sub.Status); err != nil {
		return nil, err
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, err
	}
	return result, nil
}

func createSubscriptionMutationWork(ctx context.Context, tx pgx.Tx, a *Actor, panelID string, subscriptionID int64, opKey, inputHash string, data provisionData, paymentIntentID *int64) error {
	payload, err := json.Marshal(data)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO work_items(deployment_id,account_id,actor_id,panel_id,subscription_id,payment_intent_id,operation_key,input_hash,kind,desired_state)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,'subscription_update',$9)`, a.DeploymentID, a.AccountID, a.ID, panelID, subscriptionID, paymentIntentID, opKey, inputHash, payload)
	return err
}
