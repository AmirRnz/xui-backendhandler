package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"example.com/xui-commerce/backend/internal/api"
	"example.com/xui-commerce/backend/internal/config"
)

func TestHTTPClientAndActorIdentityStayWithinDeployment(t *testing.T) {
	s, _ := testStore(t)
	ctx := context.Background()
	finland := resolve(t, s, "retail-finland", 97101)
	germany := resolve(t, s, "retail-germany", 97101)
	if _, err := s.DB.Exec(ctx, `UPDATE actors SET role='admin',approval_status='approved' WHERE id=$1`, germany.ID); err != nil {
		t.Fatal(err)
	}
	handler := api.New(s, config.Config{ClientCredentials: []config.ClientCredential{{DeploymentID: "retail-finland", Token: "retail-finland-test-token-000000000000"}}}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	request := func(method, path, token, actor string, body []byte) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, bytes.NewReader(body))
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		if actor != "" {
			req.Header.Set("X-Actor-Telegram-ID", actor)
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}
	resolved := request(http.MethodPost, "/v1/actors/resolve", "retail-finland-test-token-000000000000", "", []byte(`{"telegram_id":97101}`))
	if resolved.Code != http.StatusOK {
		t.Fatalf("scoped resolve status=%d body=%s", resolved.Code, resolved.Body.String())
	}
	var view struct {
		TelegramID int64  `json:"telegram_id"`
		Channel    string `json:"channel"`
		Role       string `json:"role"`
	}
	if err := json.Unmarshal(resolved.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if view.TelegramID != 97101 || view.Channel != "retail" || view.Role != "customer" {
		t.Fatalf("unexpected deployment-scoped actor: %+v", view)
	}
	var account int64
	if err := s.DB.QueryRow(ctx, `SELECT account_id FROM actors WHERE deployment_id='retail-finland' AND telegram_id=97101`).Scan(&account); err != nil {
		t.Fatal(err)
	}
	if account != finland.AccountID || account == germany.AccountID {
		t.Fatal("HTTP identity resolution crossed deployment accounts")
	}
	adminAttempt := request(http.MethodPost, "/v1/payment-intents/1/approve", "retail-finland-test-token-000000000000", "97101", []byte(`{}`))
	if adminAttempt.Code != http.StatusForbidden {
		t.Fatalf("admin role from another deployment leaked through shared Telegram ID: %d %s", adminAttempt.Code, adminAttempt.Body.String())
	}
	if noToken := request(http.MethodGet, "/v1/me", "", "97101", nil); noToken.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated bot received status %d", noToken.Code)
	}
	if noActor := request(http.MethodGet, "/v1/wallet", "retail-finland-test-token-000000000000", "", nil); noActor.Code != http.StatusBadRequest {
		t.Fatalf("missing actor header received status %d", noActor.Code)
	}
}
