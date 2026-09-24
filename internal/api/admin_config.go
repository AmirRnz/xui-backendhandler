package api

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"example.com/xui-commerce/backend/internal/secrets"
	"example.com/xui-commerce/backend/internal/store"
)

func (h *Handler) adminActor(w http.ResponseWriter, r *http.Request) (*store.Actor, bool) {
	deployment := principalFrom(r).DeploymentID
	if deployment != "retail-finland" && deployment != "reseller-turk1" {
		writeError(w, http.StatusForbidden, "forbidden", "administrator role is required")
		return nil, false
	}
	raw := strings.TrimSpace(r.Header.Get("X-Actor-Telegram-ID"))
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "invalid_actor", "X-Actor-Telegram-ID must be a positive integer")
		return nil, false
	}
	a, err := h.Store.Actor(r.Context(), deployment, id)
	if errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrForbidden) {
		writeError(w, http.StatusForbidden, "forbidden", "administrator role is required")
		return nil, false
	}
	if err != nil {
		h.fail(w, err)
		return nil, false
	}
	if a == nil {
		writeError(w, http.StatusForbidden, "forbidden", "administrator role is required")
		return nil, false
	}
	if a.TelegramID != 96937669 || a.Role != "admin" || !a.Enabled || a.ApprovalStatus != "approved" {
		writeError(w, http.StatusForbidden, "forbidden", "administrator role is required")
		return nil, false
	}
	return a, true
}

func (h *Handler) adminConfig(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.adminActor(w, r); !ok {
		return
	}
	c, err := h.Store.AdminConfiguration(r.Context(), principalFrom(r).DeploymentID)
	if err != nil {
		h.fail(w, err)
		return
	}
	panelID, _ := c.Panel["id"].(string)
	if h.Config.PanelTokens[panelID] != "" {
		c.Panel["token_configured"] = true
	}
	writeJSON(w, 200, c)
}

func (h *Handler) adminPendingResellers(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.adminActor(w, r); !ok {
		return
	}
	v, err := h.Store.PendingResellers(r.Context(), principalFrom(r).DeploymentID)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, 200, v)
}

func (h *Handler) adminApproveReseller(w http.ResponseWriter, r *http.Request) {
	h.reviewReseller(w, r, "approved")
}
func (h *Handler) adminRejectReseller(w http.ResponseWriter, r *http.Request) {
	h.reviewReseller(w, r, "rejected")
}
func (h *Handler) reviewReseller(w http.ResponseWriter, r *http.Request, status string) {
	a, ok := h.adminActor(w, r)
	if !ok {
		return
	}
	tgID, err := strconv.ParseInt(r.PathValue("telegram_id"), 10, 64)
	if err != nil || tgID <= 0 {
		writeError(w, 400, "invalid_request", "reseller Telegram ID must be positive")
		return
	}
	var empty struct{}
	if !decode(w, r, &empty) {
		return
	}
	v, err := h.Store.SetResellerApproval(r.Context(), principalFrom(r).DeploymentID, a.ID, tgID, status)
	if err != nil {
		h.adminConfigFail(w, err)
		return
	}
	writeJSON(w, 200, v)
}

func (h *Handler) adminCreatePlan(w http.ResponseWriter, r *http.Request) {
	a, ok := h.adminActor(w, r)
	if !ok {
		return
	}
	var p store.AdminPlan
	if !decode(w, r, &p) {
		return
	}
	id, err := h.Store.SaveAdminPlan(r.Context(), principalFrom(r).DeploymentID, a.ID, p)
	if err != nil {
		h.adminConfigFail(w, err)
		return
	}
	c, err := h.Store.AdminConfiguration(r.Context(), principalFrom(r).DeploymentID)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"id": id, "config": c})
}

func (h *Handler) adminUpdatePlan(w http.ResponseWriter, r *http.Request) {
	a, ok := h.adminActor(w, r)
	if !ok {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, 400, "invalid_request", "plan ID must be positive")
		return
	}
	var p store.AdminPlan
	if !decode(w, r, &p) {
		return
	}
	p.ID = id
	if _, err = h.Store.SaveAdminPlan(r.Context(), principalFrom(r).DeploymentID, a.ID, p); err != nil {
		h.adminConfigFail(w, err)
		return
	}
	h.adminConfig(w, r)
}

func (h *Handler) adminPaymentInstructions(w http.ResponseWriter, r *http.Request) {
	a, ok := h.adminActor(w, r)
	if !ok {
		return
	}
	var q struct {
		CardNumber   string `json:"card_number"`
		CardOwner    string `json:"card_owner"`
		Instructions string `json:"instructions"`
	}
	if !decode(w, r, &q) {
		return
	}
	if err := h.Store.SavePaymentInstructions(r.Context(), principalFrom(r).DeploymentID, a.ID, q.CardNumber, q.CardOwner, q.Instructions); err != nil {
		h.adminConfigFail(w, err)
		return
	}
	h.adminConfig(w, r)
}

func (h *Handler) adminSettings(w http.ResponseWriter, r *http.Request) {
	a, ok := h.adminActor(w, r)
	if !ok {
		return
	}
	var q struct {
		RetailTrialResetDays      *int           `json:"retail_trial_reset_days"`
		UnapprovedTrialDailyLimit *int           `json:"unapproved_trial_daily_limit"`
		ResellerApprovedRequired  *bool          `json:"reseller_approved_required"`
		Features                  map[string]any `json:"features"`
		Text                      map[string]any `json:"text"`
	}
	if !decode(w, r, &q) {
		return
	}
	if q.RetailTrialResetDays == nil && q.UnapprovedTrialDailyLimit == nil && q.ResellerApprovedRequired == nil && q.Features == nil && q.Text == nil {
		writeError(w, 400, "invalid_request", "at least one setting is required")
		return
	}
	if err := h.Store.PatchAdminSettings(r.Context(), principalFrom(r).DeploymentID, a.ID, q.RetailTrialResetDays, q.UnapprovedTrialDailyLimit, q.ResellerApprovedRequired, q.Features, q.Text); err != nil {
		h.adminConfigFail(w, err)
		return
	}
	h.adminConfig(w, r)
}

func (h *Handler) adminPanel(w http.ResponseWriter, r *http.Request) {
	a, ok := h.adminActor(w, r)
	if !ok {
		return
	}
	var q struct {
		BaseURL string `json:"base_url"`
		Token   string `json:"token"`
	}
	if !decode(w, r, &q) {
		return
	}
	if len(h.Config.PanelSecretsKey) != 32 {
		writeError(w, http.StatusServiceUnavailable, "secret_store_unavailable", "panel secret encryption is not configured")
		return
	}
	if strings.TrimSpace(q.Token) == "" || len(q.Token) > 4096 {
		writeError(w, 400, "invalid_request", "token is required and must be at most 4096 characters")
		return
	}
	ciphertext, err := secrets.Seal(h.Config.PanelSecretsKey, []byte(q.Token))
	if err != nil {
		h.fail(w, err)
		return
	}
	if err = h.Store.SavePanelConfig(r.Context(), principalFrom(r).DeploymentID, a.ID, q.BaseURL, ciphertext); err != nil {
		h.adminConfigFail(w, err)
		return
	}
	h.adminConfig(w, r)
}

func (h *Handler) adminConfigFail(w http.ResponseWriter, err error) {
	if errors.Is(err, store.ErrInvalidAdminConfig) {
		writeError(w, 400, "invalid_request", err.Error())
		return
	}
	h.fail(w, err)
}
