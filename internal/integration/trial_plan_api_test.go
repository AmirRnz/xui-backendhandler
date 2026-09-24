package integration

import (
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

func TestResellerPublicTrialPlansExposeExistingPolicyFields(t *testing.T) {
	s, _ := testStore(t)
	ctx := context.Background()
	resolve(t, s, "reseller-turk1", 97654383)
	planID := addPlan(t, s, "reseller-turk1", "test", "Daily test", 0, 6)
	if _, err := s.DB.Exec(ctx, `UPDATE plans SET test_ip_limit=3 WHERE deployment_id='reseller-turk1' AND id=$1`, planID); err != nil {
		t.Fatal(err)
	}
	const token = "reseller-turk1-trial-plan-test-token-000000"
	handler := api.New(s, config.Config{ClientCredentials: []config.ClientCredential{{DeploymentID: "reseller-turk1", Token: token}}}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	req := httptest.NewRequest(http.MethodGet, "/v1/plans?kind=test", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-Actor-Telegram-ID", "97654383")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, req)
	if response.Code != http.StatusOK {
		t.Fatalf("trial plan list status=%d body=%s", response.Code, response.Body.String())
	}
	var plans []struct {
		ID          int64 `json:"id"`
		TestIPLimit int   `json:"test_ip_limit"`
		MaxPerDay   int   `json:"max_per_day"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &plans); err != nil {
		t.Fatal(err)
	}
	if len(plans) != 1 || plans[0].ID != planID || plans[0].TestIPLimit != 3 || plans[0].MaxPerDay != 6 {
		t.Fatalf("public trial plan omitted or changed policy fields: %+v", plans)
	}
	if strings.Contains(response.Body.String(), "panel_id") || strings.Contains(response.Body.String(), "inbound_ids") {
		t.Fatalf("public plan response exposed backend-only panel details: %s", response.Body.String())
	}
}
