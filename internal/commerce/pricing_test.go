package commerce

import "testing"

func TestCalculateQuoteLegacyRoundingAndLimitedPlan(t *testing.T) {
	p := Plan{ID: 1, Name: "limited", Kind: "paid", IsLimited: true, BaseIPLimit: 1, MaxIPLimit: 4, MinDataGB: 10,
		PricePerGBToman: 1000, PricePerExtraMonthToman: 500, PricePerExtraIPToman: 2000,
		DiscountTiers: []DiscountTier{{Months: 3, BasisPoints: 1250}}}
	q, err := CalculateQuote(p, 3, 2, 11)
	if err != nil {
		t.Fatal(err)
	}
	// (11,000 traffic + 1,000 time + 6,000 extra IP) * 12.5% half-up.
	if q.FinalPriceToman != 15750 || q.DiscountToman != 2250 || q.DurationDays != 90 {
		t.Fatalf("unexpected quote: %+v", q)
	}
}

func TestCalculateQuoteRejectsBoundsAndOverflow(t *testing.T) {
	p := Plan{ID: 1, IsLimited: false, BaseIPLimit: 1, MaxIPLimit: 2, BasePriceToman: 1 << 62}
	if _, err := CalculateQuote(p, 3, 1, 0); err == nil {
		t.Fatal("expected overflow")
	}
	if _, err := CalculateQuote(p, 1, 3, 0); err == nil {
		t.Fatal("expected plan limit rejection")
	}
}
