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
	"reflect"
	"strings"
	"sync"
	"testing"

	"example.com/xui-commerce/backend/internal/api"
	"example.com/xui-commerce/backend/internal/commerce"
	"example.com/xui-commerce/backend/internal/config"
	"example.com/xui-commerce/backend/internal/store"
	"example.com/xui-commerce/backend/internal/xui"
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
	resolve(t, s, "retail-finland", 971991)
	if rec := request(http.MethodGet, "/v1/features", finToken, "971991", ""); rec.Code != 200 {
		t.Fatalf("public feature config status=%d body=%s", rec.Code, rec.Body.String())
	}
	plan := `{"name":"admin draft","kind":"paid","enabled":false,"is_limited":false,"description":"","base_price_toman":1000,"price_per_extra_ip_toman":0,"price_per_gb_toman":0,"price_per_extra_month_toman":0,"base_ip_limit":1,"max_ip_limit":1,"min_data_gb":0,"max_data_bytes":0,"expire_seconds":0,"test_ip_limit":1,"max_per_day":0,"flow":"","inbound_ids":[],"discount_tiers":[{"months":12,"basis_points":750}],"usage_description":""}`
	created := request(http.MethodPost, "/v1/admin/config/plans", finToken, "96937669", plan)
	if created.Code != http.StatusCreated {
		t.Fatalf("plan create status=%d body=%s", created.Code, created.Body.String())
	}
	missingInbound := strings.Replace(plan, `"enabled":false`, `"enabled":true`, 1)
	if rec := request(http.MethodPost, "/v1/admin/config/plans", finToken, "96937669", missingInbound); rec.Code != http.StatusBadRequest {
		t.Fatalf("enabled plan without an inbound was accepted: status=%d body=%s", rec.Code, rec.Body.String())
	}
	duplicateDiscount := strings.Replace(plan, `[{"months":12,"basis_points":750}]`, `[{"months":12,"basis_points":750},{"months":12,"basis_points":500}]`, 1)
	if rec := request(http.MethodPost, "/v1/admin/config/plans", finToken, "96937669", duplicateDiscount); rec.Code != http.StatusBadRequest {
		t.Fatalf("plan with duplicate discount durations was accepted: status=%d body=%s", rec.Code, rec.Body.String())
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
	var discountsJSON string
	if err := s.DB.QueryRow(ctx, `SELECT discount_tiers::text FROM plans WHERE deployment_id='retail-finland' AND id=$1`, createdPlan.ID).Scan(&discountsJSON); err != nil {
		t.Fatal(err)
	}
	var discounts []struct {
		Months      int   `json:"months"`
		BasisPoints int64 `json:"basis_points"`
	}
	if err := json.Unmarshal([]byte(discountsJSON), &discounts); err != nil {
		t.Fatal(err)
	}
	if len(discounts) != 1 || discounts[0].Months != 12 || discounts[0].BasisPoints != 750 {
		t.Fatalf("admin plan API did not preserve integer discount tiers: %+v", discounts)
	}
	legacyUpdate := strings.Replace(plan, `,"discount_tiers":[{"months":12,"basis_points":750}]`, "", 1)
	updated := request(http.MethodPut, fmt.Sprintf("/v1/admin/config/plans/%d", createdPlan.ID), finToken, "96937669", legacyUpdate)
	if updated.Code != http.StatusOK {
		t.Fatalf("update without optional discount_tiers failed: status=%d body=%s", updated.Code, updated.Body.String())
	}
	if err := s.DB.QueryRow(ctx, `SELECT discount_tiers::text FROM plans WHERE deployment_id='retail-finland' AND id=$1`, createdPlan.ID).Scan(&discountsJSON); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(discountsJSON), &discounts); err != nil {
		t.Fatal(err)
	}
	if len(discounts) != 1 || discounts[0].Months != 12 || discounts[0].BasisPoints != 750 {
		t.Fatalf("legacy admin update erased saved discount tiers: %+v", discounts)
	}

	allowed := resolve(t, s, "retail-finland", 971991)
	restrictedPlan := strings.Replace(plan, `"name":"admin draft"`, `"name":"restricted draft"`, 1)
	restrictedPlan = strings.TrimSuffix(restrictedPlan, "}") + fmt.Sprintf(`,"allowed_telegram_ids":[%d]}`, allowed.TelegramID)
	restrictedCreated := request(http.MethodPost, "/v1/admin/config/plans", finToken, "96937669", restrictedPlan)
	if restrictedCreated.Code != http.StatusCreated {
		t.Fatalf("restricted plan create status=%d body=%s", restrictedCreated.Code, restrictedCreated.Body.String())
	}
	var restricted struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(restrictedCreated.Body.Bytes(), &restricted); err != nil {
		t.Fatal(err)
	}
	var isGlobal bool
	var grants int
	if err := s.DB.QueryRow(ctx, `SELECT is_global FROM plans WHERE deployment_id='retail-finland' AND id=$1`, restricted.ID).Scan(&isGlobal); err != nil {
		t.Fatal(err)
	}
	if isGlobal {
		t.Fatal("allowlisted plan was created as global")
	}
	if err := s.DB.QueryRow(ctx, `SELECT count(*) FROM plan_access WHERE deployment_id='retail-finland' AND plan_id=$1`, restricted.ID).Scan(&grants); err != nil {
		t.Fatal(err)
	}
	if grants != 1 {
		t.Fatalf("restricted plan has %d grants, want 1", grants)
	}
	withoutAccess := strings.Replace(restrictedPlan, fmt.Sprintf(`,"allowed_telegram_ids":[%d]`, allowed.TelegramID), "", 1)
	if rec := request(http.MethodPut, fmt.Sprintf("/v1/admin/config/plans/%d", restricted.ID), finToken, "96937669", withoutAccess); rec.Code != http.StatusOK {
		t.Fatalf("legacy plan update without allowlist status=%d body=%s", rec.Code, rec.Body.String())
	}
	if err := s.DB.QueryRow(ctx, `SELECT count(*) FROM plan_access WHERE deployment_id='retail-finland' AND plan_id=$1`, restricted.ID).Scan(&grants); err != nil || grants != 1 {
		t.Fatalf("omitted allowlist must preserve grants: count=%d err=%v", grants, err)
	}
	globalPlan := strings.Replace(restrictedPlan, fmt.Sprintf(`"allowed_telegram_ids":[%d]`, allowed.TelegramID), `"allowed_telegram_ids":[]`, 1)
	if rec := request(http.MethodPut, fmt.Sprintf("/v1/admin/config/plans/%d", restricted.ID), finToken, "96937669", globalPlan); rec.Code != http.StatusOK {
		t.Fatalf("global plan update status=%d body=%s", rec.Code, rec.Body.String())
	}
	if err := s.DB.QueryRow(ctx, `SELECT is_global FROM plans WHERE deployment_id='retail-finland' AND id=$1`, restricted.ID).Scan(&isGlobal); err != nil || !isGlobal {
		t.Fatalf("empty allowlist should make plan global: global=%t err=%v", isGlobal, err)
	}
}

func TestAdminCreateEnabledPlanPersistsInboundIDsAndReadsItBack(t *testing.T) {
	s, _ := testStore(t)
	const token = "retail-finland-plan-create-test-token-000000"
	handler := api.New(s, config.Config{ClientCredentials: []config.ClientCredential{{DeploymentID: "retail-finland", Token: token}}}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	input := store.AdminPlan{
		Name: "Plan create integration", Kind: "paid", Enabled: true,
		BasePriceToman: 25000, BaseIPLimit: 1, MaxIPLimit: 4,
		InboundIDs: []int{12, 21}, DiscountTiers: []commerce.DiscountTier{{Months: 3, BasisPoints: 500}},
		IsGlobal: true, AllowedTelegramIDs: []int64{},
	}
	body, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/admin/config/plans", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-Actor-Telegram-ID", "96937669")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create enabled plan status=%d body=%s", rec.Code, rec.Body.String())
	}
	var response struct {
		ID     int64                    `json:"id"`
		Config store.AdminConfiguration `json:"config"`
	}
	if err = json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	var saved *store.AdminPlan
	for i := range response.Config.Plans {
		if response.Config.Plans[i].ID == response.ID {
			saved = &response.Config.Plans[i]
			break
		}
	}
	if response.ID <= 0 || response.Config.DeploymentID != "retail-finland" || saved == nil {
		t.Fatalf("create response did not read back plan %d for correct deployment: %+v", response.ID, response.Config)
	}
	if !reflect.DeepEqual(saved.InboundIDs, input.InboundIDs) || saved.Name != input.Name || len(saved.DiscountTiers) != 1 || saved.DiscountTiers[0].BasisPoints != 500 {
		t.Fatalf("created plan readback mismatch: %+v", saved)
	}
	var inboundIDs []int32
	if err = s.DB.QueryRow(context.Background(), `SELECT inbound_ids FROM plans WHERE deployment_id='retail-finland' AND id=$1`, response.ID).Scan(&inboundIDs); err != nil {
		t.Fatalf("read persisted inbound array: %v", err)
	}
	if !reflect.DeepEqual(inboundIDs, []int32{12, 21}) {
		t.Fatalf("persisted inbounds = %v, want [12 21]", inboundIDs)
	}
}

func TestAdminInboundPickerReturnsAuthenticatedPanelOptions(t *testing.T) {
	s, _ := testStore(t)
	const token = "retail-finland-test-token-000000000000"
	var panelToken string
	panelServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/panel/api/inbounds/options" {
			t.Errorf("unexpected panel path %s", r.URL.Path)
		}
		panelToken = r.Header.Get("Authorization")
		_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "obj": []xui.InboundOption{{ID: 12, Remark: "Main TLS", Tag: "vless-in", Protocol: "vless", Port: 443, Enable: true}}})
	}))
	defer panelServer.Close()
	if _, err := s.DB.Exec(context.Background(), `UPDATE panels SET base_url=$1 WHERE id='panel-retail-finland'`, panelServer.URL); err != nil {
		t.Fatal(err)
	}
	handler := api.New(s, config.Config{
		ClientCredentials: []config.ClientCredential{{DeploymentID: "retail-finland", Token: token}},
		PanelTokens:       map[string]string{"panel-retail-finland": "panel-secret"},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	req := httptest.NewRequest(http.MethodGet, "/v1/admin/panels/inbounds", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-Actor-Telegram-ID", "96937669")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("inbound picker status=%d body=%s", rec.Code, rec.Body.String())
	}
	if panelToken != "Bearer panel-secret" {
		t.Fatalf("configured panel credential was not used: auth=%q", panelToken)
	}
	var result struct {
		Inbounds []xui.InboundOption `json:"inbounds"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Inbounds) != 1 || result.Inbounds[0].ID != 12 || result.Inbounds[0].Remark != "Main TLS" || result.Inbounds[0].Protocol != "vless" || result.Inbounds[0].Port != 443 || !result.Inbounds[0].Enable {
		t.Fatalf("picker metadata was incomplete: %+v", result.Inbounds)
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

func TestAuditSubjectRefMigrationUpgradesExisting008Schema(t *testing.T) {
	s, _ := testStore(t)
	ctx := context.Background()
	if _, err := s.DB.Exec(ctx, `ALTER TABLE admin_configuration_audit DROP COLUMN subject_ref`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec(ctx, `DELETE FROM schema_migrations WHERE version='009_admin_audit_subject_ref.sql'`); err != nil {
		t.Fatal(err)
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("apply upgrade migration: %v", err)
	}
	var exists bool
	if err := s.DB.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM information_schema.columns WHERE table_schema=current_schema() AND table_name='admin_configuration_audit' AND column_name='subject_ref')`).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if !exists {
		t.Fatal("upgrade migration did not restore subject_ref")
	}
}

func TestPrivilegedHTTPRoutesRequireDesignatedAdminIdentity(t *testing.T) {
	s, _ := testStore(t)
	ordinary := resolve(t, s, "retail-finland", 971992)
	if _, err := s.DB.Exec(context.Background(), `UPDATE actors SET role='admin',approval_status='approved',enabled=true WHERE id=$1`, ordinary.ID); err != nil {
		t.Fatal(err)
	}
	const token = "retail-finland-test-token-000000000000"
	handler := api.New(s, config.Config{ClientCredentials: []config.ClientCredential{{DeploymentID: "retail-finland", Token: token}}}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	paths := []struct{ method, path string }{
		{http.MethodGet, "/v1/admin/payments"},
		{http.MethodPost, "/v1/payment-intents/1/approve"},
		{http.MethodGet, "/v1/admin/topups"},
		{http.MethodPost, "/v1/wallet/topups/1/approve"},
		{http.MethodGet, "/v1/admin/refunds"},
		{http.MethodPost, "/v1/admin/refunds/1/approve"},
		{http.MethodGet, "/v1/admin/work-items"},
		{http.MethodGet, "/v1/admin/config"},
		{http.MethodGet, "/v1/admin/panels/inbounds"},
		{http.MethodGet, "/v1/admin/resellers/pending"},
		{http.MethodPost, "/v1/admin/resellers/97654399/approve"},
		{http.MethodPost, "/v1/admin/resellers/97654399/reject"},
		{http.MethodPost, "/v1/admin/config/plans"},
		{http.MethodPut, "/v1/admin/config/plans/1"},
		{http.MethodPatch, "/v1/admin/config/payment-instructions"},
		{http.MethodPatch, "/v1/admin/config/settings"},
		{http.MethodPut, "/v1/admin/config/panel"},
	}
	for _, tc := range paths {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(`{}`))
			req.Header.Set("Authorization", "Bearer "+token)
			req.Header.Set("X-Actor-Telegram-ID", "971992")
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("database-assigned admin role bypassed designated ID guard: status=%d body=%s", rec.Code, rec.Body.String())
			}
		})
	}
	validReq := httptest.NewRequest(http.MethodGet, "/v1/admin/payments", nil)
	validReq.Header.Set("Authorization", "Bearer "+token)
	validReq.Header.Set("X-Actor-Telegram-ID", "96937669")
	validRec := httptest.NewRecorder()
	handler.ServeHTTP(validRec, validReq)
	if validRec.Code != http.StatusOK {
		t.Fatalf("designated administrator was denied an admin route: status=%d body=%s", validRec.Code, validRec.Body.String())
	}
}
