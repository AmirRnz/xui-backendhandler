package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"example.com/xui-commerce/backend/internal/api"
	"example.com/xui-commerce/backend/internal/config"
)

func TestAdminConfigIsDeploymentScopedAndSecretsAreWriteOnly(t *testing.T) {
	s, _ := testStore(t)
	ctx := context.Background()
	const finToken = "retail-finland-test-token-000000000000"
	const resToken = "reseller-turk1-test-token-000000000000"
	const gerToken = "retail-germany-test-token-000000000000"
	key := bytes.Repeat([]byte{7}, 32)
	handler := api.New(s, config.Config{PanelSecretsKey: key, ClientCredentials: []config.ClientCredential{{DeploymentID: "retail-finland", Token: finToken}, {DeploymentID: "reseller-turk1", Token: resToken}, {DeploymentID: "retail-germany", Token: gerToken}}}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	request := func(method, path, token, actor, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("X-Actor-Telegram-ID", actor)
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}
	if rec := request(http.MethodGet, "/v1/admin/config", finToken, "96937669", ""); rec.Code != 200 {
		t.Fatalf("Finland admin config status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := request(http.MethodGet, "/v1/admin/config", resToken, "96937669", ""); rec.Code != 200 {
		t.Fatalf("reseller admin config status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := request(http.MethodGet, "/v1/admin/config", gerToken, "96937669", ""); rec.Code != http.StatusForbidden {
		t.Fatalf("Germany should not inherit admin: %d %s", rec.Code, rec.Body.String())
	}
	if rec := request(http.MethodGet, "/v1/admin/config", finToken, "971991", ""); rec.Code != http.StatusForbidden {
		t.Fatalf("ordinary actor accessed admin config: %d %s", rec.Code, rec.Body.String())
	}
	if rec := request(http.MethodGet, "/v1/features", finToken, "971991", ""); rec.Code != 200 {
		t.Fatalf("public feature config status=%d body=%s", rec.Code, rec.Body.String())
	}
	plan := `{"name":"admin draft","kind":"paid","enabled":false,"is_limited":false,"description":"","base_price_toman":1000,"price_per_extra_ip_toman":0,"price_per_gb_toman":0,"price_per_extra_month_toman":0,"base_ip_limit":1,"max_ip_limit":1,"min_data_gb":0,"max_data_bytes":0,"expire_seconds":0,"test_ip_limit":1,"max_per_day":0,"flow":"","inbound_ids":[],"usage_description":""}`
	created := request(http.MethodPost, "/v1/admin/config/plans", finToken, "96937669", plan)
	if created.Code != http.StatusCreated {
		t.Fatalf("plan create status=%d body=%s", created.Code, created.Body.String())
	}
	if rec := request(http.MethodPatch, "/v1/admin/config/payment-instructions", finToken, "96937669", `{"card_number":"1111","card_owner":"Example","instructions":"Pay here"}`); rec.Code != 200 {
		t.Fatalf("payment update status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := request(http.MethodPatch, "/v1/admin/config/settings", finToken, "96937669", `{"retail_trial_reset_days":0,"features":{"trials_enabled":false},"text":{"welcome":"Welcome"}}`); rec.Code != 200 {
		t.Fatalf("settings update status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := request(http.MethodPut, "/v1/admin/config/panel", finToken, "96937669", `{"base_url":"https://panel.example.test","token":"test-panel-token-secret"}`); rec.Code != 200 {
		t.Fatalf("panel update status=%d body=%s", rec.Code, rec.Body.String())
	}
	configView := request(http.MethodGet, "/v1/admin/config", finToken, "96937669", "")
	if strings.Contains(configView.Body.String(), "test-panel-token-secret") {
		t.Fatal("panel token was exposed in admin config response")
	}
	var view map[string]any
	if err := json.Unmarshal(configView.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	panel, ok := view["panel"].(map[string]any)
	if !ok || panel["token_configured"] != true {
		t.Fatalf("panel config status missing: %#v", view["panel"])
	}
	var encrypted []byte
	if err := s.DB.QueryRow(ctx, `SELECT encrypted_api_token FROM panels WHERE id='panel-retail-finland'`).Scan(&encrypted); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encrypted, []byte("test-panel-token-secret")) {
		t.Fatal("panel token was stored in plaintext")
	}
	var audits int
	if err := s.DB.QueryRow(ctx, `SELECT count(*) FROM admin_configuration_audit WHERE deployment_id='retail-finland' AND actor_id=(SELECT id FROM actors WHERE deployment_id='retail-finland' AND telegram_id=96937669)`).Scan(&audits); err != nil {
		t.Fatal(err)
	}
	if audits < 4 {
		t.Fatalf("expected configuration changes to be audited, got %d", audits)
	}
}
