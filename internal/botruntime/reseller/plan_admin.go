package reseller

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/telebot.v3"
)

const resellerPlanInboundPageSize = 12
const maxResellerPlanLifetimeSeconds = int64(10 * 365 * 24 * 60 * 60)

type panelInbound struct {
	ID       int    `json:"id"`
	Remark   string `json:"remark"`
	Tag      string `json:"tag"`
	Protocol string `json:"protocol"`
	Port     int    `json:"port"`
	Enable   bool   `json:"enable"`
}

func defaultPlanDraft(name string) *plan {
	return &plan{
		Name: name, Enabled: true, BaseIP: 1, MaxIP: 1, TestIPLimit: 1,
		ExpireSeconds: int64(24 * time.Hour / time.Second), InboundIDs: []int{},
		DiscountTiers: []discountTier{}, AllowedTelegramIDs: []int64{}, IsGlobal: true,
	}
}

func (a *botApp) startPlanDraft(c telebot.Context, act actor) error {
	if !isPrivateChat(c.Chat()) {
		return a.show(c, "ساخت طرح فقط در گفت‌وگوی خصوصی مدیر در دسترس است.", markup([]telebot.Btn{btn("مدیریت", "admin")}))
	}
	inbounds, err := a.planInbounds(c, act.TelegramID)
	if err != nil {
		return a.sendFailure(c, err)
	}
	if activeInboundIDs(inbounds) == 0 {
		return a.show(c, "برای ساخت طرح ابتدا حداقل یک inbound فعال در پنل 3x-ui ایجاد کنید.", markup([]telebot.Btn{btn("اتصال پنل", "cfg|panel")}, []telebot.Btn{btn("طرح‌ها", "cfg|plans")}))
	}
	st := conversation{Step: "plan-draft-name", PlanDraft: defaultPlanDraft(""), Expires: time.Now().Add(20 * time.Minute)}
	a.setFlow(act.TelegramID, st)
	return a.show(c, "نام طرح جدید را بفرستید.", markup([]telebot.Btn{btn("لغو", "cfg|plans")}))
}

func (a *botApp) planInbounds(c telebot.Context, actorID int64) ([]panelInbound, error) {
	var response struct {
		Inbounds []panelInbound `json:"inbounds"`
	}
	if err := a.call(c, "GET", "/v1/admin/panels/inbounds", actorID, nil, &response); err != nil {
		return nil, err
	}
	return response.Inbounds, nil
}

func activeInboundIDs(inbounds []panelInbound) int {
	count := 0
	for _, inbound := range inbounds {
		if inbound.Enable && inbound.ID > 0 {
			count++
		}
	}
	return count
}

func validatePlanInboundSelection(p plan, inbounds []panelInbound) error {
	if p.Enabled && len(p.InboundIDs) == 0 {
		return fmt.Errorf("برای طرح فعال حداقل یک inbound لازم است")
	}
	if !p.Enabled {
		return nil
	}
	active := make(map[int]bool, len(inbounds))
	for _, inbound := range inbounds {
		if inbound.Enable && inbound.ID > 0 {
			active[inbound.ID] = true
		}
	}
	for _, id := range p.InboundIDs {
		if !active[id] {
			return fmt.Errorf("inbound شماره %d فعال نیست", id)
		}
	}
	return nil
}

func (a *botApp) showPlanDraftKind(c telebot.Context, act actor, p *plan) error {
	if p == nil || strings.TrimSpace(p.Name) == "" {
		return a.configPlans(c, act)
	}
	return a.show(c, fmt.Sprintf("نام طرح: %s\n\nنوع طرح را انتخاب کنید:", p.Name), markup(
		[]telebot.Btn{btn("💼 طرح پولی", "plannewkind|paid"), btn("🧪 طرح تست", "plannewkind|test")},
		[]telebot.Btn{btn("لغو", "plandraft|cancel")},
	))
}

func (a *botApp) choosePlanDraftKind(c telebot.Context, act actor, kind string) error {
	if kind != "paid" && kind != "test" {
		return a.show(c, "نوع طرح نامعتبر است.", markup([]telebot.Btn{btn("طرح‌ها", "cfg|plans")}))
	}
	a.mu.Lock()
	st, ok := a.flows[act.TelegramID]
	a.mu.Unlock()
	if !ok || st.PlanDraft == nil || strings.TrimSpace(st.PlanDraft.Name) == "" || time.Now().After(st.Expires) {
		a.clearFlow(act.TelegramID)
		return a.show(c, "پیش‌نویس طرح منقضی شده است. از منوی طرح‌ها دوباره شروع کنید.", markup([]telebot.Btn{btn("طرح‌ها", "cfg|plans")}))
	}
	p := st.PlanDraft
	p.Kind = kind
	if kind == "test" {
		p.IsLimited = false
		p.BasePrice, p.PriceExtraIP, p.PriceGB, p.PriceExtraMonth = 0, 0, 0, 0
		p.BaseIP, p.MaxIP = 0, 0
	} else {
		p.MaxPerDay = 0
	}
	st.Step = ""
	st.Expires = time.Now().Add(20 * time.Minute)
	a.setFlow(act.TelegramID, st)
	return a.showPlanDraft(c, act, p)
}

func (a *botApp) showPlanDraft(c telebot.Context, act actor, p *plan) error {
	if p == nil || (p.Kind != "paid" && p.Kind != "test") {
		return a.configPlans(c, act)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "پیش‌نویس طرح %s\n\nنام: %s\nتوضیحات: %s\nراهنما: %s\nاینباندهای فعال: %s\nFlow: %s\nدسترسی: %s\n", planKindLabel(p.Kind), planSummaryText(p.Name, 120), planSummaryText(p.Description, 400), planSummaryText(p.UsageDescription, 400), planSummaryText(planInboundNames(p.InboundIDs), 300), planSummaryText(p.Flow, 120), planSummaryText(planAccessLabel(p.AllowedTelegramIDs), 400))
	if p.Kind == "test" {
		fmt.Fprintf(&b, "مدت: %s\nسقف حجم: %s\nIP نمایشی: %d\nسهمیه روزانه: %d\n", humanDuration(p.ExpireSeconds), formatPlanData(p.MaxBytes), p.TestIPLimit, p.MaxPerDay)
	} else if p.IsLimited {
		fmt.Fprintf(&b, "نوع: حجمی\nقیمت هر GB: %s تومان\nحداقل حجم: %d GB\nحداکثر حجم: %s\nقیمت ماه اضافه: %s تومان\n", formatToman(p.PriceGB), p.MinGB, formatPlanData(p.MaxBytes), formatToman(p.PriceExtraMonth))
		fmt.Fprintf(&b, "IP پایه/حداکثر: %d/%d\nقیمت IP اضافه: %s تومان\nتخفیف‌ها: %s\n", p.BaseIP, p.MaxIP, formatToman(p.PriceExtraIP), planSummaryText(planDiscountLabel(p.DiscountTiers), 400))
	} else {
		fmt.Fprintf(&b, "نوع: نامحدود\nقیمت پایه: %s تومان در ماه\nIP پایه/حداکثر: %d/%d\nقیمت IP اضافه: %s تومان\nتخفیف‌ها: %s\n", formatToman(p.BasePrice), p.BaseIP, p.MaxIP, formatToman(p.PriceExtraIP), planSummaryText(planDiscountLabel(p.DiscountTiers), 400))
	}
	rows := [][]telebot.Btn{
		{btn("📝 نام", "plandraft|field|name"), btn("📝 توضیحات", "plandraft|field|description"), btn("📖 راهنما", "plandraft|field|usage_description")},
		{btn("📡 انتخاب inboundها", "plandraft|inbounds"), btn("👥 دسترسی", "plandraft|field|access"), btn("⚡ Flow", "plandraft|flow")},
	}
	if p.Kind == "test" {
		rows = append(rows, []telebot.Btn{btn("⏱️ مدت ساعت", "plandraft|field|duration_hours"), btn("💾 سقف حجم GB", "plandraft|field|max_data_gb")})
		rows = append(rows, []telebot.Btn{btn("🌐 IP نمایشی", "plandraft|field|test_ip_limit"), btn("📊 سهمیه روزانه", "plandraft|field|max_per_day")})
	} else {
		limitLabel := "📊 نوع: نامحدود"
		if p.IsLimited {
			limitLabel = "📊 نوع: حجمی"
		}
		rows = append(rows, []telebot.Btn{btn(limitLabel, "plandraft|toggle-limited")})
		if p.IsLimited {
			rows = append(rows, []telebot.Btn{btn("💵 قیمت/GB", "plandraft|field|price_per_gb_toman"), btn("💾 حداقل GB", "plandraft|field|min_data_gb")})
			rows = append(rows, []telebot.Btn{btn("💾 حداکثر حجم GB", "plandraft|field|max_data_gb"), btn("⏱️ قیمت ماه اضافه", "plandraft|field|price_per_extra_month_toman")})
		} else {
			rows = append(rows, []telebot.Btn{btn("💵 قیمت پایه تومان", "plandraft|field|base_price_toman")})
		}
		rows = append(rows, []telebot.Btn{btn("🌐 IP پایه/حداکثر", "plandraft|field|ip_limits"), btn("💲 قیمت IP اضافه", "plandraft|field|price_per_extra_ip_toman")})
		rows = append(rows, []telebot.Btn{btn("🏷️ تخفیف‌ها", "plandraft|field|discount_tiers")})
	}
	rows = append(rows, []telebot.Btn{btn("💾 ذخیره طرح", "plandraft|save"), btn("❌ انصراف", "plandraft|cancel")})
	return a.show(c, strings.TrimSpace(b.String()), markup(rows...))
}

func (a *botApp) handlePlanDraftAction(c telebot.Context, act actor, args []string) error {
	if len(args) == 0 {
		return a.configPlans(c, act)
	}
	a.mu.Lock()
	st, ok := a.flows[act.TelegramID]
	a.mu.Unlock()
	if !ok || st.PlanDraft == nil || time.Now().After(st.Expires) {
		a.clearFlow(act.TelegramID)
		return a.show(c, "پیش‌نویس طرح منقضی شده است. از منوی طرح‌ها دوباره شروع کنید.", markup([]telebot.Btn{btn("طرح‌ها", "cfg|plans")}))
	}
	p := st.PlanDraft
	switch args[0] {
	case "cancel":
		a.clearFlow(act.TelegramID)
		return a.configPlans(c, act)
	case "field":
		if len(args) != 2 {
			return a.showPlanDraft(c, act, p)
		}
		return a.promptPlanDraftField(c, act, st, args[1])
	case "toggle-limited":
		if p.Kind != "paid" {
			return a.showPlanDraft(c, act, p)
		}
		p.IsLimited = !p.IsLimited
		st.Expires = time.Now().Add(20 * time.Minute)
		a.setFlow(act.TelegramID, st)
		return a.showPlanDraft(c, act, p)
	case "flow":
		m := markup(
			[]telebot.Btn{btn("⚡ xtls-rprx-vision", "plandraft|flow-set|xtls-rprx-vision")},
			[]telebot.Btn{btn("بدون Flow", "plandraft|flow-set|-")},
			[]telebot.Btn{btn("Flow سفارشی", "plandraft|field|flow_custom")},
			[]telebot.Btn{btn("بازگشت", "plandraft|back")},
		)
		return a.show(c, "Flow فعلی: "+planValueOrDash(p.Flow), m)
	case "flow-set":
		if len(args) != 2 {
			return a.showPlanDraft(c, act, p)
		}
		p.Flow = args[1]
		if p.Flow == "-" {
			p.Flow = ""
		}
		st.Step = ""
		st.Expires = time.Now().Add(20 * time.Minute)
		a.setFlow(act.TelegramID, st)
		return a.showPlanDraft(c, act, p)
	case "back":
		if p.ID > 0 {
			a.clearFlow(act.TelegramID)
			return a.planEditor(c, act, strconv.FormatInt(p.ID, 10))
		}
		return a.showPlanDraft(c, act, p)
	case "inbounds":
		return a.showPlanDraftInbounds(c, act, st)
	case "save":
		return a.savePlanDraft(c, act, st)
	default:
		return a.showPlanDraft(c, act, p)
	}
}

func (a *botApp) promptPlanDraftField(c telebot.Context, act actor, st conversation, field string) error {
	prompt := planDraftPrompt(field)
	if prompt == "" {
		return a.showPlanDraft(c, act, st.PlanDraft)
	}
	st.Step = "plan-draft-field"
	st.Vals = map[string]string{"field": field}
	return a.setFlowAndPrompt(c, act.TelegramID, st, prompt)
}

func planDraftPrompt(field string) string {
	return map[string]string{
		"name":                        "نام طرح را بفرستید.",
		"description":                 "توضیحات طرح را بفرستید؛ برای پاک‌کردن - بفرستید.",
		"usage_description":           "راهنمای اتصال را بفرستید؛ برای پاک‌کردن - بفرستید.",
		"base_price_toman":            "قیمت پایه ماهانه را به تومان بفرستید.",
		"price_per_extra_ip_toman":    "قیمت ماهانه هر IP اضافه را به تومان بفرستید.",
		"price_per_gb_toman":          "قیمت هر گیگابایت را به تومان بفرستید.",
		"price_per_extra_month_toman": "قیمت هر ماه اضافه را به تومان بفرستید.",
		"ip_limits":                   "IP پایه و حداکثر را بفرستید، مثل 1-3؛ برای نامحدود 0 بفرستید.",
		"min_data_gb":                 "حداقل حجم را به GB بفرستید.",
		"duration_hours":              "مدت تست را به ساعت بفرستید، مثل 24 یا 0.5.",
		"max_data_gb":                 "سقف حجم را به GB بفرستید؛ صفر یعنی نامحدود.",
		"test_ip_limit":               "تعداد IP نمایشی تست را بفرستید؛ صفر یعنی نامحدود.",
		"max_per_day":                 "سهمیه تست روزانه را بفرستید؛ صفر یعنی بدون سقف طرح.",
		"discount_tiers":              "تخفیف‌ها را با فرمت ماه:درصد وارد کنید؛ نمونه 3:10,6:20. برای حذف همه - بفرستید.",
		"access":                      "شناسه‌های تلگرام مجاز را با کاما جدا کنید؛ - یعنی همه resellerهای این deployment.",
		"flow_custom":                 "Flow سفارشی را بفرستید؛ برای پاک‌کردن - بفرستید.",
	}[field]
}

func (a *botApp) savePlanDraftField(c telebot.Context, act actor, st conversation, raw string) error {
	if st.PlanDraft == nil {
		return a.configPlans(c, act)
	}
	field := st.Vals["field"]
	if err := setPlanDraftValue(st.PlanDraft, field, raw); err != nil {
		st.Expires = time.Now().Add(20 * time.Minute)
		a.setFlow(act.TelegramID, st)
		return a.setFlowAndPrompt(c, act.TelegramID, st, "مقدار نامعتبر است: "+err.Error()+"\n"+planDraftPrompt(field))
	}
	st.Step, st.Vals = "", nil
	st.Expires = time.Now().Add(20 * time.Minute)
	a.setFlow(act.TelegramID, st)
	return a.showPlanDraft(c, act, st.PlanDraft)
}

func setPlanDraftValue(p *plan, field, raw string) error {
	raw = strings.TrimSpace(raw)
	if field == "description" || field == "usage_description" || field == "flow_custom" {
		if raw == "-" {
			raw = ""
		}
		if field == "description" && len([]byte(raw)) > 2048 || field == "usage_description" && len([]byte(raw)) > 4096 || field == "flow_custom" && len([]byte(raw)) > 120 {
			return fmt.Errorf("متن از حد مجاز بیشتر است")
		}
		switch field {
		case "description":
			p.Description = raw
		case "usage_description":
			p.UsageDescription = raw
		case "flow_custom":
			p.Flow = raw
		}
		return nil
	}
	if field == "ip_limits" {
		if raw == "0" || strings.EqualFold(raw, "unlimited") {
			p.BaseIP, p.MaxIP = 0, 0
			return nil
		}
		parts := strings.FieldsFunc(raw, func(r rune) bool { return r == '-' || r == ',' })
		if len(parts) != 2 {
			return fmt.Errorf("دو عدد با خط تیره بفرستید؛ نمونه 1-3")
		}
		base, err1 := strconv.Atoi(strings.TrimSpace(parts[0]))
		max, err2 := strconv.Atoi(strings.TrimSpace(parts[1]))
		if err1 != nil || err2 != nil || base <= 0 || max < base || max > 10000 {
			return fmt.Errorf("حد IP معتبر نیست")
		}
		p.BaseIP, p.MaxIP = base, max
		return nil
	}
	if field == "discount_tiers" {
		tiers, err := parsePlanDiscounts(raw)
		if err == nil {
			p.DiscountTiers = tiers
		}
		return err
	}
	if field == "access" {
		ids, err := parsePlanAccess(raw)
		if err == nil {
			p.AllowedTelegramIDs = ids
			p.IsGlobal = len(ids) == 0
		}
		return err
	}
	if field == "duration_hours" {
		hours, err := strconv.ParseFloat(raw, 64)
		seconds := hours * 3600
		if err != nil || math.IsNaN(hours) || math.IsInf(hours, 0) || hours <= 0 || seconds > float64(maxResellerPlanLifetimeSeconds) {
			return fmt.Errorf("مدت باید بیشتر از صفر و حداکثر ده سال باشد")
		}
		p.ExpireSeconds = int64(math.Round(seconds))
		return nil
	}
	if field == "max_data_gb" {
		gb, err := strconv.ParseFloat(raw, 64)
		bytes := gb * float64(uint64(1)<<30)
		if err != nil || math.IsNaN(gb) || math.IsInf(gb, 0) || gb < 0 || bytes >= float64(math.MaxInt64) {
			return fmt.Errorf("حجم باید صفر یا عددی مثبت و محدود باشد")
		}
		p.MaxBytes = int64(bytes)
		return nil
	}
	if field == "name" && (raw == "" || len([]byte(raw)) > 120) {
		return fmt.Errorf("نام باید بین ۱ تا ۱۲۰ بایت باشد")
	}
	return setPlanField(p, field, raw)
}

func parsePlanDiscounts(raw string) ([]discountTier, error) {
	if raw == "" || raw == "-" {
		return []discountTier{}, nil
	}
	parts := strings.Split(raw, ",")
	if len(parts) > 64 {
		return nil, fmt.Errorf("حداکثر ۶۴ تخفیف مجاز است")
	}
	tiers := make([]discountTier, 0, len(parts))
	seen := make(map[int]struct{}, len(parts))
	for _, part := range parts {
		pair := strings.Split(strings.TrimSpace(part), ":")
		if len(pair) != 2 {
			return nil, fmt.Errorf("فرمت هر تخفیف ماه:درصد است")
		}
		months, err1 := strconv.Atoi(strings.TrimSpace(pair[0]))
		percent, err2 := strconv.ParseFloat(strings.TrimSpace(strings.TrimSuffix(pair[1], "%")), 64)
		basisPoints := percent * 100
		if err1 != nil || err2 != nil || months < 1 || months > 36 || math.IsNaN(percent) || math.IsInf(percent, 0) || percent < 0 || percent > 100 || math.Abs(basisPoints-math.Round(basisPoints)) > 1e-7 {
			return nil, fmt.Errorf("ماه باید بین ۱ و ۳۶ و درصد بین ۰ و ۱۰۰ با دقت حداکثر ۰٫۰۱ باشد")
		}
		if _, ok := seen[months]; ok {
			return nil, fmt.Errorf("تعداد ماه تخفیف تکراری است")
		}
		seen[months] = struct{}{}
		tiers = append(tiers, discountTier{Months: months, BasisPoints: int64(math.Round(basisPoints))})
	}
	sort.Slice(tiers, func(i, j int) bool { return tiers[i].Months < tiers[j].Months })
	return tiers, nil
}

func parsePlanAccess(raw string) ([]int64, error) {
	if raw == "" || raw == "-" {
		return []int64{}, nil
	}
	parts := strings.Split(raw, ",")
	if len(parts) > 256 {
		return nil, fmt.Errorf("حداکثر ۲۵۶ شناسه مجاز است")
	}
	ids := make([]int64, 0, len(parts))
	seen := make(map[int64]struct{}, len(parts))
	for _, part := range parts {
		id, err := strconv.ParseInt(strings.TrimSpace(part), 10, 64)
		if err != nil || id <= 0 {
			return nil, fmt.Errorf("شناسه تلگرام باید عدد صحیح مثبت باشد")
		}
		if _, ok := seen[id]; ok {
			return nil, fmt.Errorf("شناسه تلگرام تکراری است")
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids, nil
}

func validatePlanDraft(p plan) error {
	if strings.TrimSpace(p.Name) == "" || len([]byte(p.Name)) > 120 || len([]byte(p.Description)) > 2048 || len([]byte(p.UsageDescription)) > 4096 || len([]byte(p.Flow)) > 120 {
		return fmt.Errorf("نام، توضیحات یا Flow طرح معتبر نیست")
	}
	if p.Kind != "paid" && p.Kind != "test" {
		return fmt.Errorf("نوع طرح را انتخاب کنید")
	}
	if len(p.InboundIDs) == 0 || len(p.InboundIDs) > 64 {
		return fmt.Errorf("حداقل یک و حداکثر ۶۴ inbound فعال انتخاب کنید")
	}
	seenInbound := make(map[int]struct{}, len(p.InboundIDs))
	for _, id := range p.InboundIDs {
		if id <= 0 {
			return fmt.Errorf("شناسه inbound نامعتبر است")
		}
		if _, exists := seenInbound[id]; exists {
			return fmt.Errorf("inbound تکراری انتخاب شده است")
		}
		seenInbound[id] = struct{}{}
	}
	if p.Kind == "test" {
		if p.ExpireSeconds <= 0 || p.ExpireSeconds > maxResellerPlanLifetimeSeconds || p.TestIPLimit < 0 || p.TestIPLimit > 10000 || p.MaxPerDay < 0 || p.MaxPerDay > 10000 || p.MaxBytes < 0 {
			return fmt.Errorf("مدت، حجم یا IP طرح تست از محدوده مجاز خارج است")
		}
	} else {
		if p.BaseIP < 0 || p.MaxIP < p.BaseIP || p.MaxIP > 10000 || p.BaseIP == 0 && p.MaxIP != 0 {
			return fmt.Errorf("حد IP پایه و حداکثر معتبر نیست")
		}
		if p.IsLimited {
			if p.PriceGB <= 0 || p.MinGB <= 0 || p.MinGB > 100000 || p.MaxBytes < 0 {
				return fmt.Errorf("طرح حجمی به قیمت هر GB و حداقل حجم مثبت نیاز دارد")
			}
		} else if p.BasePrice <= 0 {
			return fmt.Errorf("قیمت پایه باید بیشتر از صفر باشد")
		}
	}
	return nil
}

func (a *botApp) savePlanDraft(c telebot.Context, act actor, st conversation) error {
	p := st.PlanDraft
	if p == nil {
		return a.configPlans(c, act)
	}
	if err := validatePlanDraft(*p); err != nil {
		return a.show(c, "پیش از ذخیره، این موارد را اصلاح کنید: "+err.Error(), markup([]telebot.Btn{btn("بازگشت به پیش‌نویس", "plandraft|back")}))
	}
	inbounds, err := a.planInbounds(c, act.TelegramID)
	if err != nil {
		return a.sendFailure(c, err)
	}
	if err := validatePlanInboundSelection(*p, inbounds); err != nil {
		return a.show(c, "انتخاب inbound معتبر نیست: "+err.Error(), markup([]telebot.Btn{btn("انتخاب inboundها", "plandraft|inbounds")}))
	}
	p.IsGlobal = len(p.AllowedTelegramIDs) == 0
	var result struct {
		ID     int64       `json:"id"`
		Config adminConfig `json:"config"`
	}
	if err = a.call(c, "POST", "/v1/admin/config/plans", act.TelegramID, p, &result); err != nil {
		return a.show(c, "پاسخ ذخیره طرح تأیید نشد؛ پیش‌نویس نگه داشته شد. ابتدا فهرست طرح‌ها را تازه کنید تا از ذخیره‌شدن احتمالی مطمئن شوید.\n"+resellerFailureHint(err), markup([]telebot.Btn{btn("طرح‌ها را تازه کنید", "cfg|plans")}, []telebot.Btn{btn("بازگشت به پیش‌نویس", "plandraft|back")}))
	}
	if result.ID <= 0 {
		return a.show(c, "backend طرح را ذخیره کرد اما شناسهٔ آن را برنگرداند. فهرست طرح‌ها را تازه کنید و پیش از تلاش دوباره بررسی کنید.", markup([]telebot.Btn{btn("طرح‌ها را تازه کنید", "cfg|plans")}))
	}
	var created *plan
	for i := range result.Config.Plans {
		if result.Config.Plans[i].ID == result.ID {
			created = &result.Config.Plans[i]
			break
		}
	}
	if created == nil {
		var cfg adminConfig
		if err := a.call(c, "GET", "/v1/admin/config", act.TelegramID, nil, &cfg); err == nil {
			for i := range cfg.Plans {
				if cfg.Plans[i].ID == result.ID {
					created = &cfg.Plans[i]
					break
				}
			}
		}
	}
	if created == nil {
		return a.show(c, fmt.Sprintf("طرح شماره %d ذخیره شد اما در خواندن دوباره پیدا نشد. فهرست را تازه کنید.", result.ID), markup([]telebot.Btn{btn("طرح‌ها را تازه کنید", "cfg|plans")}))
	}
	a.clearFlow(act.TelegramID)
	return a.show(c, "✅ طرح شماره "+strconv.FormatInt(result.ID, 10)+" ذخیره و دوباره از backend خوانده شد.\n\n"+formatAdminPlan(*created), planEditorMarkup(*created))
}

func (a *botApp) showPlanDraftInbounds(c telebot.Context, act actor, st conversation) error {
	if st.PlanDraft == nil {
		return a.configPlans(c, act)
	}
	inbounds, err := a.planInbounds(c, act.TelegramID)
	if err != nil {
		return a.sendFailure(c, err)
	}
	available := make([]panelInbound, 0, len(inbounds))
	for _, inbound := range inbounds {
		if inbound.ID > 0 && (inbound.Enable || st.PlanDraft.ID > 0) {
			available = append(available, inbound)
		}
	}
	if len(available) == 0 {
		return a.show(c, "پنل inbound فعال ندارد.", markup([]telebot.Btn{btn("بازگشت", "plandraft|back")}))
	}
	sort.Slice(available, func(i, j int) bool { return available[i].ID < available[j].ID })
	st.DraftInboundPage = clampPlanPage(st.DraftInboundPage, len(available))
	st.Expires = time.Now().Add(20 * time.Minute)
	a.setFlow(act.TelegramID, st)
	start := st.DraftInboundPage * resellerPlanInboundPageSize
	end := min(start+resellerPlanInboundPageSize, len(available))
	selected := make(map[int]bool, len(st.PlanDraft.InboundIDs))
	for _, id := range st.PlanDraft.InboundIDs {
		selected[id] = true
	}
	rows := make([][]telebot.Btn, 0, resellerPlanInboundPageSize+3)
	var summary strings.Builder
	fmt.Fprintf(&summary, "اینباندهای پنل را برای طرح «%s» انتخاب کنید. برای طرح جدید فقط inbound فعال قابل انتخاب است؛ inbound غیرفعال انتخاب‌شده را می‌توانید حذف کنید.\nانتخاب‌شده: ", st.PlanDraft.Name)
	if len(st.PlanDraft.InboundIDs) == 0 {
		summary.WriteString("هیچ‌کدام")
	} else {
		summary.WriteString(planInboundNames(st.PlanDraft.InboundIDs))
	}
	for _, inbound := range available[start:end] {
		mark := "⬜"
		if selected[inbound.ID] {
			mark = "✅"
			if !inbound.Enable {
				mark = "⚠️"
			}
		} else if !inbound.Enable {
			mark = "⛔"
		}
		label := fmt.Sprintf("%s %d · %s · %s:%d", mark, inbound.ID, short(inbound.Remark, 22), inbound.Protocol, inbound.Port)
		rows = append(rows, []telebot.Btn{btn(label, fmt.Sprintf("planinbound|toggle|%d", inbound.ID))})
	}
	if len(available) > resellerPlanInboundPageSize {
		prev, next := st.DraftInboundPage-1, st.DraftInboundPage+1
		nav := []telebot.Btn{}
		if prev >= 0 {
			nav = append(nav, btn("⬅️ قبلی", fmt.Sprintf("planinbound|page|%d", prev)))
		}
		if next*resellerPlanInboundPageSize < len(available) {
			nav = append(nav, btn("بعدی ➡️", fmt.Sprintf("planinbound|page|%d", next)))
		}
		if len(nav) > 0 {
			rows = append(rows, nav)
		}
	}
	if len(available) <= 64 {
		rows = append(rows, []telebot.Btn{btn("✅ انتخاب همه فعال", "planinbound|all")})
	}
	rows = append(rows, []telebot.Btn{btn("🧹 پاک‌کردن انتخاب", "planinbound|clear"), btn("💾 تأیید و بازگشت", "planinbound|done")})
	rows = append(rows, []telebot.Btn{btn("بازگشت", "plandraft|back")})
	return a.show(c, summary.String(), markup(rows...))
}

func (a *botApp) handlePlanInboundAction(c telebot.Context, act actor, args []string) error {
	if len(args) == 0 {
		return a.configPlans(c, act)
	}
	a.mu.Lock()
	st, ok := a.flows[act.TelegramID]
	a.mu.Unlock()
	if !ok || st.PlanDraft == nil || time.Now().After(st.Expires) {
		return a.show(c, "فرآیند انتخاب inbound منقضی شده است.", markup([]telebot.Btn{btn("طرح‌ها", "cfg|plans")}))
	}
	inbounds, err := a.planInbounds(c, act.TelegramID)
	if err != nil {
		return a.sendFailure(c, err)
	}
	active := make(map[int]bool, len(inbounds))
	exists := make(map[int]bool, len(inbounds))
	var activeList []int
	for _, inbound := range inbounds {
		if inbound.ID > 0 {
			exists[inbound.ID] = true
			if inbound.Enable {
				active[inbound.ID] = true
				activeList = append(activeList, inbound.ID)
			}
		}
	}
	sort.Ints(activeList)
	switch args[0] {
	case "page":
		if len(args) != 2 {
			return a.showPlanDraftInbounds(c, act, st)
		}
		page, parseErr := strconv.Atoi(args[1])
		if parseErr != nil || page < 0 {
			return a.showPlanDraftInbounds(c, act, st)
		}
		st.DraftInboundPage = page
	case "toggle":
		if len(args) != 2 {
			return a.showPlanDraftInbounds(c, act, st)
		}
		id, parseErr := strconv.Atoi(args[1])
		if parseErr != nil || !exists[id] {
			return a.show(c, "این inbound در پنل پیدا نشد.", markup([]telebot.Btn{btn("بازگشت", "plandraft|inbounds")}))
		}
		if !active[id] && !containsInbound(st.PlanDraft.InboundIDs, id) {
			return a.show(c, "inbound غیرفعال را نمی‌توان به طرح اضافه کرد.", markup([]telebot.Btn{btn("بازگشت", "plandraft|inbounds")}))
		}
		st.PlanDraft.InboundIDs = togglePlanInbound(st.PlanDraft.InboundIDs, id)
	case "all":
		if len(activeList) > 64 {
			return a.show(c, "حداکثر ۶۴ inbound را جداگانه انتخاب کنید.", markup([]telebot.Btn{btn("بازگشت", "plandraft|inbounds")}))
		}
		st.PlanDraft.InboundIDs = append([]int(nil), activeList...)
	case "clear":
		st.PlanDraft.InboundIDs = []int{}
	case "done":
		if st.PlanDraft.ID > 0 {
			return a.saveExistingPlanInbounds(c, act, st.PlanDraft)
		}
		st.Step, st.Vals = "", nil
	default:
		return a.showPlanDraftInbounds(c, act, st)
	}
	st.Expires = time.Now().Add(20 * time.Minute)
	a.setFlow(act.TelegramID, st)
	if args[0] == "done" {
		return a.showPlanDraft(c, act, st.PlanDraft)
	}
	return a.showPlanDraftInbounds(c, act, st)
}

func (a *botApp) startExistingPlanInboundEdit(c telebot.Context, act actor, p plan) error {
	st := conversation{Step: "plan-inbound-edit", PlanID: p.ID, PlanDraft: &p, Expires: time.Now().Add(20 * time.Minute)}
	a.setFlow(act.TelegramID, st)
	return a.showPlanDraftInbounds(c, act, st)
}

func (a *botApp) saveExistingPlanInbounds(c telebot.Context, act actor, p *plan) error {
	if p == nil || p.ID <= 0 {
		return a.configPlans(c, act)
	}
	inbounds, err := a.planInbounds(c, act.TelegramID)
	if err != nil {
		return a.sendFailure(c, err)
	}
	if err := validatePlanInboundSelection(*p, inbounds); err != nil {
		return a.show(c, "انتخاب inbound معتبر نیست: "+err.Error(), markup([]telebot.Btn{btn("بازگشت به inboundها", fmt.Sprintf("planfield|%d|inbound_ids", p.ID))}))
	}
	payload, _ := json.Marshal(p)
	if err := a.call(c, "PUT", fmt.Sprintf("/v1/admin/config/plans/%d", p.ID), act.TelegramID, json.RawMessage(payload), nil); err != nil {
		return a.sendFailure(c, err)
	}
	a.clearFlow(act.TelegramID)
	return a.planEditorWithNotice(c, act, strconv.FormatInt(p.ID, 10), "✅ inboundهای طرح ذخیره شد.")
}

func planEditorMarkup(p plan) *telebot.ReplyMarkup {
	id := strconv.FormatInt(p.ID, 10)
	return markup(
		[]telebot.Btn{btn("نام", "planfield|"+id+"|name"), btn("نوع paid/test", "planfield|"+id+"|kind")},
		[]telebot.Btn{btn("فعال", "planfield|"+id+"|enabled"), btn("محدودیت حجمی", "planfield|"+id+"|is_limited")},
		[]telebot.Btn{btn("توضیحات", "planfield|"+id+"|description"), btn("قیمت پایه", "planfield|"+id+"|base_price_toman")},
		[]telebot.Btn{btn("قیمت هر GB", "planfield|"+id+"|price_per_gb_toman"), btn("قیمت IP اضافه", "planfield|"+id+"|price_per_extra_ip_toman")},
		[]telebot.Btn{btn("قیمت ماه اضافه", "planfield|"+id+"|price_per_extra_month_toman"), btn("سهمیه روزانه", "planfield|"+id+"|max_per_day")},
		[]telebot.Btn{btn("IP پایه", "planfield|"+id+"|base_ip_limit"), btn("حداکثر IP", "planfield|"+id+"|max_ip_limit")},
		[]telebot.Btn{btn("حداقل GB", "planfield|"+id+"|min_data_gb"), btn("حداکثر حجم GB", "planfield|"+id+"|max_data_gb")},
		[]telebot.Btn{btn("مدت تست", "planfield|"+id+"|expire_seconds"), btn("IP تست", "planfield|"+id+"|test_ip_limit")},
		[]telebot.Btn{btn("Flow", "planfield|"+id+"|flow"), btn("Inboundها", "planfield|"+id+"|inbound_ids")},
		[]telebot.Btn{btn("تخفیف‌ها", "planfield|"+id+"|discount_tiers"), btn("دسترسی", "planfield|"+id+"|allowed_telegram_ids")},
		[]telebot.Btn{btn("توضیح مصرف", "planfield|"+id+"|usage_description")},
		[]telebot.Btn{btn("بازگشت", "cfg|plans")},
	)
}

func formatAdminPlan(p plan) string {
	price := fmt.Sprintf("قیمت پایه %s تومان", formatToman(p.BasePrice))
	if p.IsLimited {
		price = fmt.Sprintf("قیمت هر GB %s تومان · حداقل %d GB", formatToman(p.PriceGB), p.MinGB)
	}
	return fmt.Sprintf("طرح شماره %d\n%s · %s · فعال: %t\n%s\nحد IP پایه/حداکثر: %d/%d · سقف حجم: %s · مدت تست: %s\nقیمت IP اضافه: %s تومان · سهمیه روزانه: %d\nتوضیحات: %s\nراهنما: %s\nFlow: %s\nInboundها: %s\nتخفیف‌ها: %s\nدسترسی: %s", p.ID, planSummaryText(p.Name, 120), planKindLabel(p.Kind), p.Enabled, price, p.BaseIP, p.MaxIP, formatPlanData(p.MaxBytes), humanDuration(p.ExpireSeconds), formatToman(p.PriceExtraIP), p.MaxPerDay, planSummaryText(p.Description, 400), planSummaryText(p.UsageDescription, 400), planSummaryText(p.Flow, 120), planSummaryText(planInboundNames(p.InboundIDs), 300), planSummaryText(planDiscountLabel(p.DiscountTiers), 400), planSummaryText(planAccessLabel(p.AllowedTelegramIDs), 400))
}

func planKindLabel(kind string) string {
	if kind == "test" {
		return "تست"
	}
	return "پولی"
}

func planInboundNames(ids []int) string {
	if len(ids) == 0 {
		return "هیچ‌کدام"
	}
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = strconv.Itoa(id)
	}
	return strings.Join(parts, ", ")
}

func planAccessLabel(ids []int64) string {
	if len(ids) == 0 {
		return "همه resellerهای این deployment"
	}
	values := make([]string, len(ids))
	for i, id := range ids {
		values[i] = strconv.FormatInt(id, 10)
	}
	return strings.Join(values, ", ")
}

func planDiscountLabel(tiers []discountTier) string {
	if len(tiers) == 0 {
		return "بدون تخفیف"
	}
	parts := make([]string, len(tiers))
	for i, tier := range tiers {
		parts[i] = fmt.Sprintf("%d ماه: %.2f%%", tier.Months, float64(tier.BasisPoints)/100)
	}
	return strings.Join(parts, "، ")
}

func formatPlanData(bytes int64) string {
	if bytes == 0 {
		return "نامحدود"
	}
	return fmt.Sprintf("%.2f GB", float64(bytes)/(1<<30))
}

func planValueOrDash(value string) string {
	if strings.TrimSpace(value) == "" {
		return "—"
	}
	return value
}

func planSummaryText(value string, maxRunes int) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "—"
	}
	runes := []rune(value)
	if len(runes) > maxRunes {
		return string(runes[:maxRunes]) + "…"
	}
	return value
}

func togglePlanInbound(ids []int, selected int) []int {
	out := make([]int, 0, len(ids)+1)
	found := false
	for _, id := range ids {
		if id == selected {
			found = true
		} else {
			out = append(out, id)
		}
	}
	if !found {
		out = append(out, selected)
	}
	sort.Ints(out)
	return out
}

func containsInbound(ids []int, target int) bool {
	for _, id := range ids {
		if id == target {
			return true
		}
	}
	return false
}

func clampPlanPage(page, items int) int {
	if page < 0 {
		return 0
	}
	last := (items - 1) / resellerPlanInboundPageSize
	if page > last {
		return last
	}
	return page
}
