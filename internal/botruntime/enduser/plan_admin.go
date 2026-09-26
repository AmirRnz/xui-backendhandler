package enduser

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/telebot.v3"
)

const (
	planInboundPageSize          = 12
	maxRetailPlanLifetimeSeconds = int64(10 * 365 * 24 * 60 * 60)
)

type panelInbound struct {
	ID       int    `json:"id"`
	Remark   string `json:"remark"`
	Tag      string `json:"tag"`
	Protocol string `json:"protocol"`
	Port     int    `json:"port"`
	Enable   bool   `json:"enable"`
}

func (a *botApp) startPlanDraft(c telebot.Context) error {
	inbounds, err := a.panelInbounds(c)
	if err != nil {
		return a.planDraftNeedsPanel(c, "اتصال این deployment به پنل 3x-ui در دسترس نیست. اتصال پنل را بررسی کنید، سپس دوباره طرح بسازید.")
	}
	active := false
	for _, inbound := range inbounds {
		if inbound.Enable {
			active = true
			break
		}
	}
	if !active {
		return a.planDraftNeedsPanel(c, "برای ساخت طرح باید دست‌کم یک inbound فعال در پنل 3x-ui داشته باشید.")
	}
	st := a.state(c.Sender().ID)
	st.Admin = "new-plan-name"
	st.Draft = nil
	st.DraftInboundPage = 0
	st = a.next(st)
	a.setState(c.Sender().ID, st)
	return a.prompt(c, "نام طرح جدید را ارسال کنید.", true)
}

func (a *botApp) planDraftNeedsPanel(c telebot.Context, message string) error {
	st := a.state(c.Sender().ID)
	markup := &telebot.ReplyMarkup{}
	markup.Inline(
		markup.Row(markup.Data("🖥 بررسی اتصال پنل", "nav", st.Nonce, "config-panel")),
		markup.Row(markup.Data("🔄 تلاش دوباره", "nav", st.Nonce, "config-new-plan"), markup.Data("↩️ طرح‌ها", "nav", st.Nonce, "config-plans")),
	)
	return present(c, message, markup, c.Callback() != nil)
}

func defaultPlanDraft(name, kind string) *adminPlan {
	p := &adminPlan{
		Name:               strings.TrimSpace(name),
		Kind:               kind,
		Enabled:            true,
		BaseIP:             1,
		MaxIP:              1,
		TestIPLimit:        1,
		MaxPerDay:          0,
		ExpireSeconds:      int64(24 * time.Hour / time.Second),
		IsGlobal:           true,
		InboundIDs:         []int64{},
		DiscountTiers:      []discountTier{},
		AllowedTelegramIDs: []int64{},
	}
	return p
}

func (a *botApp) choosePlanKind(c telebot.Context, st conversation, kind string) error {
	if st.Draft == nil || (kind != "paid" && kind != "test") {
		return a.adminPlans(c, true)
	}
	st.Draft.Kind = kind
	if kind == "paid" {
		st.Draft.MaxPerDay = 0
	} else {
		st.Draft.BasePrice = 0
		st.Draft.PricePerExtraIP = 0
		st.Draft.PricePerGB = 0
		st.Draft.PricePerExtraMonth = 0
	}
	st.Admin = ""
	st.DraftInboundPage = 0
	st = a.next(st)
	a.setState(c.Sender().ID, st)
	return a.showPlanDraft(c, true)
}

func (a *botApp) showPlanDraft(c telebot.Context, edit bool) error {
	st := a.state(c.Sender().ID)
	p := st.Draft
	if p == nil || (p.Kind != "paid" && p.Kind != "test") {
		return a.adminPlans(c, edit)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "تنظیم پیش‌نویس طرح %s\n\n", draftKindLabel(p.Kind))
	fmt.Fprintf(&b, "نام: %s\nتوضیحات: %s\nراهنما: %s\n", valueOrDash(p.Name), valueOrDash(p.Description), valueOrDash(p.UsageDescription))
	fmt.Fprintf(&b, "اینباندهای انتخاب‌شده: %s\n", formatInboundIDs(p.InboundIDs))
	fmt.Fprintf(&b, "دسترسی: %s\nفلو: %s\n", planAccessLabel(p.IsGlobal, p.AllowedTelegramIDs), valueOrDash(p.Flow))
	if p.Kind == "test" {
		fmt.Fprintf(&b, "مدت: %s\nسقف حجم: %s\nIP نمایشی: %d\n", humanDuration(p.ExpireSeconds), formatDataCap(p.MaxBytes), p.TestIPLimit)
	} else {
		if p.IsLimited {
			fmt.Fprintf(&b, "نوع: حجمی\nقیمت هر GB: %s تومان\nحداقل حجم: %d GB\nقیمت ماه اضافه: %s تومان\n", formatToman(p.PricePerGB), p.MinGB, formatToman(p.PricePerExtraMonth))
		} else {
			fmt.Fprintf(&b, "نوع: نامحدود\nقیمت پایه: %s تومان در ماه\n", formatToman(p.BasePrice))
		}
		fmt.Fprintf(&b, "IP نمایشی پایه/حداکثر: %d/%d\nهزینه IP اضافه: %s تومان\nتخفیف‌ها: %s\n", p.BaseIP, p.MaxIP, formatToman(p.PricePerExtraIP), formatDiscountTiers(p.DiscountTiers))
	}
	markup := &telebot.ReplyMarkup{}
	rows := []telebot.Row{
		markup.Row(markup.Data("📝 نام", "nav", st.Nonce, "draft-field", "name"), markup.Data("📝 توضیحات", "nav", st.Nonce, "draft-field", "description"), markup.Data("📖 راهنما", "nav", st.Nonce, "draft-field", "usage_description")),
		markup.Row(markup.Data("📡 انتخاب اینباندها", "nav", st.Nonce, "draft-inbounds"), markup.Data("👥 دسترسی", "nav", st.Nonce, "draft-field", "access")),
		markup.Row(markup.Data("⚡ فلو", "nav", st.Nonce, "draft-flow")),
	}
	if p.Kind == "test" {
		rows = append(rows,
			markup.Row(markup.Data("⏱️ مدت ساعت", "nav", st.Nonce, "draft-field", "duration_hours"), markup.Data("💾 سقف حجم GB", "nav", st.Nonce, "draft-field", "max_data_gb")),
			markup.Row(markup.Data("🌐 IP نمایشی", "nav", st.Nonce, "draft-field", "test_ip_limit")),
		)
	} else {
		limitLabel := "📊 نوع: نامحدود"
		if p.IsLimited {
			limitLabel = "📊 نوع: حجمی"
		}
		rows = append(rows, markup.Row(markup.Data(limitLabel, "nav", st.Nonce, "draft-toggle-limited")))
		if p.IsLimited {
			rows = append(rows,
				markup.Row(markup.Data("💵 قیمت/GB", "nav", st.Nonce, "draft-field", "price_per_gb_toman"), markup.Data("💾 حداقل GB", "nav", st.Nonce, "draft-field", "min_data_gb")),
				markup.Row(markup.Data("⏱️ قیمت ماه اضافه", "nav", st.Nonce, "draft-field", "price_per_extra_month_toman")),
			)
		} else {
			rows = append(rows, markup.Row(markup.Data("💵 قیمت پایه تومان", "nav", st.Nonce, "draft-field", "base_price_toman")))
		}
		rows = append(rows,
			markup.Row(markup.Data("🌐 IP پایه/حداکثر", "nav", st.Nonce, "draft-field", "ip_limits"), markup.Data("💲 قیمت IP اضافه", "nav", st.Nonce, "draft-field", "price_per_extra_ip_toman")),
			markup.Row(markup.Data("🏷️ تخفیف‌ها", "nav", st.Nonce, "draft-field", "discount_tiers")),
		)
	}
	rows = append(rows, markup.Row(markup.Data("💾 ذخیره طرح", "nav", st.Nonce, "draft-save"), markup.Data("❌ انصراف", "nav", st.Nonce, "draft-cancel")))
	markup.Inline(rows...)
	return present(c, strings.TrimSpace(b.String()), markup, edit)
}

func (a *botApp) panelInbounds(c telebot.Context) ([]panelInbound, error) {
	act, err := a.resolve(c)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var response struct {
		Inbounds []panelInbound `json:"inbounds"`
	}
	if err := a.api.Call(ctx, "GET", "/v1/admin/panels/inbounds", act.TelegramID, nil, &response); err != nil {
		return nil, err
	}
	return response.Inbounds, nil
}

func (a *botApp) showDraftInbounds(c telebot.Context, edit bool) error {
	st := a.state(c.Sender().ID)
	if st.Draft == nil {
		return a.adminPlans(c, edit)
	}
	inbounds, err := a.panelInbounds(c)
	if err != nil {
		return sendFailure(c, err)
	}
	available := inbounds
	if len(available) == 0 {
		return present(c, "پنل اینباند فعالی ندارد. ابتدا در 3x-ui یک inbound فعال ایجاد کنید.", backToPlanDraft(st), edit)
	}
	totalPages := (len(available) + planInboundPageSize - 1) / planInboundPageSize
	page := st.DraftInboundPage
	if page < 0 {
		page = 0
	}
	if page >= totalPages {
		page = totalPages - 1
	}
	st.DraftInboundPage = page
	st = a.next(st)
	a.setState(c.Sender().ID, st)
	st = a.state(c.Sender().ID)
	markup := &telebot.ReplyMarkup{}
	start := page * planInboundPageSize
	end := start + planInboundPageSize
	if end > len(available) {
		end = len(available)
	}
	rows := make([]telebot.Row, 0, planInboundPageSize+3)
	for _, inbound := range available[start:end] {
		selected := hasInbound(st.Draft.InboundIDs, int64(inbound.ID))
		mark := "⬜"
		if selected {
			mark = "✅"
		} else if !inbound.Enable {
			mark = "⛔"
		}
		label := inbound.Remark
		if strings.TrimSpace(label) == "" {
			label = inbound.Tag
		}
		if strings.TrimSpace(label) == "" {
			label = "Inbound"
		}
		if !inbound.Enable {
			label += " · غیرفعال"
		}
		label = truncateButton(label)
		rows = append(rows, markup.Row(markup.Data(fmt.Sprintf("%s #%d %s · %s:%d", mark, inbound.ID, label, inbound.Protocol, inbound.Port), "nav", st.Nonce, "draft-inbound-toggle", strconv.Itoa(inbound.ID))))
	}
	pageButtons := make([]telebot.Btn, 0, 2)
	if page > 0 {
		pageButtons = append(pageButtons, markup.Data("◀️ قبلی", "nav", st.Nonce, "draft-inbounds-page", strconv.Itoa(page-1)))
	}
	if page+1 < totalPages {
		pageButtons = append(pageButtons, markup.Data("بعدی ▶️", "nav", st.Nonce, "draft-inbounds-page", strconv.Itoa(page+1)))
	}
	if len(pageButtons) > 0 {
		rows = append(rows, markup.Row(pageButtons...))
	}
	rows = append(rows,
		markup.Row(markup.Data("✅ انتخاب همه فعال‌ها", "nav", st.Nonce, "draft-inbounds-all"), markup.Data("🗑 پاک‌کردن انتخاب", "nav", st.Nonce, "draft-inbounds-clear")),
		markup.Row(markup.Data("اتمام انتخاب", "nav", st.Nonce, "draft-inbounds-done")),
	)
	markup.Inline(rows...)
	text := fmt.Sprintf("اینباندهای فعال پنل را برای این طرح انتخاب کنید. اینباند غیرفعال را می‌توانید از انتخاب حذف کنید.\nصفحه %d از %d · انتخاب‌شده: %d", page+1, totalPages, len(st.Draft.InboundIDs))
	return present(c, text, markup, edit)
}

func (a *botApp) toggleDraftInbound(c telebot.Context, st conversation, rawID string) error {
	if st.Draft == nil {
		return a.adminPlans(c, true)
	}
	id, err := strconv.Atoi(rawID)
	if err != nil || id <= 0 {
		return a.showDraftInbounds(c, true)
	}
	inbounds, err := a.panelInbounds(c)
	if err != nil {
		return sendFailure(c, err)
	}
	want := int64(id)
	selectedIndex := -1
	for i, existing := range st.Draft.InboundIDs {
		if existing == want {
			selectedIndex = i
			break
		}
	}
	valid := false
	for _, inbound := range inbounds {
		if inbound.ID == id && inbound.Enable {
			valid = true
			break
		}
	}
	if !valid && selectedIndex == -1 {
		return a.showDraftInbounds(c, true)
	}
	if selectedIndex >= 0 {
		st.Draft.InboundIDs = append(st.Draft.InboundIDs[:selectedIndex], st.Draft.InboundIDs[selectedIndex+1:]...)
		return a.savePlanDraftAndShowInbounds(c, st)
	}
	if len(st.Draft.InboundIDs) >= 64 {
		return c.Send("حداکثر ۶۴ inbound برای هر طرح مجاز است.")
	}
	st.Draft.InboundIDs = append(st.Draft.InboundIDs, want)
	sort.Slice(st.Draft.InboundIDs, func(i, j int) bool { return st.Draft.InboundIDs[i] < st.Draft.InboundIDs[j] })
	return a.savePlanDraftAndShowInbounds(c, st)
}

func (a *botApp) setDraftAllInbounds(c telebot.Context, st conversation, selected bool) error {
	if st.Draft == nil {
		return a.adminPlans(c, true)
	}
	if !selected {
		st.Draft.InboundIDs = []int64{}
		return a.savePlanDraftAndShowInbounds(c, st)
	}
	inbounds, err := a.panelInbounds(c)
	if err != nil {
		return sendFailure(c, err)
	}
	ids := make([]int64, 0, len(inbounds))
	for _, inbound := range inbounds {
		if inbound.Enable {
			ids = append(ids, int64(inbound.ID))
		}
	}
	if len(ids) > 64 {
		return c.Send("این پنل بیش از ۶۴ inbound فعال دارد؛ آن‌ها را جداگانه انتخاب کنید.")
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	st.Draft.InboundIDs = ids
	return a.savePlanDraftAndShowInbounds(c, st)
}

func (a *botApp) savePlanDraftAndShow(c telebot.Context, st conversation, edit bool) error {
	st.Admin = ""
	st = a.next(st)
	a.setState(c.Sender().ID, st)
	return a.showPlanDraft(c, edit)
}

func (a *botApp) savePlanDraftAndShowInbounds(c telebot.Context, st conversation) error {
	st.Admin = ""
	st = a.next(st)
	a.setState(c.Sender().ID, st)
	return a.showDraftInbounds(c, true)
}

func (a *botApp) editPlanInbounds(c telebot.Context, id int64) error {
	cfg, err := a.loadConfig(c)
	if err != nil {
		return sendFailure(c, err)
	}
	for _, p := range cfg.Plans {
		if p.ID == id {
			st := a.state(c.Sender().ID)
			st.Draft = &p
			st.Admin = ""
			st.DraftInboundPage = 0
			st = a.next(st)
			a.setState(c.Sender().ID, st)
			return a.showDraftInbounds(c, true)
		}
	}
	return a.adminPlans(c, true)
}

func (a *botApp) saveExistingPlanInbounds(c telebot.Context, p *adminPlan) error {
	if p == nil || p.ID <= 0 {
		return a.adminPlans(c, true)
	}
	if p.Enabled && len(p.InboundIDs) == 0 {
		return c.Send("برای ذخیره یک طرح فعال، حداقل یک inbound را انتخاب کنید.")
	}
	inbounds, err := a.panelInbounds(c)
	if err != nil {
		return sendFailure(c, err)
	}
	available := make(map[int64]bool, len(inbounds))
	for _, inbound := range inbounds {
		if inbound.Enable {
			available[int64(inbound.ID)] = true
		}
	}
	for _, id := range p.InboundIDs {
		if !available[id] {
			return c.Send(fmt.Sprintf("Inbound شماره %d دیگر فعال نیست؛ انتخاب‌ها را به‌روز کنید.", id))
		}
	}
	return a.updatePlan(c, *p)
}

func (a *botApp) promptDraftField(c telebot.Context, st conversation, field string) error {
	if st.Draft == nil {
		return a.adminPlans(c, true)
	}
	if field == "flow" {
		markup := &telebot.ReplyMarkup{}
		markup.Inline(
			markup.Row(markup.Data("⚡ xtls-rprx-vision", "nav", st.Nonce, "draft-flow-set", "xtls-rprx-vision")),
			markup.Row(markup.Data("بدون فلو", "nav", st.Nonce, "draft-flow-set", "")),
			markup.Row(markup.Data("فلو سفارشی", "nav", st.Nonce, "draft-field", "flow_custom")),
			markup.Row(markup.Data("↩️ پیش‌نویس", "nav", st.Nonce, "draft-back")),
		)
		return present(c, "فلو فعلی: "+valueOrDash(st.Draft.Flow), markup, true)
	}
	st.Admin = "draft:" + field
	st = a.next(st)
	a.setState(c.Sender().ID, st)
	return a.prompt(c, draftFieldPrompt(field), true)
}

func draftFieldPrompt(field string) string {
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
		"max_per_day":                 "سهمیه روزانه تست را بفرستید؛ صفر یعنی نامحدود.",
		"discount_tiers":              "تخفیف‌ها را به فرمت ماه:درصد وارد کنید، مثل 3:10,6:20؛ برای حذف - بفرستید.",
		"access":                      "شناسه‌های عددی تلگرام کاربران مجاز را با کاما بفرستید؛ - یعنی همه کاربران این instance.",
		"flow_custom":                 "مقدار فلو را بفرستید؛ برای پاک‌کردن - بفرستید.",
	}[field]
}

func (a *botApp) saveDraftText(c telebot.Context, st conversation, value string) error {
	if st.Draft == nil || !strings.HasPrefix(st.Admin, "draft:") {
		return c.Send("پیش‌نویس طرح منقضی شده است؛ دوباره شروع کنید.")
	}
	field := strings.TrimPrefix(st.Admin, "draft:")
	value = strings.TrimSpace(value)
	p := st.Draft
	parseNonnegativeInt64 := func() (int64, error) {
		n, err := strconv.ParseInt(value, 10, 64)
		if err != nil || n < 0 {
			return 0, fmt.Errorf("عدد صحیح نامنفی وارد کنید")
		}
		return n, nil
	}
	parseNonnegativeInt := func() (int, error) {
		n, err := strconv.Atoi(value)
		if err != nil || n < 0 {
			return 0, fmt.Errorf("عدد صحیح نامنفی وارد کنید")
		}
		return n, nil
	}
	var err error
	switch field {
	case "name":
		if value == "" || len([]byte(value)) > 120 {
			err = fmt.Errorf("نام باید بین ۱ تا ۱۲۰ بایت باشد")
		} else {
			p.Name = value
		}
	case "description", "usage_description", "flow_custom":
		if value == "-" {
			value = ""
		}
		switch field {
		case "description":
			if len([]byte(value)) > 2048 {
				err = fmt.Errorf("توضیحات حداکثر ۲۰۴۸ بایت باشد")
			} else {
				p.Description = value
			}
		case "usage_description":
			if len([]byte(value)) > 4096 {
				err = fmt.Errorf("راهنما حداکثر ۴۰۹۶ بایت باشد")
			} else {
				p.UsageDescription = value
			}
		case "flow_custom":
			if len(value) > 120 {
				err = fmt.Errorf("فلو حداکثر ۱۲۰ بایت باشد")
			} else {
				p.Flow = value
			}
		}
	case "base_price_toman", "price_per_extra_ip_toman", "price_per_gb_toman", "price_per_extra_month_toman":
		var n int64
		n, err = parseNonnegativeInt64()
		if err == nil {
			switch field {
			case "base_price_toman":
				p.BasePrice = n
			case "price_per_extra_ip_toman":
				p.PricePerExtraIP = n
			case "price_per_gb_toman":
				p.PricePerGB = n
			case "price_per_extra_month_toman":
				p.PricePerExtraMonth = n
			}
		}
	case "min_data_gb", "test_ip_limit", "max_per_day":
		var n int
		n, err = parseNonnegativeInt()
		if err == nil && (field == "min_data_gb" && n > 100000 || field == "test_ip_limit" && n > 10000 || field == "max_per_day" && n > 10000) {
			err = fmt.Errorf("عدد از حداکثر مجاز بیشتر است")
		}
		if err == nil {
			switch field {
			case "min_data_gb":
				p.MinGB = n
			case "test_ip_limit":
				p.TestIPLimit = n
			case "max_per_day":
				p.MaxPerDay = n
			}
		}
	case "ip_limits":
		if value == "0" || strings.EqualFold(value, "unlimited") {
			p.BaseIP, p.MaxIP = 0, 0
		} else {
			parts := strings.FieldsFunc(value, func(r rune) bool { return r == '-' || r == ',' })
			if len(parts) != 2 {
				err = fmt.Errorf("دو عدد با خط تیره بفرستید؛ نمونه 1-3")
			} else {
				base, e1 := strconv.Atoi(strings.TrimSpace(parts[0]))
				max, e2 := strconv.Atoi(strings.TrimSpace(parts[1]))
				if e1 != nil || e2 != nil || base <= 0 || max < base || max > 10000 {
					err = fmt.Errorf("حد IP نامعتبر است")
				} else {
					p.BaseIP, p.MaxIP = base, max
				}
			}
		}
	case "duration_hours":
		hours, e := strconv.ParseFloat(value, 64)
		seconds := hours * 3600
		if e != nil || math.IsNaN(hours) || math.IsInf(hours, 0) || hours <= 0 || seconds < 1 || seconds > float64(maxRetailPlanLifetimeSeconds) {
			err = fmt.Errorf("مدت باید بین صفر تا ده سال باشد")
		} else {
			p.ExpireSeconds = int64(seconds)
		}
	case "max_data_gb":
		gb, e := strconv.ParseFloat(value, 64)
		bytes := gb * float64(uint64(1)<<30)
		if e != nil || math.IsNaN(gb) || math.IsInf(gb, 0) || gb < 0 || bytes >= float64(math.MaxInt64) {
			err = fmt.Errorf("حجم باید صفر یا عددی مثبت و محدود باشد")
		} else {
			p.MaxBytes = int64(bytes)
		}
	case "discount_tiers":
		p.DiscountTiers, err = parseDiscountTierText(value)
	case "access":
		p.AllowedTelegramIDs, err = parseTelegramIDList(value)
		if err == nil {
			p.IsGlobal = len(p.AllowedTelegramIDs) == 0
		}
	default:
		err = fmt.Errorf("فیلد طرح نامعتبر است")
	}
	if err != nil {
		return c.Send("مقدار نامعتبر است: " + err.Error())
	}
	st.Admin = ""
	return a.savePlanDraftAndShow(c, st, false)
}

func (a *botApp) savePlanDraft(c telebot.Context) error {
	st := a.state(c.Sender().ID)
	p := st.Draft
	if p == nil {
		return a.adminPlans(c, true)
	}
	if err := validatePlanDraft(*p); err != nil {
		return c.Send("پیش از ذخیره، این موارد را اصلاح کنید: " + err.Error())
	}
	inbounds, err := a.panelInbounds(c)
	if err != nil {
		return sendFailure(c, err)
	}
	available := make(map[int64]bool, len(inbounds))
	for _, inbound := range inbounds {
		if inbound.Enable {
			available[int64(inbound.ID)] = true
		}
	}
	for _, id := range p.InboundIDs {
		if !available[id] {
			return c.Send(fmt.Sprintf("Inbound شماره %d دیگر فعال نیست؛ انتخاب‌ها را به‌روز کنید.", id))
		}
	}
	return a.createPlan(c, p)
}

func validatePlanDraft(p adminPlan) error {
	if strings.TrimSpace(p.Name) == "" || len([]byte(p.Name)) > 120 {
		return fmt.Errorf("نام طرح الزامی است")
	}
	if len([]byte(p.Description)) > 2048 || len([]byte(p.UsageDescription)) > 4096 || len(p.Flow) > 120 {
		return fmt.Errorf("توضیحات، راهنما یا فلو از حداکثر طول مجاز بیشتر است")
	}
	if p.Kind != "paid" && p.Kind != "test" {
		return fmt.Errorf("نوع طرح را انتخاب کنید")
	}
	if len(p.InboundIDs) == 0 {
		return fmt.Errorf("حداقل یک inbound فعال انتخاب کنید")
	}
	if len(p.InboundIDs) > 64 {
		return fmt.Errorf("حداکثر ۶۴ inbound انتخاب کنید")
	}
	if p.Kind == "test" {
		if p.ExpireSeconds <= 0 || p.ExpireSeconds > maxRetailPlanLifetimeSeconds {
			return fmt.Errorf("مدت تست باید بیشتر از صفر و حداکثر ده سال باشد")
		}
		return nil
	}
	if p.BaseIP < 0 || p.MaxIP < p.BaseIP || p.MaxIP > 10000 || p.BaseIP == 0 && p.MaxIP != 0 {
		return fmt.Errorf("حد IP پایه و حداکثر معتبر نیست")
	}
	if p.IsLimited {
		if p.PricePerGB <= 0 || p.MinGB <= 0 {
			return fmt.Errorf("طرح حجمی به قیمت هر GB و حداقل حجم مثبت نیاز دارد")
		}
	} else if p.BasePrice <= 0 {
		return fmt.Errorf("قیمت پایه باید بیشتر از صفر باشد")
	}
	return nil
}

func parseTelegramIDList(raw string) ([]int64, error) {
	if strings.TrimSpace(raw) == "" || strings.TrimSpace(raw) == "-" {
		return []int64{}, nil
	}
	parts := strings.Split(raw, ",")
	if len(parts) > 256 {
		return nil, fmt.Errorf("حداکثر ۲۵۶ کاربر مجاز است")
	}
	ids := make([]int64, 0, len(parts))
	seen := make(map[int64]struct{}, len(parts))
	for _, part := range parts {
		id, err := strconv.ParseInt(strings.TrimSpace(part), 10, 64)
		if err != nil || id <= 0 {
			return nil, fmt.Errorf("شناسه‌های تلگرام باید اعداد مثبت باشند")
		}
		if _, exists := seen[id]; exists {
			return nil, fmt.Errorf("شناسه تکراری وجود دارد")
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	return ids, nil
}

func parseDiscountTierText(raw string) ([]discountTier, error) {
	if strings.TrimSpace(raw) == "" || strings.TrimSpace(raw) == "-" {
		return []discountTier{}, nil
	}
	parts := strings.Split(raw, ",")
	if len(parts) > 64 {
		return nil, fmt.Errorf("حداکثر ۶۴ تخفیف مجاز است")
	}
	items := make([]discountTier, 0, len(parts))
	seen := make(map[int]struct{}, len(parts))
	for _, part := range parts {
		pair := strings.Split(strings.TrimSpace(part), ":")
		if len(pair) != 2 {
			return nil, fmt.Errorf("فرمت ماه:درصد را وارد کنید")
		}
		months, errMonths := strconv.Atoi(strings.TrimSpace(pair[0]))
		percent, errPercent := strconv.ParseFloat(strings.TrimSpace(pair[1]), 64)
		basisPoints := percent * 100
		if errMonths != nil || months < 1 || months > 36 || errPercent != nil || math.IsNaN(percent) || math.IsInf(percent, 0) || percent < 0 || percent > 100 || math.Abs(basisPoints-math.Round(basisPoints)) > 0.000001 {
			return nil, fmt.Errorf("ماه باید ۱ تا ۳۶ و درصد ۰ تا ۱۰۰ با حداکثر دو اعشار باشد")
		}
		if _, exists := seen[months]; exists {
			return nil, fmt.Errorf("هر تعداد ماه را فقط یک‌بار وارد کنید")
		}
		seen[months] = struct{}{}
		items = append(items, discountTier{Months: months, BasisPoints: int(math.Round(basisPoints))})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Months < items[j].Months })
	return items, nil
}

func formatDiscountTiers(items []discountTier) string {
	if len(items) == 0 {
		return "ندارد"
	}
	parts := make([]string, 0, len(items))
	for _, item := range items {
		parts = append(parts, fmt.Sprintf("%d ماه: %s%%", item.Months, formatBasisPoints(item.BasisPoints)))
	}
	return strings.Join(parts, "، ")
}

func formatDataCap(bytes int64) string {
	if bytes <= 0 {
		return "نامحدود"
	}
	return fmt.Sprintf("%.2f GB", float64(bytes)/(1<<30))
}

func formatInboundIDs(ids []int64) string {
	if len(ids) == 0 {
		return "انتخاب نشده"
	}
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = strconv.FormatInt(id, 10)
	}
	return strings.Join(parts, ", ")
}

func hasInbound(ids []int64, target int64) bool {
	for _, id := range ids {
		if id == target {
			return true
		}
	}
	return false
}

func planAccessLabel(global bool, ids []int64) string {
	if global || len(ids) == 0 {
		return "همه کاربران این instance"
	}
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = strconv.FormatInt(id, 10)
	}
	return "فقط " + strings.Join(parts, ", ")
}

func valueOrDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "—"
	}
	return strings.TrimSpace(s)
}

func planEditorMarkup(st conversation, p adminPlan) *telebot.ReplyMarkup {
	markup := &telebot.ReplyMarkup{}
	id := strconv.FormatInt(p.ID, 10)
	field := func(label, name string) telebot.Btn {
		return markup.Data(label, "nav", st.Nonce, "pf", id, planFieldCode(name))
	}
	toggle := func(label, code string) telebot.Btn {
		return markup.Data(label, "nav", st.Nonce, "pt", id, code)
	}
	rows := []telebot.Row{
		markup.Row(field("نام: "+buttonValue(p.Name), "name"), field("نوع: "+p.Kind, "kind")),
		markup.Row(toggle(boolLabel("طرح فعال", p.Enabled), "e")),
		markup.Row(field("توضیحات", "description"), field("راهنمای اتصال", "usage_description")),
		markup.Row(field("دسترسی", "access"), field("Flow", "flow")),
		markup.Row(markup.Data("📡 انتخاب اینباندهای پنل", "nav", st.Nonce, "plan-inbounds", id)),
	}
	if p.Kind == "paid" {
		rows = append(rows,
			markup.Row(toggle(boolLabel("طرح حجمی", p.IsLimited), "l")),
			markup.Row(field("قیمت پایه", "base_price_toman"), field("قیمت هر GB", "price_per_gb_toman")),
			markup.Row(field("قیمت IP اضافه", "price_per_extra_ip_toman"), field("قیمت ماه اضافه", "price_per_extra_month_toman")),
			markup.Row(field(fmt.Sprintf("IP پایه/حد: %d/%d", p.BaseIP, p.MaxIP), "ip_limits"), field(fmt.Sprintf("حداقل GB: %d", p.MinGB), "min_data_gb")),
			markup.Row(field("حداکثر حجم bytes", "max_data_bytes"), field("تخفیف‌ها", "discount_tiers")),
		)
	} else {
		rows = append(rows,
			markup.Row(field("مدت تست seconds", "expire_seconds"), field(fmt.Sprintf("IP نمایشی: %d", p.TestIPLimit), "test_ip_limit")),
		)
	}
	rows = append(rows, markup.Row(markup.Data("↩️ بازگشت به طرح‌ها", "nav", st.Nonce, "config-plans")))
	markup.Inline(rows...)
	return markup
}

func boolText(value bool) string {
	if value {
		return "فعال"
	}
	return "غیرفعال"
}

func draftKindLabel(kind string) string {
	if kind == "test" {
		return "تست"
	}
	return "خرید"
}

func backToPlanDraft(st conversation) *telebot.ReplyMarkup {
	markup := &telebot.ReplyMarkup{}
	label := "↩️ بازگشت به پیش‌نویس"
	if st.Draft != nil && st.Draft.ID > 0 {
		label = "↩️ بازگشت به طرح"
	}
	markup.Inline(markup.Row(markup.Data(label, "nav", st.Nonce, "draft-back")))
	return markup
}
