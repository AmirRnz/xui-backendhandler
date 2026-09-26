package api

import (
	"context"
	"net/http"
	"time"

	"example.com/xui-commerce/backend/internal/secrets"
	"example.com/xui-commerce/backend/internal/xui"
)

func (h *Handler) adminInbounds(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.adminActor(w, r); !ok {
		return
	}
	configuration, err := h.Store.AdminConfiguration(r.Context(), principalFrom(r).DeploymentID)
	if err != nil {
		h.fail(w, err)
		return
	}
	panelID, _ := configuration.Panel["id"].(string)
	baseURL, err := h.Store.PanelEndpoint(r.Context(), panelID)
	if err != nil || panelID == "" {
		writeError(w, http.StatusServiceUnavailable, "panel_not_configured", "a 3x-ui panel must be configured before selecting inbounds")
		return
	}
	token := h.Config.PanelTokens[panelID]
	if token == "" {
		ciphertext, secretErr := h.Store.EncryptedPanelToken(r.Context(), panelID)
		if secretErr != nil || len(ciphertext) == 0 || len(h.Config.PanelSecretsKey) != 32 {
			writeError(w, http.StatusServiceUnavailable, "panel_not_configured", "the 3x-ui panel API token is not configured")
			return
		}
		plain, openErr := secrets.Open(h.Config.PanelSecretsKey, ciphertext)
		if openErr != nil {
			writeError(w, http.StatusServiceUnavailable, "panel_secret_unavailable", "the 3x-ui panel API token could not be read")
			return
		}
		token = string(plain)
	}
	panel, err := xui.New(baseURL, token, 12*time.Second)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "panel_not_configured", "the configured 3x-ui panel connection is invalid")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 12*time.Second)
	defer cancel()
	inbounds, err := panel.ListInbounds(ctx)
	if err != nil {
		writeError(w, http.StatusBadGateway, "panel_unavailable", "could not load inbounds from the configured 3x-ui panel")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"inbounds": inbounds})
}
