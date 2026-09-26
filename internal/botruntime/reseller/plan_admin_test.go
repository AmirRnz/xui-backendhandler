package reseller

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"example.com/xui-commerce/backend/internal/botruntime/backend"
	"gopkg.in/telebot.v3"
)

func TestPlanDraftValidationRequiresTermsAndActiveInboundSelection(t *testing.T) {
	p := defaultPlanDraft("Retail plan")
	p.Kind = "paid"
	if err := validatePlanDraft(*p); err == nil || !strings.Contains(err.Error(), "inbound") {
		t.Fatalf("draft without inbound should fail clearly, got %v", err)
	}
	p.InboundIDs = []int{2, 5}
	if err := validatePlanDraft(*p); err == nil || !strings.Contains(err.Error(), "قیمت پایه") {
		t.Fatalf("paid plan without price should fail, got %v", err)
	}
	p.BasePrice = 1000
	if err := validatePlanDraft(*p); err != nil {
		t.Fatalf("valid unlimited paid plan rejected: %v", err)
	}
	p.IsLimited = true
	if err := validatePlanDraft(*p); err == nil {
		t.Fatal("limited plan without price per GB and minimum volume should fail")
	}
	p.PriceGB, p.MinGB = 100, 1
	if err := validatePlanDraft(*p); err != nil {
		t.Fatalf("valid limited paid plan rejected: %v", err)
	}
	p.InboundIDs = []int{2, 2}
	if err := validatePlanDraft(*p); err == nil || !strings.Contains(err.Error(), "تکراری") {
		t.Fatalf("duplicate inbound should fail, got %v", err)
	}

	test := defaultPlanDraft("Trial")
	test.Kind = "test"
	test.InboundIDs = []int{3}
	if err := validatePlanDraft(*test); err != nil {
		t.Fatalf("valid test plan rejected: %v", err)
	}
	test.ExpireSeconds = maxResellerPlanLifetimeSeconds + 1
	if err := validatePlanDraft(*test); err == nil {
		t.Fatal("test plan longer than ten years should fail")
	}
}

func TestResellerFailureHintSanitizesBackendErrors(t *testing.T) {
	secret := "database-password-must-not-reach-telegram"
	internal := &backend.APIError{Status: http.StatusInternalServerError, Code: "internal_error", Message: secret}
	if hint := resellerFailureHint(internal); strings.Contains(hint, secret) {
		t.Fatalf("internal failure leaked backend message: %q", hint)
	}
	invalid := &backend.APIError{Status: http.StatusBadRequest, Code: "invalid_request", Message: " plan has no active inbound \n"}
	if hint := resellerFailureHint(invalid); !strings.Contains(hint, "plan has no active inbound") || strings.Contains(hint, "\n") {
		t.Fatalf("validation hint should be useful and single-line: %q", hint)
	}
}

func TestEnabledPlanRequiresSelectedActiveInbounds(t *testing.T) {
	inbounds := []panelInbound{{ID: 1, Enable: false}, {ID: 2, Enable: true}}
	p := defaultPlanDraft("Plan")
	p.Enabled = true
	p.InboundIDs = []int{2}
	if err := validatePlanInboundSelection(*p, inbounds); err != nil {
		t.Fatalf("active inbound was rejected: %v", err)
	}
	p.InboundIDs = []int{1}
	if err := validatePlanInboundSelection(*p, inbounds); err == nil {
		t.Fatal("enabled plan accepted an inactive inbound")
	}
	p.Enabled = false
	if err := validatePlanInboundSelection(*p, inbounds); err != nil {
		t.Fatalf("disabled plan should be editable while its old inbound is inactive: %v", err)
	}
	p.InboundIDs = nil
	p.Enabled = true
	if err := validatePlanInboundSelection(*p, inbounds); err == nil {
		t.Fatal("enabled plan without any inbound was accepted")
	}
}

func TestPlanDraftParsesDiscountsAccessAndLimits(t *testing.T) {
	tiers, err := parsePlanDiscounts("3:10,6:12.5")
	if err != nil || len(tiers) != 2 || tiers[0].Months != 3 || tiers[0].BasisPoints != 1000 || tiers[1].BasisPoints != 1250 {
		t.Fatalf("discounts parsed as %+v, %v", tiers, err)
	}
	if _, err := parsePlanDiscounts("3:10,3:15"); err == nil {
		t.Fatal("duplicate discount months should fail")
	}
	ids, err := parsePlanAccess("42,41")
	if err != nil || len(ids) != 2 || ids[0] != 41 || ids[1] != 42 {
		t.Fatalf("allowlist parsed as %v, %v", ids, err)
	}
	if _, err := parsePlanAccess("42,42"); err == nil {
		t.Fatal("duplicate access IDs should fail")
	}
	p := defaultPlanDraft("Draft")
	if err := setPlanDraftValue(p, "ip_limits", "2-5"); err != nil || p.BaseIP != 2 || p.MaxIP != 5 {
		t.Fatalf("IP limits were not parsed: base=%d max=%d err=%v", p.BaseIP, p.MaxIP, err)
	}
	if err := setPlanDraftValue(p, "duration_hours", "0.5"); err != nil || p.ExpireSeconds != 1800 {
		t.Fatalf("fractional test duration was not parsed: seconds=%d err=%v", p.ExpireSeconds, err)
	}
	if err := setPlanDraftValue(p, "access", "-"); err != nil || !p.IsGlobal || len(p.AllowedTelegramIDs) != 0 {
		t.Fatalf("global access marker failed: global=%t ids=%v err=%v", p.IsGlobal, p.AllowedTelegramIDs, err)
	}
}

func TestExistingPlanEditorAcceptsItsDisplayedGBField(t *testing.T) {
	p := defaultPlanDraft("Existing")
	if err := setPlanField(p, "max_data_gb", "1.5"); err != nil {
		t.Fatalf("displayed GB field rejected: %v", err)
	}
	want := int64(1.5 * float64(uint64(1)<<30))
	if p.MaxBytes != want {
		t.Fatalf("GB value stored as %d bytes, want %d", p.MaxBytes, want)
	}
	if got := planFieldValue(*p, "max_data_gb"); got != "1.50 GB" {
		t.Fatalf("current GB value = %q, want %q", got, "1.50 GB")
	}
	for _, value := range []string{"-1", "NaN", "Inf", "999999999999999999999999"} {
		if err := setPlanField(p, "max_data_gb", value); err == nil {
			t.Errorf("invalid GB value %q was accepted", value)
		}
	}
}

func TestExistingPlanEditorUsesReadablePromptsAndValues(t *testing.T) {
	p := defaultPlanDraft("Paid")
	p.DiscountTiers = []discountTier{{Months: 3, BasisPoints: 1250}}
	p.AllowedTelegramIDs = []int64{41, 42}
	if got := planFieldValue(*p, "discount_tiers"); got != "3 ماه: 12.50%" {
		t.Fatalf("discount value = %q", got)
	}
	if got := planFieldValue(*p, "allowed_telegram_ids"); got != "41, 42" {
		t.Fatalf("access value = %q", got)
	}
	if prompt := planFieldPrompt("allowed_telegram_ids"); !strings.Contains(prompt, "شناسه") {
		t.Fatalf("access field has no useful prompt: %q", prompt)
	}
}

func TestExistingPlanEditorValidatesLimitsAndClearsOptionalText(t *testing.T) {
	p := defaultPlanDraft("Paid")
	for _, input := range []struct {
		field string
		value string
	}{
		{field: "base_ip_limit", value: "10001"},
		{field: "max_per_day", value: "10001"},
		{field: "expire_seconds", value: "315360001"},
	} {
		if err := setPlanField(p, input.field, input.value); err == nil {
			t.Errorf("%s accepted out-of-range value %q", input.field, input.value)
		}
	}
	p.Description = "old"
	p.Flow = "xtls-rprx-vision"
	p.UsageDescription = "old instructions"
	for _, field := range []string{"description", "flow", "usage_description"} {
		if err := setPlanField(p, field, "-"); err != nil {
			t.Fatalf("clear %s: %v", field, err)
		}
	}
	if p.Description != "" || p.Flow != "" || p.UsageDescription != "" {
		t.Fatalf("optional text fields were not cleared: %+v", p)
	}
}

func TestResellerPlanDraftPostsAndConfirmsBackendReadback(t *testing.T) {
	var posted plan
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-secret" || r.Header.Get("X-Actor-Telegram-ID") != "96937669" {
			t.Errorf("plan create request missing scoped credentials: auth=%q actor=%q", r.Header.Get("Authorization"), r.Header.Get("X-Actor-Telegram-ID"))
		}
		switch r.Method + " " + r.URL.Path {
		case "GET /v1/admin/panels/inbounds":
			_, _ = w.Write([]byte(`{"inbounds":[{"id":12,"remark":"Main","protocol":"vless","port":443,"enable":true}]}`))
		case "POST /v1/admin/config/plans":
			if err := json.NewDecoder(r.Body).Decode(&posted); err != nil {
				t.Errorf("decode plan create payload: %v", err)
				http.Error(w, "invalid body", http.StatusBadRequest)
				return
			}
			saved := posted
			saved.ID = 92
			_ = json.NewEncoder(w).Encode(map[string]any{"id": saved.ID, "config": adminConfig{Plans: []plan{saved}}})
		default:
			t.Errorf("unexpected plan create request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	api, err := backend.New(server.URL, "test-secret", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	app := &botApp{api: api, flows: map[int64]conversation{}}
	draft := defaultPlanDraft("Reseller plan")
	draft.Kind = "paid"
	draft.BasePrice = 25000
	draft.InboundIDs = []int{12}
	state := conversation{PlanDraft: draft}
	app.setFlow(adminTelegramID, state)
	ctx := &adminPagingContext{sender: &telebot.User{ID: adminTelegramID}, chat: &telebot.Chat{ID: adminTelegramID, Type: telebot.ChatPrivate}}
	if err := app.savePlanDraft(ctx, actor{TelegramID: adminTelegramID, Role: "admin"}, state); err != nil {
		t.Fatalf("save reseller plan: %v", err)
	}
	if posted.Name != draft.Name || posted.Kind != "paid" || posted.BasePrice != 25000 || !reflect.DeepEqual(posted.InboundIDs, []int{12}) || !posted.IsGlobal {
		t.Fatalf("plan payload mismatch: %+v", posted)
	}
	if text, ok := ctx.shown.(string); !ok || !strings.Contains(text, "طرح شماره 92 ذخیره و دوباره از backend خوانده شد") {
		t.Fatalf("success screen did not confirm readback: %#v", ctx.shown)
	}
	if _, exists := app.flows[adminTelegramID]; exists {
		t.Fatal("successful plan save did not clear its draft flow")
	}
}

func TestResellerPlanSummariesStayWithinTelegramLimit(t *testing.T) {
	draft := defaultPlanDraft("Plan")
	draft.Kind = "paid"
	draft.Description = strings.Repeat("説明", 1000)
	draft.UsageDescription = strings.Repeat("راهنما", 1000)
	draft.DiscountTiers = make([]discountTier, 36)
	for i := range draft.DiscountTiers {
		draft.DiscountTiers[i] = discountTier{Months: i + 1, BasisPoints: 100}
	}
	draft.AllowedTelegramIDs = make([]int64, 256)
	for i := range draft.AllowedTelegramIDs {
		draft.AllowedTelegramIDs[i] = int64(i + 1000000000)
	}
	for i := 1; i <= 64; i++ {
		draft.InboundIDs = append(draft.InboundIDs, i)
	}
	if got := len([]rune(formatAdminPlan(*draft))); got > 4096 {
		t.Fatalf("saved-plan summary exceeds Telegram limit: %d runes", got)
	}
	app := &botApp{flows: map[int64]conversation{}}
	state := conversation{PlanDraft: draft}
	app.setFlow(adminTelegramID, state)
	ctx := &adminPagingContext{sender: &telebot.User{ID: adminTelegramID}, chat: &telebot.Chat{ID: adminTelegramID, Type: telebot.ChatPrivate}}
	if err := app.showPlanDraft(ctx, actor{TelegramID: adminTelegramID, Role: "admin"}, draft); err != nil {
		t.Fatalf("render large reseller plan draft: %v", err)
	}
	text, ok := ctx.shown.(string)
	if !ok || len([]rune(text)) > 4096 {
		t.Fatalf("plan draft summary has invalid Telegram length: %d", len([]rune(strings.TrimSpace(text))))
	}
}

func TestPlanInboundSelectionAndPagingStaySortedAndBounded(t *testing.T) {
	if got, want := togglePlanInbound([]int{4, 9}, 6), []int{4, 6, 9}; !reflect.DeepEqual(got, want) {
		t.Fatalf("adding inbound = %v, want %v", got, want)
	}
	if got, want := togglePlanInbound([]int{4, 6, 9}, 6), []int{4, 9}; !reflect.DeepEqual(got, want) {
		t.Fatalf("toggling selected inbound = %v, want %v", got, want)
	}
	if got := clampPlanPage(-4, 30); got != 0 {
		t.Fatalf("negative page clamped to %d, want 0", got)
	}
	if got := clampPlanPage(40, 30); got != 2 {
		t.Fatalf("high page clamped to %d, want 2", got)
	}
}

func TestPlanAdminButtonLabelsFitTelegramLimits(t *testing.T) {
	p := plan{Name: strings.Repeat("套餐", 60), Kind: "paid", Enabled: true}
	if got := len([]rune(planPickerLabel(p))); got > 64 {
		t.Fatalf("plan picker label has %d runes", got)
	}
	inbound := panelInbound{ID: 123456789, Remark: strings.Repeat("入口", 40), Protocol: strings.Repeat("vless", 30), Port: 443}
	if got := len([]rune(planInboundOptionLabel("✅", inbound))); got > 64 {
		t.Fatalf("inbound option label has %d runes", got)
	}
}

func TestPlanManagementCallbacksAreAdminOnly(t *testing.T) {
	for _, action := range []string{"plannewkind", "plandraft", "planinbound"} {
		if !adminCallbackAction(action) {
			t.Errorf("%q must be treated as an admin callback", action)
		}
	}
}
