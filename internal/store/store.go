package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"example.com/xui-commerce/backend/internal/commerce"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrNotFound           = errors.New("not found")
	ErrForbidden          = errors.New("actor is not authorized for this operation")
	ErrConflict           = errors.New("request conflicts with existing operation")
	ErrInsufficientFunds  = errors.New("insufficient wallet balance")
	ErrQuotaExceeded      = errors.New("trial quota exceeded")
	ErrInvalidAdminConfig = errors.New("invalid admin configuration")
	ErrTopupBelowMinimum  = errors.New("top-up amount is below the configured minimum")
)

type Store struct{ DB *pgxpool.Pool }

type Actor struct {
	ID             int64  `json:"id"`
	DeploymentID   string `json:"deployment_id"`
	AccountID      int64  `json:"account_id"`
	TelegramID     int64  `json:"telegram_id"`
	Role           string `json:"role"`
	ApprovalStatus string `json:"approval_status"`
	Channel        string `json:"channel"`
	Enabled        bool   `json:"enabled"`
}

type PurchaseResult struct {
	OrderID         int64  `json:"order_id"`
	Status          string `json:"status"`
	PaymentIntentID int64  `json:"payment_intent_id,omitempty"`
	SubscriptionID  int64  `json:"subscription_id,omitempty"`
	AmountToman     int64  `json:"amount_toman"`
}

type Subscription struct {
	ID                int64     `json:"id"`
	Email             string    `json:"email"`
	DisplayName       string    `json:"display_name"`
	PlanID            int64     `json:"plan_id"`
	Status            string    `json:"status"`
	Kind              string    `json:"kind"`
	IPLimit           int       `json:"ip_limit"`
	TrafficLimitBytes int64     `json:"traffic_limit_bytes"`
	ExpiryTimeMS      int64     `json:"expiry_time_ms"`
	Links             []string  `json:"links"`
	CreatedAt         time.Time `json:"created_at"`
}

func (s *Store) ResolveActor(ctx context.Context, deployment string, telegramID int64) (*Actor, error) {
	if telegramID <= 0 || deployment == "" {
		return nil, fmt.Errorf("deployment and positive Telegram ID are required")
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, fmt.Sprintf("%s:%d", deployment, telegramID)); err != nil {
		return nil, err
	}
	actor, err := actorByTelegram(ctx, tx, deployment, telegramID)
	if err == nil {
		if err = tx.Commit(ctx); err != nil {
			return nil, err
		}
		return actor, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	var channel string
	var enabled bool
	if err = tx.QueryRow(ctx, `SELECT channel,enabled FROM deployments WHERE id=$1`, deployment).Scan(&channel, &enabled); err != nil {
		return nil, err
	}
	if !enabled {
		return nil, ErrForbidden
	}
	kind, role := "retail_customer", "customer"
	if channel == "reseller" {
		kind, role = "reseller", "reseller"
	} else if channel == "web" {
		kind, role = "organization", "customer"
	}
	var accountID int64
	if err = tx.QueryRow(ctx, `INSERT INTO commercial_accounts(home_deployment_id,kind) VALUES($1,$2) RETURNING id`, deployment, kind).Scan(&accountID); err != nil {
		return nil, err
	}
	var actorID int64
	var actorEnabled bool
	var approval string
	if err = tx.QueryRow(ctx, `INSERT INTO actors(deployment_id,account_id,telegram_id,identity_provider,external_subject,role) VALUES($1,$2,$3::bigint,'telegram',($3::bigint)::text,$4) RETURNING id,enabled,approval_status`, deployment, accountID, telegramID, role).Scan(&actorID, &actorEnabled, &approval); err != nil {
		return nil, err
	}
	actor, err = actorByTelegram(ctx, tx, deployment, telegramID)
	if err != nil {
		return nil, err
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, err
	}
	return actor, nil
}

func (s *Store) Actor(ctx context.Context, deployment string, telegramID int64) (*Actor, error) {
	a, err := actorByTelegram(ctx, s.DB, deployment, telegramID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if !a.Enabled {
		return nil, ErrForbidden
	}
	return a, nil
}

type rowQuery interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func actorByTelegram(ctx context.Context, q rowQuery, deployment string, telegramID int64) (*Actor, error) {
	a := &Actor{}
	err := q.QueryRow(ctx, `SELECT a.id,a.deployment_id,a.account_id,a.telegram_id,a.role,a.approval_status,a.enabled,d.channel
		FROM actors a JOIN deployments d ON d.id=a.deployment_id JOIN commercial_accounts c ON c.id=a.account_id
		WHERE a.deployment_id=$1 AND a.telegram_id=$2 AND c.status='active'`, deployment, telegramID).
		Scan(&a.ID, &a.DeploymentID, &a.AccountID, &a.TelegramID, &a.Role, &a.ApprovalStatus, &a.Enabled, &a.Channel)
	return a, err
}

func (s *Store) ListPlans(ctx context.Context, a *Actor, kind string) ([]commerce.Plan, error) {
	if a == nil || !a.Enabled || a.DeploymentID == "" {
		return nil, ErrForbidden
	}
	if kind != "paid" && kind != "test" {
		return nil, fmt.Errorf("unsupported plan kind")
	}
	rows, err := s.DB.Query(ctx, `SELECT p.id,p.name,p.description,p.kind,p.enabled,p.is_limited,p.base_price_toman,p.price_per_extra_ip_toman,p.price_per_gb_toman,p.price_per_extra_month_toman,
		base_ip_limit,max_ip_limit,min_data_gb,max_data_bytes,expire_seconds,test_ip_limit,max_per_day,flow,
		array_to_json(inbound_ids)::text,discount_tiers::text,usage_description,panel_id
		FROM plans p WHERE p.deployment_id=$1 AND p.kind=$2 AND p.enabled
		AND (p.is_global OR EXISTS(SELECT 1 FROM plan_access pa WHERE pa.deployment_id=p.deployment_id AND pa.plan_id=p.id AND pa.account_id=$3)) ORDER BY p.id`, a.DeploymentID, kind, a.AccountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	plans := make([]commerce.Plan, 0)
	for rows.Next() {
		p, scanErr := scanPlan(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		plans = append(plans, p)
	}
	return plans, rows.Err()
}

type scanner interface{ Scan(...any) error }

func scanPlan(r scanner) (commerce.Plan, error) {
	var p commerce.Plan
	var inboundJSON, discountJSON string
	err := r.Scan(&p.ID, &p.Name, &p.Description, &p.Kind, &p.Enabled, &p.IsLimited, &p.BasePriceToman, &p.PricePerExtraIPToman, &p.PricePerGBToman, &p.PricePerExtraMonthToman,
		&p.BaseIPLimit, &p.MaxIPLimit, &p.MinDataGB, &p.MaxDataBytes, &p.ExpireSeconds, &p.IPLimit, &p.MaxPerDay, &p.Flow, &inboundJSON, &discountJSON, &p.UsageDescription, &p.PanelID)
	if err != nil {
		return p, err
	}
	if err = json.Unmarshal([]byte(inboundJSON), &p.InboundIDs); err != nil {
		return p, err
	}
	if p.InboundIDs == nil {
		p.InboundIDs = []int{}
	}
	if err = json.Unmarshal([]byte(discountJSON), &p.DiscountTiers); err != nil {
		return p, err
	}
	if p.DiscountTiers == nil {
		p.DiscountTiers = []commerce.DiscountTier{}
	}
	return p, nil
}

func planByID(ctx context.Context, tx pgx.Tx, deployment string, accountID, id int64) (commerce.Plan, error) {
	p, err := scanPlan(tx.QueryRow(ctx, `SELECT id,name,description,kind,enabled,is_limited,base_price_toman,price_per_extra_ip_toman,price_per_gb_toman,price_per_extra_month_toman,
		base_ip_limit,max_ip_limit,min_data_gb,max_data_bytes,expire_seconds,test_ip_limit,max_per_day,flow,
		array_to_json(inbound_ids)::text,discount_tiers::text,usage_description,panel_id
		FROM plans p WHERE p.deployment_id=$1 AND p.id=$3
		AND (p.is_global OR EXISTS(SELECT 1 FROM plan_access pa WHERE pa.deployment_id=p.deployment_id AND pa.plan_id=p.id AND pa.account_id=$2))`, deployment, accountID, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return commerce.Plan{}, ErrNotFound
	}
	return p, err
}

func (s *Store) CreateQuote(ctx context.Context, a *Actor, planID int64, months, ipLimit, dataGB int, key string) (*commerce.Quote, error) {
	if a == nil || !a.Enabled || a.DeploymentID == "" {
		return nil, ErrForbidden
	}
	if key == "" || len(key) > 160 {
		return nil, fmt.Errorf("a bounded idempotency key is required")
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	p, err := planByID(ctx, tx, a.DeploymentID, a.AccountID, planID)
	if err != nil {
		return nil, err
	}
	if !p.Enabled || p.Kind != "paid" {
		return nil, ErrNotFound
	}
	q, err := commerce.CalculateQuote(p, months, ipLimit, dataGB)
	if err != nil {
		return nil, err
	}
	q.Key = key
	planJSON, _ := json.Marshal(p)
	termsJSON, _ := json.Marshal(q)
	var id int64
	err = tx.QueryRow(ctx, `INSERT INTO purchase_quotes(deployment_id,account_id,actor_id,quote_key,plan_id,plan_snapshot,terms,months,ip_limit,data_gb,final_price_toman)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11) ON CONFLICT(deployment_id,account_id,quote_key) DO NOTHING RETURNING id`, a.DeploymentID, a.AccountID, a.ID, key, planID, planJSON, termsJSON, months, q.IPLimit, dataGB, q.FinalPriceToman).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		var storedTerms []byte
		var storedPlanID int64
		err = tx.QueryRow(ctx, `SELECT id,terms,plan_id FROM purchase_quotes WHERE deployment_id=$1 AND account_id=$2 AND quote_key=$3`, a.DeploymentID, a.AccountID, key).Scan(&id, &storedTerms, &storedPlanID)
		if err != nil {
			return nil, err
		}
		if storedPlanID != planID || !jsonEqual(storedTerms, termsJSON) {
			return nil, ErrConflict
		}
	} else if err != nil {
		return nil, err
	}
	q.ID = id
	if err = tx.Commit(ctx); err != nil {
		return nil, err
	}
	return &q, nil
}

func jsonEqual(a, b []byte) bool {
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return string(a) == string(b)
	}
	xb, _ := json.Marshal(x)
	yb, _ := json.Marshal(y)
	return string(xb) == string(yb)
}
func hashJSON(v any) string {
	b, _ := json.Marshal(v)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
func validKey(s string) bool { return strings.TrimSpace(s) != "" && len(s) <= 160 }
