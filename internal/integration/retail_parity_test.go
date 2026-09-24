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
	"example.com/xui-commerce/backend/internal/commerce"
	"example.com/xui-commerce/backend/internal/config"
)

func retailParityRequest(handler http.Handler, method, path, telegramID, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer retail-finland-parity-token-000000000000")
	req.Header.Set("X-Actor-Telegram-ID", telegramID)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func TestRetailPublicPlansExposeDescriptionAndStoredDiscountTiers(t *testing.T) {
	s, _ := testStore(t)
	ctx := context.Background()
	applicant := resolve(t, s, "retail-finland", 97541001)
	planID := addPlan(t, s, "retail-finland", "paid", "Parity plan", 1000, 0)
	if _, err := s.DB.Exec(ctx, `UPDATE plans SET description=$1,discount_tiers=$2::jsonb WHERE deployment_id='retail-finland' AND id=$3`, "Plan details from legacy", `[{"months":12,"basis_points":500}]`, planID); err != nil {
		t.Fatal(err)
	}
	handler := api.New(s, config.Config{ClientCredentials: []config.ClientCredential{{DeploymentID: "retail-finland", Token: "retail-finland-parity-token-000000000000"}}}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	response := retailParityRequest(handler, http.MethodGet, "/v1/plans?kind=paid", "97541001", "")
	if response.Code != http.StatusOK {
		t.Fatalf("plan list status=%d body=%s", response.Code, response.Body.String())
	}
	var plans []struct {
		ID            int64  `json:"id"`
		Description   string `json:"description"`
		DiscountTiers []struct {
			Months      int   `json:"months"`
			BasisPoints int64 `json:"basis_points"`
		} `json:"discount_tiers"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &plans); err != nil {
		t.Fatal(err)
	}
	if len(plans) != 1 || plans[0].ID != planID || plans[0].Description != "Plan details from legacy" || len(plans[0].DiscountTiers) != 1 || plans[0].DiscountTiers[0].Months != 12 || plans[0].DiscountTiers[0].BasisPoints != 500 {
		t.Fatalf("public plan did not expose persisted legacy fields: %+v", plans)
	}

	quote, err := s.CreateQuote(ctx, applicant, planID, 12, 1, 0, "retail-parity-quote")
	if err != nil {
		t.Fatal(err)
	}
	if quote.DiscountToman != 600 || quote.FinalPriceToman != 11400 {
		t.Fatalf("existing quote arithmetic changed: discount=%d final=%d", quote.DiscountToman, quote.FinalPriceToman)
	}
	if _, err = s.DB.Exec(ctx, `UPDATE plans SET description='Changed after quote',discount_tiers='[{"months":12,"basis_points":1000}]'::jsonb WHERE deployment_id='retail-finland' AND id=$1`, planID); err != nil {
		t.Fatal(err)
	}
	var termsJSON []byte
	if err = s.DB.QueryRow(ctx, `SELECT terms FROM purchase_quotes WHERE deployment_id='retail-finland' AND account_id=$1 AND quote_key='retail-parity-quote'`, applicant.AccountID).Scan(&termsJSON); err != nil {
		t.Fatal(err)
	}
	var storedTerms commerce.Quote
	if err = json.Unmarshal(termsJSON, &storedTerms); err != nil {
		t.Fatal(err)
	}
	if storedTerms.DiscountToman != 600 || storedTerms.FinalPriceToman != 11400 {
		t.Fatalf("catalog edit altered immutable quote terms: %+v", storedTerms)
	}
}

func TestRetailMinimumTopupIsConfigurableExposedAndEnforced(t *testing.T) {
	s, _ := testStore(t)
	resolve(t, s, "retail-finland", 96937669)
	user := resolve(t, s, "retail-finland", 97541002)
	handler := api.New(s, config.Config{ClientCredentials: []config.ClientCredential{{DeploymentID: "retail-finland", Token: "retail-finland-parity-token-000000000000"}}}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	setMinimum := retailParityRequest(handler, http.MethodPatch, "/v1/admin/config/settings", "96937669", `{"min_topup_toman":5000}`)
	if setMinimum.Code != http.StatusOK {
		t.Fatalf("set min topup status=%d body=%s", setMinimum.Code, setMinimum.Body.String())
	}
	var settingsResponse struct {
		Settings map[string]any `json:"settings"`
	}
	if err := json.Unmarshal(setMinimum.Body.Bytes(), &settingsResponse); err != nil {
		t.Fatal(err)
	}
	if settingsResponse.Settings["min_topup_toman"] != float64(5000) {
		t.Fatalf("admin config did not expose min_topup_toman: %+v", settingsResponse.Settings)
	}

	instructions := retailParityRequest(handler, http.MethodGet, "/v1/payment-instructions", "97541002", "")
	if instructions.Code != http.StatusOK || !strings.Contains(instructions.Body.String(), `"min_topup_toman":5000`) {
		t.Fatalf("payment instructions did not expose minimum: status=%d body=%s", instructions.Code, instructions.Body.String())
	}
	belowMinimum := retailParityRequest(handler, http.MethodPost, "/v1/wallet/topups", "97541002", `{"amount_toman":4999,"idempotency_key":"below-minimum"}`)
	if belowMinimum.Code != http.StatusBadRequest || !strings.Contains(belowMinimum.Body.String(), `"code":"min_topup_not_met"`) {
		t.Fatalf("below-minimum topup status=%d body=%s", belowMinimum.Code, belowMinimum.Body.String())
	}
	var topupCount int
	if err := s.DB.QueryRow(context.Background(), `SELECT count(*) FROM topup_requests WHERE deployment_id='retail-finland' AND account_id=$1`, user.AccountID).Scan(&topupCount); err != nil {
		t.Fatal(err)
	}
	if topupCount != 0 {
		t.Fatalf("below-minimum topup created a row: count=%d", topupCount)
	}
	accepted := retailParityRequest(handler, http.MethodPost, "/v1/wallet/topups", "97541002", `{"amount_toman":5000,"idempotency_key":"at-minimum"}`)
	if accepted.Code != http.StatusCreated {
		t.Fatalf("topup at configured minimum was rejected: status=%d body=%s", accepted.Code, accepted.Body.String())
	}
	resolve(t, s, "retail-germany", 97541002)
	germanyToken := "retail-germany-parity-token-000000000000"
	germanyHandler := api.New(s, config.Config{ClientCredentials: []config.ClientCredential{{DeploymentID: "retail-germany", Token: germanyToken}}}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	request := httptest.NewRequest(http.MethodGet, "/v1/payment-instructions", nil)
	request.Header.Set("Authorization", "Bearer "+germanyToken)
	request.Header.Set("X-Actor-Telegram-ID", "97541002")
	response := httptest.NewRecorder()
	germanyHandler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"min_topup_toman":0`) {
		t.Fatalf("retail Finland minimum leaked to Germany: status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestResellerAccessRequestRowsEnforceActorDeploymentScope(t *testing.T) {
	s, _ := testStore(t)
	retailActor := resolve(t, s, "retail-finland", 97541003)
	if _, err := s.DB.Exec(context.Background(), `INSERT INTO reseller_access_requests(deployment_id,actor_id) VALUES('reseller-turk1',$1)`, retailActor.ID); err == nil {
		t.Fatal("reseller access request accepted an actor from another deployment")
	}
}
