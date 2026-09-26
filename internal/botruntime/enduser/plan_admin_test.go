package enduser

import "testing"

func TestPlanDraftValidationRequiresActiveInboundAndUsableTerms(t *testing.T) {
	base := defaultPlanDraft("Retail", "paid")
	base.BasePrice = 1000
	if err := validatePlanDraft(*base); err == nil {
		t.Fatal("draft without an inbound should not be saved")
	}
	base.InboundIDs = []int64{1}
	if err := validatePlanDraft(*base); err != nil {
		t.Fatalf("valid unlimited paid plan rejected: %v", err)
	}
	base.IsLimited = true
	if err := validatePlanDraft(*base); err == nil {
		t.Fatal("limited plan without per-GB price and minimum volume should not be saved")
	}
	base.PricePerGB = 100
	base.MinGB = 1
	if err := validatePlanDraft(*base); err != nil {
		t.Fatalf("valid limited paid plan rejected: %v", err)
	}
	test := defaultPlanDraft("Trial", "test")
	test.InboundIDs = []int64{1}
	if err := validatePlanDraft(*test); err != nil {
		t.Fatalf("valid test plan rejected: %v", err)
	}
	test.ExpireSeconds = 0
	if err := validatePlanDraft(*test); err == nil {
		t.Fatal("test plan without positive duration should not be saved")
	}
	test.ExpireSeconds = maxRetailPlanLifetimeSeconds + 1
	if err := validatePlanDraft(*test); err == nil {
		t.Fatal("test plan beyond the supported lifetime should not be saved")
	}
	base.Description = string(make([]byte, 2049))
	if err := validatePlanDraft(*base); err == nil {
		t.Fatal("oversized plan description should not be saved")
	}
	base.Description = ""
	base.MaxIP = 10001
	if err := validatePlanDraft(*base); err == nil {
		t.Fatal("plan with excessive device limit should not be saved")
	}
}

func TestPlanDraftParsersBoundAccessAndDiscounts(t *testing.T) {
	ids, err := parseTelegramIDList("41, 42,41")
	if err == nil || ids != nil {
		t.Fatalf("duplicate Telegram IDs must be rejected, got %v, %v", ids, err)
	}
	ids, err = parseTelegramIDList("41,42")
	if err != nil || len(ids) != 2 || ids[0] != 41 || ids[1] != 42 {
		t.Fatalf("valid allowlist failed to parse: %v, %v", ids, err)
	}
	global, err := parseTelegramIDList("-")
	if err != nil || global == nil || len(global) != 0 {
		t.Fatalf("global access marker failed to parse: %v, %v", global, err)
	}
	tiers, err := parseDiscountTierText("3:10,6:12.5")
	if err != nil || len(tiers) != 2 || tiers[0].Months != 3 || tiers[0].BasisPoints != 1000 || tiers[1].Months != 6 || tiers[1].BasisPoints != 1250 {
		t.Fatalf("discount tier text was not converted to integer basis points: %+v, %v", tiers, err)
	}
	if _, err = parseDiscountTierText("3:10,3:15"); err == nil {
		t.Fatal("duplicate discount durations must be rejected")
	}
}
