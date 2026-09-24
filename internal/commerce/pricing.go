package commerce

import (
	"errors"
	"fmt"
	"math"
	"sort"
)

const GiB int64 = 1 << 30

type DiscountTier struct {
	Months      int   `json:"months"`
	BasisPoints int64 `json:"basis_points"`
}

type Plan struct {
	ID                      int64          `json:"id"`
	Name                    string         `json:"name"`
	Description             string         `json:"description"`
	Kind                    string         `json:"kind"`
	Enabled                 bool           `json:"enabled"`
	IsLimited               bool           `json:"is_limited"`
	BasePriceToman          int64          `json:"base_price_toman"`
	PricePerExtraIPToman    int64          `json:"price_per_extra_ip_toman"`
	PricePerGBToman         int64          `json:"price_per_gb_toman"`
	PricePerExtraMonthToman int64          `json:"price_per_extra_month_toman"`
	BaseIPLimit             int            `json:"base_ip_limit"`
	MaxIPLimit              int            `json:"max_ip_limit"`
	MinDataGB               int            `json:"min_data_gb"`
	MaxDataBytes            int64          `json:"max_data_bytes"`
	ExpireSeconds           int64          `json:"expire_seconds"`
	IPLimit                 int            `json:"ip_limit"`
	MaxPerDay               int            `json:"max_per_day"`
	Flow                    string         `json:"flow"`
	InboundIDs              []int          `json:"inbound_ids"`
	DiscountTiers           []DiscountTier `json:"discount_tiers"`
	UsageDescription        string         `json:"usage_description"`
	PanelID                 string         `json:"panel_id"`
}

type Quote struct {
	ID                   int64  `json:"id"`
	Key                  string `json:"quote_key"`
	PlanID               int64  `json:"plan_id"`
	PlanName             string `json:"plan_name"`
	Months               int    `json:"months"`
	DurationDays         int    `json:"duration_days"`
	IPLimit              int    `json:"ip_limit"`
	DataGB               int    `json:"data_gb"`
	BasePriceToman       int64  `json:"base_price_toman"`
	ExtraIPPriceToman    int64  `json:"extra_ip_price_toman"`
	ExtraMonthPriceToman int64  `json:"extra_month_price_toman"`
	TrafficPriceToman    int64  `json:"traffic_price_toman"`
	DiscountToman        int64  `json:"discount_toman"`
	FinalPriceToman      int64  `json:"final_price_toman"`
	Currency             string `json:"currency"`
}

var ErrInvalidQuote = errors.New("invalid quote parameters")

// CalculateQuote keeps the legacy 30-day month and half-up basis-point rounding.
// Every multiplication/addition is checked because amounts are signed int64 Toman.
func CalculateQuote(p Plan, months, ipLimit, dataGB int) (Quote, error) {
	if p.ID <= 0 || months < 1 || months > 120 || ipLimit < 0 || dataGB < 0 || p.BaseIPLimit < 0 || p.MaxIPLimit < p.BaseIPLimit {
		return Quote{}, ErrInvalidQuote
	}
	if ipLimit < p.BaseIPLimit {
		ipLimit = p.BaseIPLimit
	}
	if ipLimit > p.MaxIPLimit {
		return Quote{}, fmt.Errorf("ip limit exceeds plan maximum")
	}
	if p.IsLimited && dataGB < p.MinDataGB {
		return Quote{}, fmt.Errorf("data amount is below the plan minimum")
	}
	if !p.IsLimited && dataGB != 0 {
		return Quote{}, fmt.Errorf("data amount is not valid for an unlimited plan")
	}
	var base, traffic, extraMonth int64
	var err error
	if p.IsLimited {
		traffic, err = checkedMul(int64(dataGB), p.PricePerGBToman)
		if err != nil {
			return Quote{}, err
		}
		if months > 1 {
			extraMonth, err = checkedMul(int64(months-1), p.PricePerExtraMonthToman)
			if err != nil {
				return Quote{}, err
			}
		}
		base, err = checkedAdd(traffic, extraMonth)
	} else {
		base, err = checkedMul(p.BasePriceToman, int64(months))
	}
	if err != nil {
		return Quote{}, err
	}
	extraIP, err := checkedMul(int64(ipLimit-p.BaseIPLimit), p.PricePerExtraIPToman)
	if err != nil {
		return Quote{}, err
	}
	extraIP, err = checkedMul(extraIP, int64(months))
	if err != nil {
		return Quote{}, err
	}
	subtotal, err := checkedAdd(base, extraIP)
	if err != nil {
		return Quote{}, err
	}
	bps := bestDiscount(p.DiscountTiers, months)
	if bps < 0 || bps > 10000 {
		return Quote{}, fmt.Errorf("discount basis points must be between 0 and 10000")
	}
	product, err := checkedMul(subtotal, bps)
	if err != nil {
		return Quote{}, err
	}
	product, err = checkedAdd(product, 5000)
	if err != nil {
		return Quote{}, err
	}
	discount := product / 10000
	if discount > subtotal {
		discount = subtotal
	}
	return Quote{PlanID: p.ID, PlanName: p.Name, Months: months, DurationDays: months * 30, IPLimit: ipLimit, DataGB: dataGB,
		BasePriceToman: base, ExtraIPPriceToman: extraIP, ExtraMonthPriceToman: extraMonth, TrafficPriceToman: traffic,
		DiscountToman: discount, FinalPriceToman: subtotal - discount, Currency: "تومان"}, nil
}

func bestDiscount(tiers []DiscountTier, months int) int64 {
	copyOf := append([]DiscountTier(nil), tiers...)
	sort.Slice(copyOf, func(i, j int) bool { return copyOf[i].Months < copyOf[j].Months })
	var best int64
	for _, t := range copyOf {
		if months >= t.Months && t.BasisPoints > best {
			best = t.BasisPoints
		}
	}
	return best
}

func checkedMul(a, b int64) (int64, error) {
	if a == 0 || b == 0 {
		return 0, nil
	}
	if a == math.MinInt64 && b == -1 || b == math.MinInt64 && a == -1 {
		return 0, fmt.Errorf("integer money overflow")
	}
	r := a * b
	if r/b != a {
		return 0, fmt.Errorf("integer money overflow")
	}
	return r, nil
}
func checkedAdd(a, b int64) (int64, error) {
	if b > 0 && a > math.MaxInt64-b || b < 0 && a < math.MinInt64-b {
		return 0, fmt.Errorf("integer money overflow")
	}
	return a + b, nil
}
