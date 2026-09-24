package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"

	"example.com/xui-commerce/backend/internal/api"
	"example.com/xui-commerce/backend/internal/config"
	"example.com/xui-commerce/backend/internal/store"
)

func TestPaymentRejectIsAuditedIdempotentAndResumeIsActorScoped(t *testing.T) {
	s, _ := testStore(t)
	ctx := context.Background()
	customer := resolve(t, s, "retail-finland", 86101)
	other := resolve(t, s, "retail-finland", 86102)
	foreignAdmin := resolve(t, s, "reseller-turk1", 96937669)
	planID := addPlan(t, s, "retail-finland", "paid", "reject plan", 5000, 0)
	quote, err := s.CreateQuote(ctx, customer, planID, 1, 1, 0, "reject-payment-quote")
	if err != nil {
		t.Fatal(err)
	}
	purchase, err := s.CreatePurchase(ctx, customer, quote.ID, "direct", "reject-payment", "")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.SubmitReceipt(ctx, customer, purchase.PaymentIntentID, "tg-file-receipt-secret"); err != nil {
		t.Fatal(err)
	}
	active, err := s.ActivePaymentIntent(ctx, customer)
	if err != nil || active == nil || active["id"] != purchase.PaymentIntentID || active["status"] != "receipt_submitted" {
		t.Fatalf("resume intent=%v err=%v", active, err)
	}
	active, err = s.ActivePaymentIntent(ctx, other)
	if err != nil || active != nil {
		t.Fatalf("another actor saw active payment: %v err=%v", active, err)
	}
	if _, err = s.RejectPayment(ctx, foreignAdmin, purchase.PaymentIntentID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("cross-deployment reject err=%v", err)
	}
	admin := resolve(t, s, "retail-finland", 96937669)
	pending, err := s.PendingPayments(ctx, admin)
	if err != nil || len(pending) != 1 || pending[0]["telegram_file_id"] != "tg-file-receipt-secret" {
		t.Fatalf("admin receipt evidence=%v err=%v", pending, err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, e := s.RejectPayment(ctx, admin, purchase.PaymentIntentID); errs <- e }()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		if e != nil {
			t.Fatal(e)
		}
	}
	var status, orderStatus string
	if err = s.DB.QueryRow(ctx, `SELECT status FROM payment_intents WHERE id=$1`, purchase.PaymentIntentID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if err = s.DB.QueryRow(ctx, `SELECT status FROM orders WHERE id=$1`, purchase.OrderID).Scan(&orderStatus); err != nil {
		t.Fatal(err)
	}
	var audits, outs, settlements, works int
	if err = s.DB.QueryRow(ctx, `SELECT count(*) FROM admin_configuration_audit WHERE action='payment_rejected' AND subject_ref=$1`, "payment_intent:"+itoa(purchase.PaymentIntentID)).Scan(&audits); err != nil {
		t.Fatal(err)
	}
	if err = s.DB.QueryRow(ctx, `SELECT count(*) FROM outbox WHERE dedupe_key=$1`, "payment-rejected:"+itoa(purchase.PaymentIntentID)).Scan(&outs); err != nil {
		t.Fatal(err)
	}
	if err = s.DB.QueryRow(ctx, `SELECT count(*) FROM payment_settlements WHERE intent_id=$1`, purchase.PaymentIntentID).Scan(&settlements); err != nil {
		t.Fatal(err)
	}
	if err = s.DB.QueryRow(ctx, `SELECT count(*) FROM work_items WHERE payment_intent_id=$1`, purchase.PaymentIntentID).Scan(&works); err != nil {
		t.Fatal(err)
	}
	if status != "rejected" || orderStatus != "cancelled" || audits != 1 || outs != 1 || settlements != 0 || works != 0 {
		t.Fatalf("reject effects status=%s order=%s audit=%d outbox=%d settlements=%d work=%d", status, orderStatus, audits, outs, settlements, works)
	}
}

func TestTopupRejectHasNoCreditAndAdminReceiptOnly(t *testing.T) {
	s, _ := testStore(t)
	ctx := context.Background()
	customer := resolve(t, s, "retail-finland", 86201)
	admin := resolve(t, s, "retail-finland", 96937669)
	id, err := s.CreateTopup(ctx, customer, 7000, "reject-topup")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.SubmitTopupReceipt(ctx, customer, id, "tg-topup-evidence"); err != nil {
		t.Fatal(err)
	}
	resume, err := s.ActiveTopup(ctx, customer)
	if err != nil || resume == nil || resume["id"] != id {
		t.Fatalf("active topup=%v err=%v", resume, err)
	}
	pending, err := s.PendingTopups(ctx, admin)
	if err != nil || len(pending) != 1 || pending[0]["telegram_file_id"] != "tg-topup-evidence" {
		t.Fatalf("admin topup evidence=%v err=%v", pending, err)
	}
	if _, err = s.PendingTopups(ctx, customer); !errors.Is(err, store.ErrForbidden) {
		t.Fatalf("customer listed admin receipts: %v", err)
	}
	if _, err = s.RejectTopup(ctx, admin, id); err != nil {
		t.Fatal(err)
	}
	if _, err = s.RejectTopup(ctx, admin, id); err != nil {
		t.Fatalf("idempotent reject replay: %v", err)
	}
	var credits, ledger, audits, outs int
	if err = s.DB.QueryRow(ctx, `SELECT count(*) FROM wallet_credit_approvals WHERE topup_request_id=$1`, id).Scan(&credits); err != nil {
		t.Fatal(err)
	}
	if err = s.DB.QueryRow(ctx, `SELECT count(*) FROM wallet_ledger WHERE reference_type='topup_request' AND reference_id=$1`, id).Scan(&ledger); err != nil {
		t.Fatal(err)
	}
	if err = s.DB.QueryRow(ctx, `SELECT count(*) FROM admin_configuration_audit WHERE action='topup_rejected' AND subject_ref=$1`, "topup:"+itoa(id)).Scan(&audits); err != nil {
		t.Fatal(err)
	}
	if err = s.DB.QueryRow(ctx, `SELECT count(*) FROM outbox WHERE dedupe_key=$1`, "topup-rejected:"+itoa(id)).Scan(&outs); err != nil {
		t.Fatal(err)
	}
	if credits != 0 || ledger != 0 || audits != 1 || outs != 1 {
		t.Fatalf("topup reject effects credits=%d ledger=%d audit=%d outbox=%d", credits, ledger, audits, outs)
	}
}

func TestRefundRejectClosesOnlyPendingRequest(t *testing.T) {
	s, _ := testStore(t)
	ctx := context.Background()
	customer := resolve(t, s, "retail-finland", 86301)
	admin := resolve(t, s, "retail-finland", 96937669)
	plan := addPlan(t, s, "retail-finland", "paid", "refund reject", 9000, 0)
	if _, err := s.DB.Exec(ctx, `UPDATE commercial_accounts SET wallet_balance_toman=10000 WHERE id=$1`, customer.AccountID); err != nil {
		t.Fatal(err)
	}
	q, err := s.CreateQuote(ctx, customer, plan, 1, 1, 0, "refund-reject-q")
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.CreatePurchase(ctx, customer, q.ID, "wallet", "refund-reject-p", "x")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.DB.Exec(ctx, `UPDATE orders SET status='completed' WHERE id=$1`, p.OrderID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.DB.Exec(ctx, `UPDATE subscriptions SET status='cancelled' WHERE id=$1`, p.SubscriptionID); err != nil {
		t.Fatal(err)
	}
	r, err := s.RequestRefund(ctx, customer, p.SubscriptionID, "refund-reject-key", "cancelled service")
	if err != nil {
		t.Fatal(err)
	}
	before, err := s.Balance(ctx, customer)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.RejectRefund(ctx, admin, r.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.RejectRefund(ctx, admin, r.ID); err != nil {
		t.Fatal(err)
	}
	after, err := s.Balance(ctx, customer)
	if err != nil {
		t.Fatal(err)
	}
	var status string
	var audits, credits int
	if err = s.DB.QueryRow(ctx, `SELECT status FROM refund_requests WHERE id=$1`, r.ID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if err = s.DB.QueryRow(ctx, `SELECT count(*) FROM wallet_ledger WHERE reference_type='refund_request' AND reference_id=$1`, r.ID).Scan(&credits); err != nil {
		t.Fatal(err)
	}
	if err = s.DB.QueryRow(ctx, `SELECT count(*) FROM admin_configuration_audit WHERE action='refund_rejected' AND subject_ref=$1`, "refund:"+itoa(r.ID)).Scan(&audits); err != nil {
		t.Fatal(err)
	}
	if status != "rejected" || before != after || credits != 0 || audits != 1 {
		t.Fatalf("refund rejection status=%s balance=%d/%d credit=%d audit=%d", status, before, after, credits, audits)
	}
	if _, err = s.ApproveRefund(ctx, admin, r.ID, 9000, "should conflict with rejection", "refund-after-reject", false); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("approved rejected refund: %v", err)
	}
}

func TestAdminReceiptEndpointsRejectRegularActor(t *testing.T) {
	s, _ := testStore(t)
	resolve(t, s, "retail-finland", 86401)
	token := "retail-finland-reject-test-token-000000000000"
	h := api.New(s, config.Config{ClientCredentials: []config.ClientCredential{{DeploymentID: "retail-finland", Token: token}}}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	for _, path := range []string{"/v1/admin/payments", "/v1/admin/topups"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("X-Actor-Telegram-ID", "86401")
		res := httptest.NewRecorder()
		h.ServeHTTP(res, req)
		if res.Code != http.StatusForbidden {
			t.Fatalf("regular actor accessed %s: %d %s", path, res.Code, res.Body.String())
		}
	}
}

func TestActiveRequestAPIContractSurvivesRestartAndScopesDeployment(t *testing.T) {
	s, _ := testStore(t)
	ctx := context.Background()
	finland := resolve(t, s, "retail-finland", 86501)
	germany := resolve(t, s, "retail-germany", 86501)
	plan := addPlan(t, s, "retail-finland", "paid", "restart purchase", 5000, 0)
	quote, err := s.CreateQuote(ctx, finland, plan, 1, 1, 0, "resume-api-quote")
	if err != nil {
		t.Fatal(err)
	}
	purchase, err := s.CreatePurchase(ctx, finland, quote.ID, "direct", "resume-api-purchase", "")
	if err != nil {
		t.Fatal(err)
	}
	topupID, err := s.CreateTopup(ctx, finland, 8000, "resume-api-topup")
	if err != nil {
		t.Fatal(err)
	}
	// Same Telegram ID in another deployment owns different records. The Finland API must never return those.
	germanyPlan := addPlan(t, s, "retail-germany", "paid", "Germany plan", 6000, 0)
	germanyQuote, err := s.CreateQuote(ctx, germany, germanyPlan, 1, 1, 0, "resume-germany-quote")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.CreatePurchase(ctx, germany, germanyQuote.ID, "direct", "resume-germany-purchase", ""); err != nil {
		t.Fatal(err)
	}
	if _, err = s.CreateTopup(ctx, germany, 9000, "resume-germany-topup"); err != nil {
		t.Fatal(err)
	}
	token := "retail-finland-resume-test-token-000000000000"
	handler := api.New(s, config.Config{ClientCredentials: []config.ClientCredential{{DeploymentID: "retail-finland", Token: token}}}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	get := func(path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("X-Actor-Telegram-ID", "86501")
		res := httptest.NewRecorder()
		handler.ServeHTTP(res, req)
		return res
	}
	payment := get("/v1/payment-intents/active")
	if payment.Code != http.StatusOK {
		t.Fatalf("active payment status=%d body=%s", payment.Code, payment.Body.String())
	}
	var paymentBody struct {
		Intent *struct {
			ID      int64  `json:"id"`
			Status  string `json:"status"`
			Amount  int64  `json:"amount_toman"`
			Receipt string `json:"telegram_file_id"`
		} `json:"payment_intent"`
	}
	if err = json.NewDecoder(bytes.NewReader(payment.Body.Bytes())).Decode(&paymentBody); err != nil {
		t.Fatal(err)
	}
	if paymentBody.Intent == nil || paymentBody.Intent.ID != purchase.PaymentIntentID || paymentBody.Intent.Status != "awaiting_receipt" || paymentBody.Intent.Amount != 5000 || paymentBody.Intent.Receipt != "" {
		t.Fatalf("unexpected resumable payment response: %+v", paymentBody)
	}
	topup := get("/v1/wallet/topups/active")
	if topup.Code != http.StatusOK {
		t.Fatalf("active topup status=%d body=%s", topup.Code, topup.Body.String())
	}
	var topupBody struct {
		Topup *struct {
			ID     int64  `json:"id"`
			Status string `json:"status"`
			Amount int64  `json:"amount_toman"`
		} `json:"topup"`
	}
	if err = json.NewDecoder(bytes.NewReader(topup.Body.Bytes())).Decode(&topupBody); err != nil {
		t.Fatal(err)
	}
	if topupBody.Topup == nil || topupBody.Topup.ID != topupID || topupBody.Topup.Status != "awaiting_receipt" || topupBody.Topup.Amount != 8000 {
		t.Fatalf("unexpected resumable topup response: %+v", topupBody)
	}
}

// Small local helpers avoid coupling these tests to any deployed bot credential.
func itoa(v int64) string { return strconv.FormatInt(v, 10) }
