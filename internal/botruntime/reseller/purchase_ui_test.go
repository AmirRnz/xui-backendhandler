package reseller

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"example.com/xui-commerce/backend/internal/botruntime/backend"
)

func TestPurchaseFlowRequiresCurrentStepAndUnexpiredState(t *testing.T) {
	a := &botApp{flows: map[int64]conversation{42: {
		Step: "months", PlanID: 7, Vals: map[string]string{}, Expires: time.Now().Add(time.Minute),
	}}}
	if _, ok := a.currentPurchaseFlow(42, "months"); !ok {
		t.Fatal("active months step should be usable")
	}
	if _, ok := a.currentPurchaseFlow(42, "ip"); ok {
		t.Fatal("callback for a different wizard step must be rejected")
	}
	a.flows[42] = conversation{Step: "months", Vals: map[string]string{}, Expires: time.Now().Add(-time.Second)}
	if _, ok := a.purchaseFlow(42); ok {
		t.Fatal("expired wizard should not be restorable by a back button")
	}
}

func TestCustomIPInputCoversEveryConfiguredValuePastInlinePreview(t *testing.T) {
	p := plan{BaseIP: 2, MaxIP: 14}
	preview := purchaseIPPreview(p.BaseIP, p.MaxIP)
	if len(preview) != 8 || preview[0] != 2 || preview[len(preview)-1] != 9 {
		t.Fatalf("IP preview = %v, want first eight limits 2 through 9", preview)
	}
	if !needsCustomPurchaseIP(p.BaseIP, p.MaxIP) {
		t.Fatal("plan with limits beyond inline preview must expose custom IP input")
	}
	for value := p.BaseIP; value <= p.MaxIP; value++ {
		if !validPurchaseIPLimit(value, p) {
			t.Errorf("configured IP limit %d became unreachable", value)
		}
	}
	if validPurchaseIPLimit(p.BaseIP-1, p) || validPurchaseIPLimit(p.MaxIP+1, p) {
		t.Fatal("custom IP input must reject values outside backend plan bounds")
	}
}

func TestTopupRetryKeepsTheOriginalAmountAndIdempotencyKey(t *testing.T) {
	app := &botApp{flows: map[int64]conversation{42: {
		Step: "topup-submit", Vals: map[string]string{"amount_toman": "50000", "operation_key": "stable-key"}, Expires: time.Now().Add(time.Minute),
	}}}
	st, ok := app.currentPurchaseFlow(42, "topup-submit")
	if !ok || st.Vals["amount_toman"] != "50000" || st.Vals["operation_key"] != "stable-key" {
		t.Fatalf("retry state = %#v, active=%t", st, ok)
	}
}

func TestActiveReceiptResponsesRecoverPendingPaymentAndTopup(t *testing.T) {
	active := activeReceiptStates(activeReceiptSet{
		Payments: activeReceiptPage{Items: []activeReceipt{{ID: 11, Status: "awaiting_receipt", Amount: 90000}}},
		Topups:   activeReceiptPage{Items: []activeReceipt{{ID: 12, Status: "receipt_submitted", Amount: 50000}}},
	})
	if len(active) != 2 || active[0] != (receiptState{Kind: "payment", ID: 11}) || active[1] != (receiptState{Kind: "topup", ID: 12}) {
		t.Fatalf("recovered receipts = %#v", active)
	}
	if got := activeReceiptStates(activeReceiptSet{}); len(got) != 0 {
		t.Fatalf("empty active receipt response = %#v", got)
	}
}

func TestPhotoRecoveryAfterRestartUsesBackendActiveRequests(t *testing.T) {
	app := &botApp{receipts: map[int64]receiptState{}}
	if len(app.receipts) != 0 {
		t.Fatal("simulated restarted process should have no in-memory receipt state")
	}
	active := activeReceiptSet{
		Payments: activeReceiptPage{Items: []activeReceipt{
			{ID: 11, Status: "awaiting_receipt"},
			{ID: 10, Status: "receipt_submitted"},
		}},
		Topups: activeReceiptPage{Items: []activeReceipt{
			{ID: 12, Status: "receipt_submitted"},
		}},
	}
	got := receiptCandidatesForPhoto(active, true)
	if len(got) != 1 || got[0] != (receiptState{Kind: "payment", ID: 11}) {
		t.Fatalf("recoverable receipts = %#v; want only the awaiting payment intent", got)
	}
	active.Topups.Items[0].Status = "awaiting_receipt"
	if got = receiptCandidatesForPhoto(active, true); len(got) != 2 {
		t.Fatalf("multiple awaiting receipts = %#v; want both to require explicit selection", got)
	}
	if got = receiptCandidatesForPhoto(active, false); len(got) != 0 {
		t.Fatalf("incomplete scan must not auto-associate a photo, got %#v", got)
	}
}

func TestRestartPhotoRecoveryFindsOlderAwaitingRequestBehindNewerSubmitted(t *testing.T) {
	app := &botApp{receipts: map[int64]receiptState{}}
	if len(app.receipts) != 0 {
		t.Fatal("simulated restarted process should have no in-memory receipt state")
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/payment-intents/active":
			switch r.URL.Query().Get("before_id") {
			case "":
				_, _ = w.Write([]byte(`{"payment_intents":[{"id":19,"status":"receipt_submitted"}],"payment_intent":{"id":19,"status":"receipt_submitted"},"next_cursor":19}`))
			case "19":
				_, _ = w.Write([]byte(`{"payment_intents":[{"id":18,"status":"awaiting_receipt"}],"payment_intent":{"id":18,"status":"awaiting_receipt"},"next_cursor":null}`))
			default:
				t.Errorf("unexpected payment cursor: %s", r.URL.String())
				http.NotFound(w, r)
			}
		case "/v1/wallet/topups/active":
			_, _ = w.Write([]byte(`{"topups":[],"topup":null,"next_cursor":null}`))
		default:
			t.Errorf("unexpected request: %s", r.URL.String())
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	app.api = &backend.Client{BaseURL: server.URL, Token: "test", HTTP: server.Client()}
	active, complete, err := app.awaitingPhotoReceipts(nil, 123)
	if err != nil || !complete {
		t.Fatalf("active receipt recovery complete=%t err=%v", complete, err)
	}
	got := receiptCandidatesForPhoto(active, complete)
	if len(got) != 1 || got[0] != (receiptState{Kind: "payment", ID: 18}) {
		t.Fatalf("recovered receipts = %#v; expected older awaiting request #18", got)
	}
}

func TestActiveReceiptPaginationUsesExclusiveCursorAndBoundsMenuPages(t *testing.T) {
	var gotCursor string
	app := &botApp{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCursor = r.URL.Query().Get("before_id")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"topups":[{"id":8,"status":"awaiting_receipt"}],"next_cursor":null}`))
	}))
	defer server.Close()
	app.api = &backend.Client{BaseURL: server.URL, Token: "test", HTTP: server.Client()}
	page, err := app.activeReceiptPage(nil, 123, "topup", 9)
	if err != nil {
		t.Fatal(err)
	}
	if gotCursor != "9" || len(page.Items) != 1 || page.Items[0].ID != 8 {
		t.Fatalf("cursor=%q page=%#v", gotCursor, page)
	}
	if resumePageSize != 8 {
		t.Fatalf("resume list page size=%d; want bounded menu of 8", resumePageSize)
	}
}

func TestActiveReceiptPageSupportsLegacySingularResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"payment_intent":{"id":4,"status":"awaiting_receipt"}}`))
	}))
	defer server.Close()
	app := &botApp{api: &backend.Client{BaseURL: server.URL, Token: "test", HTTP: server.Client()}}
	page, err := app.activeReceiptPage(nil, 123, "payment", 0)
	if err != nil || len(page.Items) != 1 || page.Items[0].ID != 4 {
		t.Fatalf("legacy response page=%#v err=%v", page, err)
	}
}

func TestAdminReviewRequiresReceiptEvidenceAndUsesScopedRejectPaths(t *testing.T) {
	item := adminReviewItem{ID: 5, AccountID: 8, ActorID: 9, TelegramID: 96937669, Amount: 75000, Status: "receipt_submitted", CreatedAt: "2026-09-24T10:00:00Z", TelegramFileID: "private-file-id"}
	if !hasAdminReviewEvidence(item) {
		t.Fatal("receipt-submitted item with file evidence should permit review")
	}
	if strings.Contains(adminReviewSummary("payment", item), item.TelegramFileID) {
		t.Fatal("admin summary must not expose Telegram file ID")
	}
	if !strings.Contains(adminReviewSummary("payment", item), "تلگرام 96937669") {
		t.Fatal("admin summary should show backend-provided applicant Telegram ID")
	}
	item.TelegramFileID = ""
	if hasAdminReviewEvidence(item) {
		t.Fatal("item without receipt evidence must not permit approval")
	}
	item.TelegramID = 0
	if got := adminReviewApplicant(item); !strings.Contains(got, "actor در backend") {
		t.Fatalf("missing Telegram ID should be labeled as an internal actor ID, got %q", got)
	}
	for _, tc := range []struct{ kind, want string }{
		{"payment", "/v1/payment-intents/5/reject"},
		{"topup", "/v1/admin/topups/5/reject"},
	} {
		got, ok := adminRejectPath(tc.kind, 5)
		if !ok || got != tc.want {
			t.Errorf("adminRejectPath(%q) = %q, %t; want %q", tc.kind, got, ok, tc.want)
		}
	}
	if _, ok := adminRejectPath("payment", 0); ok {
		t.Fatal("invalid reject ID should be refused")
	}
	if got, ok := adminRefundRejectPath(5); !ok || got != "/v1/admin/refunds/5/reject" {
		t.Fatalf("adminRefundRejectPath(5) = %q, %t", got, ok)
	}
}

func TestPlanCardRendersLegacyPricingDescriptionAndDiscounts(t *testing.T) {
	card := formatPlanDetails(plan{
		Name: "Unlimited", Description: " مناسب برای استفاده روزمره ", BasePrice: 250000,
		BaseIP: 1, MaxIP: 4, PriceExtraIP: 30000,
		DiscountTiers: []discountTier{{Months: 6, BasisPoints: 500}, {Months: 12, BasisPoints: 1250}},
	}, "paid")
	for _, want := range []string{"Unlimited", "250,000 تومان/ماه", "هر IP اضافه: 30,000 تومان/ماه", "مناسب برای استفاده روزمره", "از 6 ماه: 5%", "از 12 ماه: 12.5%"} {
		if !strings.Contains(card, want) {
			t.Errorf("plan details missing %q: %s", want, card)
		}
	}
}

func TestTopupInstructionsIncludeCardOwnerAndMinimum(t *testing.T) {
	text := topupInstructionsText(paymentInstructions{
		CardNumber: "1234-5678", CardOwner: "Account Owner", Instructions: "رسید را ارسال کنید", MinTopupToman: 50000,
	})
	for _, want := range []string{"1234-5678", "Account Owner", "رسید را ارسال کنید", "50,000 تومان"} {
		if !strings.Contains(text, want) {
			t.Errorf("topup instructions missing %q: %s", want, text)
		}
	}
}

func TestPaymentDestinationMustBeCompleteBeforeDirectInvoice(t *testing.T) {
	if validPaymentDestination(paymentInstructions{CardNumber: "123", CardOwner: "Owner"}) != true {
		t.Fatal("configured card and owner should permit direct invoice")
	}
	for _, instructions := range []paymentInstructions{
		{},
		{CardNumber: "123"},
		{CardOwner: "Owner"},
		{CardNumber: "  ", CardOwner: "Owner"},
	} {
		if validPaymentDestination(instructions) {
			t.Errorf("incomplete payment destination accepted: %#v", instructions)
		}
	}
}

func TestAmbiguousPurchaseRetryKeepsSelectedPaymentMethod(t *testing.T) {
	features := runtimeConfig{Features: map[string]bool{"wallet_enabled": true, "direct_payments_enabled": true}}
	for _, method := range []string{"wallet", "direct"} {
		buttons := purchaseMethodButtons(features, method)
		if len(buttons) != 1 || !strings.Contains(buttons[0].Data, "confirm-purchase|"+method) {
			t.Errorf("retry buttons for %q = %#v; want only same-method retry", method, buttons)
		}
		other := "wallet"
		if method == "wallet" {
			other = "direct"
		}
		if !purchaseRetryMethodAllowed(method, method) || purchaseRetryMethodAllowed(other, method) {
			t.Errorf("retry guard accepted method switch after %q outcome", method)
		}
	}
	if len(purchaseMethodButtons(features, "")) != 2 {
		t.Fatal("fresh quote should allow both enabled methods")
	}
}

func TestTopupMinimumValidation(t *testing.T) {
	for _, tc := range []struct {
		amount, minimum int64
		wantError       bool
	}{
		{amount: 1, minimum: 0},
		{amount: 50000, minimum: 50000},
		{amount: 49999, minimum: 50000, wantError: true},
		{amount: 0, minimum: 0, wantError: true},
		{amount: -1, minimum: 0, wantError: true},
	} {
		err := validateTopupAmount(tc.amount, tc.minimum)
		if (err != nil) != tc.wantError {
			t.Errorf("validateTopupAmount(%d, %d) error=%v, wantError=%t", tc.amount, tc.minimum, err, tc.wantError)
		}
	}
}

func TestAdminCallbackRouteClassificationCoversPrivateAdminScreens(t *testing.T) {
	for _, action := range []string{"admin", "pending", "pendingtopups", "pendingpage", "resellers", "resellerpage", "approve", "approvetopup", "review-payment", "review-topup", "reject-payment", "reject-topup", "confirm-review-reject", "review-refund", "reject-refund", "confirm-refund-reject", "work-items", "refunds", "config", "cfg", "cfgset", "planedit", "planfield", "plannewkind", "plandraft", "planinbound", "feature"} {
		if !adminCallbackAction(action) {
			t.Errorf("admin route %q is not subject to private-chat gate", action)
		}
	}
	for _, action := range []string{"home", "wallet", "services", "support", "select", "confirm-purchase"} {
		if adminCallbackAction(action) {
			t.Errorf("customer route %q should not be classified as admin-only", action)
		}
	}
	for _, step := range []string{"cfgvalue", "cfgtextkey", "planvalue", "paneltoken", "plan-draft-name", "plan-draft-field"} {
		if !adminFlowStep(step) {
			t.Errorf("admin text step %q is not subject to private-chat gate", step)
		}
	}
	for _, step := range []string{"months", "gb", "name", "topup", "cancelid"} {
		if adminFlowStep(step) {
			t.Errorf("customer input step %q should not be classified as admin-only", step)
		}
	}
}
