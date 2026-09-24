package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"example.com/xui-commerce/backend/internal/api"
	"example.com/xui-commerce/backend/internal/config"
	"example.com/xui-commerce/backend/internal/store"
)

func TestAdminConfigIsDeploymentScopedAndSecretsAreWriteOnly(t *testing.T) {
	s, _ := testStore(t)
	ctx := context.Background()
	const finToken = "retail-finland-test-token-000000000000"
	const resToken = "reseller-turk1-test-token-000000000000"
	const gerToken = "retail-germany-test-token-000000000000"
	germanyAdmin := resolve(t, s, "retail-germany", 96937669)
	if _, err := s.DB.Exec(ctx, `UPDATE actors SET role='admin',approval_status='approved' WHERE id=$1`, germanyAdmin.ID); err != nil {
		t.Fatal(err)
	}
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
	var germanyRole string
	if err := s.DB.QueryRow(ctx, `SELECT role FROM actors WHERE id=$1`, germanyAdmin.ID).Scan(&germanyRole); err != nil {
		t.Fatal(err)
	}
	if germanyRole != "admin" {
		t.Fatalf("admin config request unexpectedly modified Germany actor: %q", germanyRole)
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
	var createdPlan struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(created.Body.Bytes(), &createdPlan); err != nil {
		t.Fatal(err)
	}
	var planAudits int
	if err := s.DB.QueryRow(ctx, `SELECT count(*) FROM admin_configuration_audit WHERE deployment_id='retail-finland' AND action='plan_created' AND subject_ref=$1`, fmt.Sprintf("plan:%d", createdPlan.ID)).Scan(&planAudits); err != nil {
		t.Fatal(err)
	}
	if planAudits != 1 {
		t.Fatalf("created plan audit subject missing: %d", planAudits)
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

func TestResellerReviewIsDeploymentScopedAndAuditedAtomically(t *testing.T) {
	s, _ := testStore(t)
	ctx := context.Background()
	admin := resolve(t, s, "reseller-turk1", 96937669)
	applicant := resolve(t, s, "reseller-turk1", 97654321)
	if applicant.Role != "reseller" || applicant.ApprovalStatus != "pending" {
		t.Fatalf("unexpected reseller applicant: %+v", applicant)
	}
	const token = "reseller-turk1-test-token-000000000000"
	handler := api.New(s, config.Config{ClientCredentials: []config.ClientCredential{{DeploymentID: "reseller-turk1", Token: token}}}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	request := func(method, path, actor, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("X-Actor-Telegram-ID", actor)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}
	pending := request(http.MethodGet, "/v1/admin/resellers/pending", "96937669", "")
	if pending.Code != 200 || !strings.Contains(pending.Body.String(), "97654321") {
		t.Fatalf("pending reseller list status=%d body=%s", pending.Code, pending.Body.String())
	}
	approved := request(http.MethodPost, "/v1/admin/resellers/97654321/approve", "96937669", `{}`)
	if approved.Code != 200 || !strings.Contains(approved.Body.String(), `"approval_status":"approved"`) {
		t.Fatalf("reseller approval status=%d body=%s", approved.Code, approved.Body.String())
	}
	if rec := request(http.MethodGet, "/v1/admin/resellers/pending", "96937669", ""); rec.Code != 200 || strings.Contains(rec.Body.String(), "97654321") {
		t.Fatalf("approved reseller remained pending: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := request(http.MethodPost, "/v1/admin/resellers/97654321/reject", "96937669", `{}`); rec.Code != http.StatusConflict {
		t.Fatalf("review replay should conflict, status=%d body=%s", rec.Code, rec.Body.String())
	}
	concurrentApplicant := resolve(t, s, "reseller-turk1", 97654322)
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for _, decision := range []string{"approved", "rejected"} {
		wg.Add(1)
		go func(status string) {
			defer wg.Done()
			_, err := s.SetResellerApproval(ctx, "reseller-turk1", admin.ID, concurrentApplicant.TelegramID, status)
			results <- err
		}(decision)
	}
	wg.Wait()
	close(results)
	successes, conflicts := 0, 0
	for err := range results {
		if err == nil {
			successes++
		} else if errors.Is(err, store.ErrConflict) {
			conflicts++
		} else {
			t.Fatalf("unexpected concurrent review error: %v", err)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("concurrent review was not a single pending transition: success=%d conflicts=%d", successes, conflicts)
	}
	var reviews int
	if err := s.DB.QueryRow(ctx, `SELECT count(*) FROM admin_configuration_audit WHERE deployment_id='reseller-turk1' AND actor_id=$1 AND action='reseller_approved' AND subject_ref='telegram:97654321'`, admin.ID).Scan(&reviews); err != nil {
		t.Fatal(err)
	}
	if reviews != 1 {
		t.Fatalf("approved reseller audit subject missing: %d", reviews)
	}
	var concurrentAudits int
	if err := s.DB.QueryRow(ctx, `SELECT count(*) FROM admin_configuration_audit WHERE deployment_id='reseller-turk1' AND actor_id=$1 AND action IN ('reseller_approved','reseller_rejected') AND subject_ref='telegram:97654322'`, admin.ID).Scan(&concurrentAudits); err != nil {
		t.Fatal(err)
	}
	if concurrentAudits != 1 {
		t.Fatalf("concurrent winning review should produce exactly one target audit row, got %d", concurrentAudits)
	}
	if err := s.SavePaymentInstructions(ctx, "retail-finland", 0, "should-rollback", "", ""); err == nil {
		t.Fatal("configuration change with invalid admin actor unexpectedly succeeded")
	}
	var card string
	if err := s.DB.QueryRow(ctx, `SELECT payment_card_number FROM deployments WHERE id='retail-finland'`).Scan(&card); err != nil {
		t.Fatal(err)
	}
	if card != "" {
		t.Fatal("configuration write was committed without its audit row")
	}
}
