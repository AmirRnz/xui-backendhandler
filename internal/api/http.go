package api

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"example.com/xui-commerce/backend/internal/config"
	"example.com/xui-commerce/backend/internal/store"
	"github.com/jackc/pgx/v5"
)

type principal struct{ DeploymentID string }
type contextKey struct{}
type Handler struct {
	Store  *store.Store
	Logger *slog.Logger
	tokens []config.ClientCredential
	Config config.Config
	mux    *http.ServeMux
}
type actorView struct {
	TelegramID     int64  `json:"telegram_id"`
	Role           string `json:"role"`
	ApprovalStatus string `json:"approval_status"`
	Channel        string `json:"channel"`
}

func New(s *store.Store, c config.Config, logger *slog.Logger) http.Handler {
	h := &Handler{Store: s, Logger: logger, mux: http.NewServeMux(), tokens: c.ClientCredentials, Config: c}
	h.routes()
	return h.authenticate(h.mux)
}

func (h *Handler) routes() {
	h.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, map[string]any{"status": "ok"}) })
	h.mux.HandleFunc("POST /v1/actors/resolve", h.resolveActor)
	h.mux.HandleFunc("GET /v1/me", h.me)
	h.mux.HandleFunc("POST /v1/reseller/access-requests", h.requestResellerAccess)
	h.mux.HandleFunc("GET /v1/features", h.features)
	h.mux.HandleFunc("GET /v1/plans", h.plans)
	h.mux.HandleFunc("POST /v1/quotes", h.quote)
	h.mux.HandleFunc("POST /v1/purchases", h.purchase)
	h.mux.HandleFunc("POST /v1/trials", h.trial)
	h.mux.HandleFunc("GET /v1/subscriptions", h.subscriptions)
	h.mux.HandleFunc("POST /v1/subscriptions/{id}/cancel", h.cancelSubscription)
	h.mux.HandleFunc("POST /v1/subscriptions/{id}/refunds", h.requestRefund)
	h.mux.HandleFunc("GET /v1/wallet", h.wallet)
	h.mux.HandleFunc("GET /v1/wallet/ledger", h.walletLedger)
	h.mux.HandleFunc("POST /v1/wallet/topups", h.createTopup)
	h.mux.HandleFunc("GET /v1/wallet/topups/active", h.activeTopup)
	h.mux.HandleFunc("POST /v1/wallet/topups/{id}/receipt", h.topupReceipt)
	h.mux.HandleFunc("POST /v1/wallet/topups/{id}/approve", h.approveTopup)
	h.mux.HandleFunc("GET /v1/admin/topups", h.pendingTopups)
	h.mux.HandleFunc("POST /v1/admin/topups/{id}/reject", h.rejectTopup)
	h.mux.HandleFunc("GET /v1/payment-intents/active", h.activePaymentIntent)
	h.mux.HandleFunc("POST /v1/payment-intents/{id}/receipt", h.paymentReceipt)
	h.mux.HandleFunc("POST /v1/payment-intents/{id}/approve", h.approvePayment)
	h.mux.HandleFunc("POST /v1/payment-intents/{id}/reject", h.rejectPayment)
	h.mux.HandleFunc("GET /v1/admin/payments", h.pendingPayments)
	h.mux.HandleFunc("GET /v1/admin/work-items", h.workItems)
	h.mux.HandleFunc("GET /v1/admin/config", h.adminConfig)
	h.mux.HandleFunc("GET /v1/admin/panels/inbounds", h.adminInbounds)
	h.mux.HandleFunc("GET /v1/admin/resellers/pending", h.adminPendingResellers)
	h.mux.HandleFunc("POST /v1/admin/resellers/{telegram_id}/approve", h.adminApproveReseller)
	h.mux.HandleFunc("POST /v1/admin/resellers/{telegram_id}/reject", h.adminRejectReseller)
	h.mux.HandleFunc("POST /v1/admin/config/plans", h.adminCreatePlan)
	h.mux.HandleFunc("PUT /v1/admin/config/plans/{id}", h.adminUpdatePlan)
	h.mux.HandleFunc("PATCH /v1/admin/config/payment-instructions", h.adminPaymentInstructions)
	h.mux.HandleFunc("PATCH /v1/admin/config/settings", h.adminSettings)
	h.mux.HandleFunc("PUT /v1/admin/config/panel", h.adminPanel)
	h.mux.HandleFunc("GET /v1/admin/refunds", h.pendingRefunds)
	h.mux.HandleFunc("POST /v1/admin/refunds/{id}/approve", h.approveRefund)
	h.mux.HandleFunc("POST /v1/admin/refunds/{id}/reject", h.rejectRefund)
	h.mux.HandleFunc("GET /v1/payment-instructions", h.paymentInstructions)
}

func (h *Handler) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			next.ServeHTTP(w, r)
			return
		}
		const prefix = "Bearer "
		header := r.Header.Get("Authorization")
		if !strings.HasPrefix(header, prefix) {
			writeError(w, http.StatusUnauthorized, "unauthorized", "client authentication required")
			return
		}
		provided := sha256.Sum256([]byte(strings.TrimSpace(strings.TrimPrefix(header, prefix))))
		deployment := ""
		for _, candidate := range h.tokens {
			expected := sha256.Sum256([]byte(candidate.Token))
			if subtle.ConstantTimeCompare(provided[:], expected[:]) == 1 {
				deployment = candidate.DeploymentID
				break
			}
		}
		if deployment == "" {
			ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
			storedDeployment, err := h.Store.AuthenticateClient(ctx, strings.TrimSpace(strings.TrimPrefix(header, prefix)))
			cancel()
			if err == nil {
				deployment = storedDeployment
			} else if !errors.Is(err, pgx.ErrNoRows) {
				if h.Logger != nil {
					h.Logger.Error("database client authentication failed", "error", err)
				}
				writeError(w, http.StatusInternalServerError, "internal_error", "request could not be authenticated")
				return
			}
		}
		if deployment == "" {
			writeError(w, http.StatusUnauthorized, "unauthorized", "invalid client credentials")
			return
		}
		checkCtx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		active, err := h.Store.IsDeploymentEnabled(checkCtx, deployment)
		cancel()
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				writeError(w, http.StatusUnauthorized, "unauthorized", "invalid client credentials")
				return
			}
			if h.Logger != nil {
				h.Logger.Error("database deployment status check failed", "error", err)
			}
			writeError(w, http.StatusInternalServerError, "internal_error", "request could not be authenticated")
			return
		}
		if !active {
			writeError(w, http.StatusUnauthorized, "unauthorized", "client identity is disabled")
			return
		}
		// Hold a deployment-scoped shared lock for the full request. Transfer
		// freeze takes the exclusive counterpart before its DB snapshot, so a
		// request racing the freeze must either finish first or observe frozen.
		leaseCtx, leaseCancel := context.WithTimeout(r.Context(), 5*time.Second)
		lease, err := h.Store.AcquireDeploymentRequestLease(leaseCtx, deployment)
		leaseCancel()
		if err != nil {
			writeError(w, http.StatusServiceUnavailable, "temporarily_unavailable", "deployment is being transferred")
			return
		}
		defer func() {
			releaseCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			if releaseErr := lease.Release(releaseCtx); releaseErr != nil && h.Logger != nil {
				h.Logger.Error("deployment request lease release failed", "error", releaseErr)
			}
		}()
		checkCtx, cancel = context.WithTimeout(r.Context(), 3*time.Second)
		active, err = lease.IsEnabled(checkCtx, true, deployment)
		cancel()
		if err != nil || !active {
			writeError(w, http.StatusUnauthorized, "unauthorized", "client identity is disabled")
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), contextKey{}, principal{deployment})))
	})
}

func (h *Handler) resolveActor(w http.ResponseWriter, r *http.Request) {
	var req struct {
		TelegramID int64 `json:"telegram_id"`
	}
	if !decode(w, r, &req) {
		return
	}
	p := principalFrom(r)
	a, err := h.Store.ResolveActor(r.Context(), p.DeploymentID, req.TelegramID)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, actorView{TelegramID: a.TelegramID, Role: a.Role, ApprovalStatus: a.ApprovalStatus, Channel: a.Channel})
}
func (h *Handler) me(w http.ResponseWriter, r *http.Request) {
	a, ok := h.actor(w, r)
	if !ok {
		return
	}
	writeJSON(w, 200, actorView{TelegramID: a.TelegramID, Role: a.Role, ApprovalStatus: a.ApprovalStatus, Channel: a.Channel})
}
func (h *Handler) requestResellerAccess(w http.ResponseWriter, r *http.Request) {
	a, ok := h.actor(w, r)
	if !ok {
		return
	}
	var empty struct{}
	if !decode(w, r, &empty) {
		return
	}
	result, err := h.Store.RequestResellerAccess(r.Context(), a)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, result)
}
func (h *Handler) features(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.actor(w, r); !ok {
		return
	}
	v, err := h.Store.PublicFeatures(r.Context(), principalFrom(r).DeploymentID)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, 200, v)
}
func (h *Handler) plans(w http.ResponseWriter, r *http.Request) {
	a, ok := h.actor(w, r)
	if !ok {
		return
	}
	kind := r.URL.Query().Get("kind")
	if kind == "" {
		kind = "paid"
	}
	plans, err := h.Store.ListPlans(r.Context(), a, kind)
	if err != nil {
		h.fail(w, err)
		return
	}
	public := make([]map[string]any, 0, len(plans))
	for _, p := range plans {
		item := map[string]any{"id": p.ID, "name": p.Name, "description": p.Description, "kind": p.Kind, "is_limited": p.IsLimited,
			"base_price_toman": p.BasePriceToman, "price_per_extra_ip_toman": p.PricePerExtraIPToman,
			"price_per_gb_toman": p.PricePerGBToman, "price_per_extra_month_toman": p.PricePerExtraMonthToman,
			"base_ip_limit": p.BaseIPLimit, "max_ip_limit": p.MaxIPLimit, "min_data_gb": p.MinDataGB,
			"max_data_bytes": p.MaxDataBytes, "expire_seconds": p.ExpireSeconds, "usage_description": p.UsageDescription,
			"discount_tiers": p.DiscountTiers, "test_ip_limit": p.IPLimit, "max_per_day": p.MaxPerDay}
		public = append(public, item)
	}
	writeJSON(w, 200, public)
}
func (h *Handler) quote(w http.ResponseWriter, r *http.Request) {
	a, ok := h.actor(w, r)
	if !ok {
		return
	}
	if !h.requireFeature(w, r, "purchases_enabled") {
		return
	}
	var q struct {
		PlanID         int64  `json:"plan_id"`
		Months         int    `json:"months"`
		IPLimit        int    `json:"ip_limit"`
		DataGB         int    `json:"data_gb"`
		IdempotencyKey string `json:"idempotency_key"`
	}
	if !decode(w, r, &q) {
		return
	}
	result, err := h.Store.CreateQuote(r.Context(), a, q.PlanID, q.Months, q.IPLimit, q.DataGB, q.IdempotencyKey)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, 201, result)
}
func (h *Handler) purchase(w http.ResponseWriter, r *http.Request) {
	a, ok := h.actor(w, r)
	if !ok {
		return
	}
	var q struct {
		QuoteID        int64  `json:"quote_id"`
		PaymentMethod  string `json:"payment_method"`
		IdempotencyKey string `json:"idempotency_key"`
		DisplayName    string `json:"display_name"`
	}
	if !decode(w, r, &q) {
		return
	}
	if !h.requireFeature(w, r, "purchases_enabled") {
		return
	}
	if q.PaymentMethod == "wallet" && !h.requireFeature(w, r, "wallet_enabled") {
		return
	}
	if q.PaymentMethod == "direct" && !h.requireFeature(w, r, "direct_payments_enabled") {
		return
	}
	result, err := h.Store.CreatePurchase(r.Context(), a, q.QuoteID, q.PaymentMethod, q.IdempotencyKey, q.DisplayName)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, 201, result)
}
func (h *Handler) trial(w http.ResponseWriter, r *http.Request) {
	a, ok := h.actor(w, r)
	if !ok {
		return
	}
	if !h.requireFeature(w, r, "trials_enabled") {
		return
	}
	var q struct {
		PlanID         int64  `json:"plan_id"`
		IdempotencyKey string `json:"idempotency_key"`
	}
	if !decode(w, r, &q) {
		return
	}
	result, err := h.Store.ClaimTrial(r.Context(), a, q.PlanID, q.IdempotencyKey)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, 202, result)
}
func (h *Handler) subscriptions(w http.ResponseWriter, r *http.Request) {
	a, ok := h.actor(w, r)
	if !ok {
		return
	}
	result, err := h.Store.Subscriptions(r.Context(), a)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, 200, result)
}
func (h *Handler) cancelSubscription(w http.ResponseWriter, r *http.Request) {
	a, ok := h.actor(w, r)
	if !ok {
		return
	}
	id, err := pathID(r)
	if err != nil {
		h.fail(w, err)
		return
	}
	var q struct {
		IdempotencyKey string `json:"idempotency_key"`
	}
	if !decode(w, r, &q) {
		return
	}
	workID, err := h.Store.CancelSubscription(r.Context(), a, id, q.IdempotencyKey)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, 202, map[string]any{"work_item_id": workID, "status": "pending"})
}

func (h *Handler) requestRefund(w http.ResponseWriter, r *http.Request) {
	a, ok := h.actor(w, r)
	if !ok {
		return
	}
	id, err := pathID(r)
	if err != nil {
		h.fail(w, err)
		return
	}
	var q struct {
		IdempotencyKey string `json:"idempotency_key"`
		Reason         string `json:"reason"`
	}
	if !decode(w, r, &q) {
		return
	}
	result, err := h.Store.RequestRefund(r.Context(), a, id, q.IdempotencyKey, q.Reason)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, 201, result)
}

func (h *Handler) pendingRefunds(w http.ResponseWriter, r *http.Request) {
	a, ok := h.adminActor(w, r)
	if !ok {
		return
	}
	v, err := h.Store.PendingRefunds(r.Context(), a)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, 200, v)
}

func (h *Handler) rejectRefund(w http.ResponseWriter, r *http.Request) {
	a, ok := h.adminActor(w, r)
	if !ok {
		return
	}
	id, err := pathID(r)
	if err != nil {
		h.fail(w, err)
		return
	}
	var q struct{}
	if !decode(w, r, &q) {
		return
	}
	already, err := h.Store.RejectRefund(r.Context(), a, id)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"refund_request_id": id, "status": "rejected", "already_rejected": already})
}

func (h *Handler) approveRefund(w http.ResponseWriter, r *http.Request) {
	a, ok := h.adminActor(w, r)
	if !ok {
		return
	}
	id, err := pathID(r)
	if err != nil {
		h.fail(w, err)
		return
	}
	var q struct {
		AmountToman    int64  `json:"amount_toman"`
		AuditNote      string `json:"audit_note"`
		IdempotencyKey string `json:"idempotency_key"`
		ManualOverride bool   `json:"manual_override"`
	}
	if !decode(w, r, &q) {
		return
	}
	balance, err := h.Store.ApproveRefund(r.Context(), a, id, q.AmountToman, q.AuditNote, q.IdempotencyKey, q.ManualOverride)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"refund_request_id": id, "status": "approved", "wallet_balance_toman": balance})
}
func (h *Handler) wallet(w http.ResponseWriter, r *http.Request) {
	a, ok := h.actor(w, r)
	if !ok {
		return
	}
	balance, err := h.Store.Balance(r.Context(), a)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"balance_toman": balance, "currency": "تومان"})
}
func (h *Handler) walletLedger(w http.ResponseWriter, r *http.Request) {
	a, ok := h.actor(w, r)
	if !ok {
		return
	}
	v, err := h.Store.WalletLedger(r.Context(), a)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, 200, v)
}
func (h *Handler) createTopup(w http.ResponseWriter, r *http.Request) {
	a, ok := h.actor(w, r)
	if !ok {
		return
	}
	if !h.requireFeature(w, r, "topups_enabled") {
		return
	}
	var q struct {
		AmountToman    int64  `json:"amount_toman"`
		IdempotencyKey string `json:"idempotency_key"`
	}
	if !decode(w, r, &q) {
		return
	}
	id, err := h.Store.CreateTopup(r.Context(), a, q.AmountToman, q.IdempotencyKey)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, 201, map[string]any{"topup_id": id, "status": "awaiting_receipt", "amount_toman": q.AmountToman})
}
func (h *Handler) topupReceipt(w http.ResponseWriter, r *http.Request) {
	a, ok := h.actor(w, r)
	if !ok {
		return
	}
	id, err := pathID(r)
	if err != nil {
		h.fail(w, err)
		return
	}
	var q struct {
		TelegramFileID string `json:"telegram_file_id"`
	}
	if !decode(w, r, &q) {
		return
	}
	if err = h.Store.SubmitTopupReceipt(r.Context(), a, id, q.TelegramFileID); err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"topup_id": id, "status": "receipt_submitted"})
}
func (h *Handler) activeTopup(w http.ResponseWriter, r *http.Request) {
	a, ok := h.actor(w, r)
	if !ok {
		return
	}
	beforeID, valid := activeRequestCursor(w, r)
	if !valid {
		return
	}
	items, next, err := h.Store.ActiveTopups(r.Context(), a, beforeID)
	if err != nil {
		h.fail(w, err)
		return
	}
	var newest map[string]any
	if len(items) > 0 {
		newest = items[0]
	}
	writeJSON(w, http.StatusOK, map[string]any{"topups": items, "next_cursor": next, "topup": newest})
}
func (h *Handler) pendingTopups(w http.ResponseWriter, r *http.Request) {
	a, ok := h.adminActor(w, r)
	if !ok {
		return
	}
	v, err := h.Store.PendingTopups(r.Context(), a)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, 200, v)
}
func (h *Handler) rejectTopup(w http.ResponseWriter, r *http.Request) {
	a, ok := h.adminActor(w, r)
	if !ok {
		return
	}
	id, err := pathID(r)
	if err != nil {
		h.fail(w, err)
		return
	}
	var q struct{}
	if !decode(w, r, &q) {
		return
	}
	already, err := h.Store.RejectTopup(r.Context(), a, id)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"topup_id": id, "status": "rejected", "already_rejected": already})
}
func (h *Handler) approveTopup(w http.ResponseWriter, r *http.Request) {
	a, ok := h.adminActor(w, r)
	if !ok {
		return
	}
	id, err := pathID(r)
	if err != nil {
		h.fail(w, err)
		return
	}
	balance, err := h.Store.ApproveTopup(r.Context(), a, id)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"topup_id": id, "status": "approved", "wallet_balance_toman": balance})
}
func (h *Handler) paymentReceipt(w http.ResponseWriter, r *http.Request) {
	a, ok := h.actor(w, r)
	if !ok {
		return
	}
	id, err := pathID(r)
	if err != nil {
		h.fail(w, err)
		return
	}
	var q struct {
		TelegramFileID string `json:"telegram_file_id"`
	}
	if !decode(w, r, &q) {
		return
	}
	if err = h.Store.SubmitReceipt(r.Context(), a, id, q.TelegramFileID); err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"payment_intent_id": id, "status": "receipt_submitted"})
}
func (h *Handler) activePaymentIntent(w http.ResponseWriter, r *http.Request) {
	a, ok := h.actor(w, r)
	if !ok {
		return
	}
	beforeID, valid := activeRequestCursor(w, r)
	if !valid {
		return
	}
	items, next, err := h.Store.ActivePaymentIntents(r.Context(), a, beforeID)
	if err != nil {
		h.fail(w, err)
		return
	}
	var newest map[string]any
	if len(items) > 0 {
		newest = items[0]
	}
	writeJSON(w, http.StatusOK, map[string]any{"payment_intents": items, "next_cursor": next, "payment_intent": newest})
}

func activeRequestCursor(w http.ResponseWriter, r *http.Request) (int64, bool) {
	raw := strings.TrimSpace(r.URL.Query().Get("before_id"))
	if raw == "" {
		return 0, true
	}
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "invalid_request", "before_id must be a positive integer")
		return 0, false
	}
	return id, true
}
func (h *Handler) pendingPayments(w http.ResponseWriter, r *http.Request) {
	a, ok := h.adminActor(w, r)
	if !ok {
		return
	}
	v, err := h.Store.PendingPayments(r.Context(), a)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, 200, v)
}
func (h *Handler) rejectPayment(w http.ResponseWriter, r *http.Request) {
	a, ok := h.adminActor(w, r)
	if !ok {
		return
	}
	id, err := pathID(r)
	if err != nil {
		h.fail(w, err)
		return
	}
	var q struct{}
	if !decode(w, r, &q) {
		return
	}
	already, err := h.Store.RejectPayment(r.Context(), a, id)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"payment_intent_id": id, "status": "rejected", "already_rejected": already})
}
func (h *Handler) approvePayment(w http.ResponseWriter, r *http.Request) {
	a, ok := h.adminActor(w, r)
	if !ok {
		return
	}
	id, err := pathID(r)
	if err != nil {
		h.fail(w, err)
		return
	}
	v, err := h.Store.ApprovePayment(r.Context(), a, id)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, 200, v)
}
func (h *Handler) workItems(w http.ResponseWriter, r *http.Request) {
	a, ok := h.adminActor(w, r)
	if !ok {
		return
	}
	v, err := h.Store.RecentWork(r.Context(), a)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, 200, v)
}
func (h *Handler) paymentInstructions(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.actor(w, r); !ok {
		return
	}
	p := principalFrom(r)
	var card, owner, details string
	var minimum int64
	err := h.Store.DB.QueryRow(r.Context(), `SELECT payment_card_number,payment_card_owner,payment_instructions,COALESCE((configuration->>'min_topup_toman')::bigint,0) FROM deployments WHERE id=$1`, p.DeploymentID).Scan(&card, &owner, &details, &minimum)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"card_number": card, "card_owner": owner, "instructions": details, "min_topup_toman": minimum})
}

func (h *Handler) actor(w http.ResponseWriter, r *http.Request) (*store.Actor, bool) {
	raw := strings.TrimSpace(r.Header.Get("X-Actor-Telegram-ID"))
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		writeError(w, 400, "invalid_actor", "X-Actor-Telegram-ID must be a positive integer")
		return nil, false
	}
	a, err := h.Store.Actor(r.Context(), principalFrom(r).DeploymentID, id)
	if err != nil {
		h.fail(w, err)
		return nil, false
	}
	return a, true
}
func (h *Handler) requireFeature(w http.ResponseWriter, r *http.Request, key string) bool {
	enabled, err := h.Store.FeatureEnabled(r.Context(), principalFrom(r).DeploymentID, key)
	if err != nil {
		h.fail(w, err)
		return false
	}
	if !enabled {
		writeError(w, http.StatusConflict, "feature_disabled", "this action is currently unavailable")
		return false
	}
	return true
}
func principalFrom(r *http.Request) principal {
	p, _ := r.Context().Value(contextKey{}).(principal)
	return p
}
func decode(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		writeError(w, 400, "invalid_request", "request body is invalid")
		return false
	}
	if dec.Decode(new(any)) != io.EOF {
		writeError(w, 400, "invalid_request", "request body must contain one JSON value")
		return false
	}
	return true
}
func pathID(r *http.Request) (int64, error) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		return 0, fmt.Errorf("invalid resource ID")
	}
	return id, nil
}
func (h *Handler) fail(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound), errors.Is(err, pgx.ErrNoRows):
		writeError(w, 404, "not_found", "resource was not found in this deployment/account")
	case errors.Is(err, store.ErrForbidden):
		writeError(w, 403, "forbidden", "actor is not authorized")
	case errors.Is(err, store.ErrTopupBelowMinimum):
		writeError(w, http.StatusBadRequest, "min_topup_not_met", err.Error())
	case errors.Is(err, store.ErrConflict), errors.Is(err, store.ErrQuotaExceeded):
		writeError(w, 409, "conflict", err.Error())
	case errors.Is(err, store.ErrInsufficientFunds):
		writeError(w, 402, "insufficient_funds", "wallet balance is insufficient")
	default:
		if h.Logger != nil {
			h.Logger.Error("backend request failed", "error", err)
		}
		writeError(w, 500, "internal_error", "request could not be completed")
	}
}
func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"code": code, "message": message}})
}
func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

// RequestContext is exported to make API smoke tests independent of HTTP server lifecycle.
func RequestContext(timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	return context.WithTimeout(context.Background(), timeout)
}
