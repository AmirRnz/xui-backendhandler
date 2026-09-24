package reseller

import (
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"gopkg.in/telebot.v3"
)

func adminCallbackAction(action string) bool {
	switch action {
	case "admin", "pending", "pendingtopups", "pendingpage", "resellers", "resellerpage", "resapprove", "resreject", "approve", "approvetopup", "review-payment", "review-topup", "reject-payment", "reject-topup", "confirm-review-reject", "review-refund", "reject-refund", "confirm-refund-reject", "work-items", "refunds", "config", "cfg", "cfgset", "planedit", "planfield", "feature":
		return true
	default:
		return false
	}
}

const resumePageSize = 8
const receiptRecoveryMaxPages = 10

func (a *botApp) activeReceipts(c telebot.Context, actorID int64) ([]receiptState, error) {
	active, err := a.activeReceiptDetails(c, actorID)
	if err != nil {
		return nil, err
	}
	return activeReceiptStates(active), nil
}

func activeReceiptStates(active activeReceiptSet) []receiptState {
	states := make([]receiptState, 0, len(active.Payments.Items)+len(active.Topups.Items))
	for _, item := range active.Payments.Items {
		if item.ID > 0 {
			states = append(states, receiptState{Kind: "payment", ID: item.ID})
		}
	}
	for _, item := range active.Topups.Items {
		if item.ID > 0 {
			states = append(states, receiptState{Kind: "topup", ID: item.ID})
		}
	}
	return states
}

func receiptCandidatesForPhoto(active activeReceiptSet, complete bool) []receiptState {
	candidates := make([]receiptState, 0, 2)
	for _, item := range active.Payments.Items {
		if item.ID > 0 && item.Status == "awaiting_receipt" {
			candidates = append(candidates, receiptState{Kind: "payment", ID: item.ID})
		}
	}
	for _, item := range active.Topups.Items {
		if item.ID > 0 && item.Status == "awaiting_receipt" {
			candidates = append(candidates, receiptState{Kind: "topup", ID: item.ID})
		}
	}
	if !complete {
		return nil
	}
	return candidates
}

func (a *botApp) activeReceiptPage(c telebot.Context, actorID int64, kind string, beforeID int64) (activeReceiptPage, error) {
	var path string
	var response struct {
		PaymentIntents []activeReceipt `json:"payment_intents"`
		Topups         []activeReceipt `json:"topups"`
		PaymentIntent  *activeReceipt  `json:"payment_intent"`
		Topup          *activeReceipt  `json:"topup"`
		NextCursor     *int64          `json:"next_cursor"`
	}
	switch kind {
	case "payment":
		path = "/v1/payment-intents/active"
	case "topup":
		path = "/v1/wallet/topups/active"
	default:
		return activeReceiptPage{}, fmt.Errorf("نوع فاکتور نامعتبر است")
	}
	if beforeID < 0 {
		return activeReceiptPage{}, fmt.Errorf("invalid active receipt cursor")
	}
	if beforeID > 0 {
		path += "?before_id=" + strconv.FormatInt(beforeID, 10)
	}
	if err := a.call(c, "GET", path, actorID, nil, &response); err != nil {
		return activeReceiptPage{}, err
	}
	items := response.PaymentIntents
	newest := response.PaymentIntent
	if kind == "topup" {
		items = response.Topups
		newest = response.Topup
	}
	// Retain compatibility with an older backend while arrays are introduced.
	if len(items) == 0 && newest != nil {
		items = []activeReceipt{*newest}
	}
	return activeReceiptPage{Items: items, NextCursor: response.NextCursor}, nil
}

func (a *botApp) activeReceiptDetails(c telebot.Context, actorID int64) (activeReceiptSet, error) {
	payments, err := a.activeReceiptPage(c, actorID, "payment", 0)
	if err != nil {
		return activeReceiptSet{}, err
	}
	topups, err := a.activeReceiptPage(c, actorID, "topup", 0)
	if err != nil {
		return activeReceiptSet{}, err
	}
	return activeReceiptSet{Payments: payments, Topups: topups}, nil
}

func (a *botApp) showResumeMenu(c telebot.Context, act actor) error {
	active, err := a.activeReceiptDetails(c, act.TelegramID)
	if err != nil {
		return a.sendFailure(c, err)
	}
	rows := make([][]telebot.Btn, 0, 3)
	var text strings.Builder
	text.WriteString("درخواست‌های پرداخت و شارژ شما:\n")
	for _, item := range []struct {
		kind  string
		label string
		page  activeReceiptPage
	}{{"payment", "پرداخت سرویس", active.Payments}, {"topup", "شارژ کیف پول", active.Topups}} {
		if len(item.page.Items) == 0 {
			continue
		}
		latest := item.page.Items[0]
		fmt.Fprintf(&text, "\n%s: %d مورد در انتظار · آخرین #%d، %s تومان، %s", item.label, len(item.page.Items), latest.ID, formatToman(latest.Amount), receiptStatusLabel(latest.Status))
		if item.page.NextCursor != nil {
			text.WriteString(" · موارد قدیمی‌تر هم وجود دارد")
		}
		rows = append(rows, []telebot.Btn{btn("مشاهده «"+item.label+"»", fmt.Sprintf("resume-list|%s|0|0", item.kind))})
	}
	if len(rows) == 0 {
		return a.show(c, "فاکتور فعالی برای ادامه وجود ندارد.", markup([]telebot.Btn{btn("خانه", "home")}))
	}
	rows = append(rows, []telebot.Btn{btn("خانه", "home")})
	return a.show(c, strings.TrimSpace(text.String()), markup(rows...))
}

func (a *botApp) showResumeList(c telebot.Context, act actor, kind string, beforeID int64, offset int) error {
	if (kind != "payment" && kind != "topup") || beforeID < 0 || offset < 0 || offset >= 100 {
		return a.show(c, "پارامتر فهرست فاکتورها نامعتبر است.", markup([]telebot.Btn{btn("ادامه فاکتور", "resume")}, []telebot.Btn{btn("خانه", "home")}))
	}
	page, err := a.activeReceiptPage(c, act.TelegramID, kind, beforeID)
	if err != nil {
		return a.sendFailure(c, err)
	}
	if offset > len(page.Items) {
		return a.show(c, "این صفحه دیگر در دسترس نیست.", markup([]telebot.Btn{btn("ادامه فاکتور", "resume")}))
	}
	label := "پرداخت سرویس"
	if kind == "topup" {
		label = "شارژ کیف پول"
	}
	if len(page.Items) == 0 {
		return a.show(c, "درخواستی برای نمایش وجود ندارد.", markup([]telebot.Btn{btn("بازگشت", "resume")}))
	}
	end := offset + resumePageSize
	if end > len(page.Items) {
		end = len(page.Items)
	}
	rows := make([][]telebot.Btn, 0, resumePageSize+3)
	var text strings.Builder
	fmt.Fprintf(&text, "%sها (موارد %d تا %d از این صفحه):", label, offset+1, end)
	for _, item := range page.Items[offset:end] {
		fmt.Fprintf(&text, "\n\n#%d · %s تومان · %s", item.ID, formatToman(item.Amount), receiptStatusLabel(item.Status))
		if item.Status == "awaiting_receipt" {
			rows = append(rows, []telebot.Btn{btn(fmt.Sprintf("📷 رسید #%d · %s", item.ID, formatToman(item.Amount)), fmt.Sprintf("resume-receipt|%s|%d", kind, item.ID))})
		}
	}
	if end < len(page.Items) {
		rows = append(rows, []telebot.Btn{btn("موارد بعدی", fmt.Sprintf("resume-list|%s|%d|%d", kind, beforeID, end))})
	} else if page.NextCursor != nil && *page.NextCursor > 0 {
		rows = append(rows, []telebot.Btn{btn("درخواست‌های قدیمی‌تر", fmt.Sprintf("resume-list|%s|%d|0", kind, *page.NextCursor))})
	}
	if beforeID > 0 || offset > 0 {
		rows = append(rows, []telebot.Btn{btn("جدیدترین درخواست‌ها", fmt.Sprintf("resume-list|%s|0|0", kind))})
	}
	rows = append(rows, []telebot.Btn{btn("بازگشت به فهرست", "resume")})
	return a.show(c, text.String(), markup(rows...))
}

func receiptStatusLabel(status string) string {
	switch status {
	case "awaiting_receipt":
		return "منتظر ارسال رسید"
	case "receipt_submitted":
		return "رسید ارسال شده، در انتظار بررسی"
	case "approved":
		return "تأیید شده"
	case "rejected":
		return "رد شده"
	default:
		return status
	}
}

func (a *botApp) findActiveReceipt(c telebot.Context, actorID int64, kind string, id int64) (*activeReceipt, error) {
	for pageNo, cursor := 0, int64(0); pageNo < receiptRecoveryMaxPages; pageNo++ {
		page, err := a.activeReceiptPage(c, actorID, kind, cursor)
		if err != nil {
			return nil, err
		}
		for i := range page.Items {
			if page.Items[i].ID == id {
				return &page.Items[i], nil
			}
		}
		if page.NextCursor == nil || *page.NextCursor <= 0 {
			return nil, nil
		}
		cursor = *page.NextCursor
	}
	return nil, nil
}

func (a *botApp) resumeReceipt(c telebot.Context, act actor, kind string, id int64) error {
	if kind != "payment" && kind != "topup" {
		return a.home(c, act, "نوع فاکتور نامعتبر است.")
	}
	selected, err := a.findActiveReceipt(c, act.TelegramID, kind, id)
	if err != nil {
		return a.sendFailure(c, err)
	}
	if selected == nil || selected.Status != "awaiting_receipt" {
		return a.show(c, "این فاکتور دیگر منتظر رسید نیست. وضعیت را از منوی ادامه فاکتور بررسی کنید.", markup([]telebot.Btn{btn("ادامه فاکتور", "resume")}, []telebot.Btn{btn("خانه", "home")}))
	}
	a.mu.Lock()
	a.receipts[act.TelegramID] = receiptState{Kind: kind, ID: id}
	a.mu.Unlock()
	return a.show(c, fmt.Sprintf("رسید فاکتور %d را به‌صورت عکس ارسال کنید. مبلغ: %s تومان.", id, formatToman(selected.Amount)), markup([]telebot.Btn{btn("لغو", "home")}))
}

func (a *botApp) awaitingPhotoReceipts(c telebot.Context, actorID int64) (activeReceiptSet, bool, error) {
	all := activeReceiptSet{}
	complete := true
	for _, kind := range []string{"payment", "topup"} {
		var cursor int64
		for pageNo := 0; pageNo < receiptRecoveryMaxPages; pageNo++ {
			page, err := a.activeReceiptPage(c, actorID, kind, cursor)
			if err != nil {
				return activeReceiptSet{}, false, err
			}
			target := &all.Payments
			if kind == "topup" {
				target = &all.Topups
			}
			target.Items = append(target.Items, page.Items...)
			if page.NextCursor == nil || *page.NextCursor <= 0 {
				target.NextCursor = nil
				break
			}
			if pageNo == receiptRecoveryMaxPages-1 {
				target.NextCursor = page.NextCursor
				complete = false
				break
			}
			cursor = *page.NextCursor
		}
	}
	return all, complete, nil
}

func (a *botApp) currentPurchaseFlow(id int64, step string) (conversation, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	st, ok := a.flows[id]
	if !ok || time.Now().After(st.Expires) || st.Step != step || st.Vals == nil {
		return conversation{}, false
	}
	return st, true
}

func (a *botApp) purchaseFlow(id int64) (conversation, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	st, ok := a.flows[id]
	if !ok || time.Now().After(st.Expires) || st.Vals == nil {
		return conversation{}, false
	}
	return st, true
}

func (a *botApp) showPurchaseDuration(c telebot.Context, p plan) error {
	rows := [][]telebot.Btn{{btn("۱ ماهه", "duration|1"), btn("۳ ماهه", "duration|3")}, {btn("۶ ماهه", "duration|6"), btn("✏️ مدت دلخواه", "duration-custom")}, {btn("بازگشت به طرح‌ها", "plans|paid"), btn("خانه", "home")}}
	return a.show(c, formatPlanDetails(p, "paid")+"\n\nمدت سرویس را انتخاب کنید:", markup(rows...))
}

func (a *botApp) advancePurchase(c telebot.Context, act actor, st conversation) error {
	p, err := a.planByID(c, st.PlanID)
	if err != nil {
		return a.sendFailure(c, err)
	}
	if p.IsLimited {
		return a.showPurchaseData(c, p, st)
	}
	return a.showPurchaseIP(c, act, st)
}

func (a *botApp) showPurchaseData(c telebot.Context, p plan, st conversation) error {
	min := p.MinGB
	if min < 1 {
		min = 1
	}
	st.Step = "gb"
	a.setFlow(c.Sender().ID, st)
	rows := [][]telebot.Btn{{btn(fmt.Sprintf("%d GB", min), fmt.Sprintf("data|%d", min)), btn(fmt.Sprintf("%d GB", min+10), fmt.Sprintf("data|%d", min+10))}, {btn(fmt.Sprintf("%d GB", min+30), fmt.Sprintf("data|%d", min+30)), btn(fmt.Sprintf("%d GB", min+50), fmt.Sprintf("data|%d", min+50))}, {btn("✏️ حجم دلخواه", "data-custom")}, {btn("بازگشت", "back-duration"), btn("خانه", "home")}}
	return a.show(c, fmt.Sprintf("%s\n\nحجم را انتخاب کنید یا مقدار دلخواه را وارد کنید (حداقل %d GB):", formatPlanDetails(p, "paid"), min), markup(rows...))
}

func (a *botApp) showPurchaseIP(c telebot.Context, act actor, st conversation) error {
	p, err := a.planByID(c, st.PlanID)
	if err != nil {
		return a.sendFailure(c, err)
	}
	st.Step = "ip"
	a.setFlow(act.TelegramID, st)
	rows := make([][]telebot.Btn, 0, 12)
	choices := purchaseIPPreview(p.BaseIP, p.MaxIP)
	for _, ip := range choices {
		label := fmt.Sprintf("%d IP هم‌زمان", ip)
		if ip == 0 {
			label = "IP هم‌زمان نامحدود"
		}
		rows = append(rows, []telebot.Btn{btn(label, fmt.Sprintf("ip|%d", ip))})
	}
	if needsCustomPurchaseIP(p.BaseIP, p.MaxIP) {
		rows = append(rows, []telebot.Btn{btn("✏️ تعداد دلخواه IP", "ip-custom")})
	}
	back := "back-duration"
	if p.IsLimited {
		back = "back-data"
	}
	rows = append(rows, []telebot.Btn{btn("بازگشت", back), btn("خانه", "home")})
	return a.show(c, fmt.Sprintf("%s\nمدت: %s ماه\n\nمحدودیت IP هم‌زمان را انتخاب کنید (%d تا %d):", p.Name, st.Vals["months"], p.BaseIP, p.MaxIP), markup(rows...))
}

func needsCustomPurchaseIP(base, max int) bool {
	if base < 0 || max < base {
		return false
	}
	return max-base+1 > len(purchaseIPPreview(base, max))
}

func purchaseIPPreview(base, max int) []int {
	if base < 0 || max < base {
		return nil
	}
	const previewLimit = 8
	count := max - base + 1
	if count > previewLimit {
		count = previewLimit
	}
	choices := make([]int, 0, count)
	for ip := base; ip < base+count; ip++ {
		choices = append(choices, ip)
	}
	return choices
}

func validPurchaseIPLimit(ip int, p plan) bool {
	return p.BaseIP >= 0 && p.MaxIP >= p.BaseIP && ip >= p.BaseIP && ip <= p.MaxIP
}

func (a *botApp) promptPurchaseName(c telebot.Context, act actor, st conversation) error {
	st.Step = "name"
	a.setFlow(act.TelegramID, st)
	return a.show(c, "نام دلخواه سرویس را بفرستید؛ برای نام پیش‌فرض دکمه را بزنید یا «-» ارسال کنید.", markup([]telebot.Btn{btn("🎲 نام پیش‌فرض", "purchase-default-name")}, []telebot.Btn{btn("بازگشت", "back-ip"), btn("خانه", "home")}))
}

func (a *botApp) createPurchaseQuote(c telebot.Context, act actor, st conversation, name string) error {
	if st.Step != "name" || st.Vals == nil {
		return a.home(c, act, "فرآیند خرید منقضی شده است.")
	}
	if utf8.RuneCountInString(name) > 64 {
		return a.setFlowAndPrompt(c, act.TelegramID, st, "نام سرویس حداکثر ۶۴ نویسه می‌تواند باشد.")
	}
	months, e1 := strconv.Atoi(st.Vals["months"])
	ip, e2 := strconv.Atoi(st.Vals["ip"])
	gb, e3 := strconv.Atoi(st.Vals["gb"])
	if e1 != nil || e2 != nil || e3 != nil || months < 1 || months > 36 || ip < 0 || gb < 0 || gb > 100000 {
		return a.home(c, act, "جزئیات خرید نامعتبر است؛ لطفاً خرید را دوباره آغاز کنید.")
	}
	p, err := a.planByID(c, st.PlanID)
	if err != nil {
		return a.sendFailure(c, err)
	}
	if !validPurchaseIPLimit(ip, p) {
		return a.home(c, act, "محدودیت IP انتخاب‌شده برای این طرح معتبر نیست؛ خرید را دوباره آغاز کنید.")
	}
	key := a.operationKey(c, "purchase")
	var q quote
	if err := a.call(c, "POST", "/v1/quotes", act.TelegramID, map[string]any{"plan_id": st.PlanID, "months": months, "ip_limit": ip, "data_gb": gb, "idempotency_key": "quote-" + key}, &q); err != nil {
		return a.sendFailure(c, err)
	}
	if q.ID <= 0 || q.Price <= 0 {
		return a.show(c, "پیش‌فاکتور معتبر از backend دریافت نشد. خرید ثبت نشده است.", markup([]telebot.Btn{btn("خانه", "home")}))
	}
	st.Step = "purchase-confirm"
	st.Vals["quote_id"] = strconv.FormatInt(q.ID, 10)
	st.Vals["amount_toman"] = strconv.FormatInt(q.Price, 10)
	st.Vals["operation_key"] = key
	st.Vals["name"] = name
	st.Expires = time.Now().Add(20 * time.Minute)
	a.setFlow(act.TelegramID, st)
	return a.renderPurchaseInvoice(c, act, st, "")
}

func (a *botApp) renderPurchaseInvoice(c telebot.Context, act actor, st conversation, notice string) error {
	quoteID, _ := strconv.ParseInt(st.Vals["quote_id"], 10, 64)
	amount, _ := strconv.ParseInt(st.Vals["amount_toman"], 10, 64)
	months, _ := strconv.Atoi(st.Vals["months"])
	ip, _ := strconv.Atoi(st.Vals["ip"])
	gb, _ := strconv.Atoi(st.Vals["gb"])
	if quoteID <= 0 || amount <= 0 {
		return a.home(c, act, "پیش‌فاکتور منقضی شده است.")
	}
	p, err := a.planByID(c, st.PlanID)
	if err != nil {
		return a.sendFailure(c, err)
	}
	name := strings.TrimSpace(st.Vals["name"])
	if name == "" {
		name = "نام پیش‌فرض (توسط سیستم انتخاب می‌شود)"
	}
	data := "نامحدود"
	if p.IsLimited {
		data = fmt.Sprintf("%d GB", gb)
	}
	text := fmt.Sprintf("🧾 پیش‌فاکتور خرید\n\nطرح: %s\nنام سرویس: %s\nمدت: %d ماه\nحجم: %s\nIP هم‌زمان: %s\nمبلغ کل: %s تومان", p.Name, name, months, data, formatIPLimit(ip), formatToman(amount))
	features := a.runtime(c, act.TelegramID)
	if featureEnabled(features.Features, "wallet_enabled") {
		var wallet struct {
			Balance int64 `json:"balance_toman"`
		}
		if err := a.call(c, "GET", "/v1/wallet", act.TelegramID, nil, &wallet); err == nil {
			text += fmt.Sprintf("\nموجودی کیف پول: %s تومان", formatToman(wallet.Balance))
		} else {
			text += "\nموجودی کیف پول: در دسترس نیست"
		}
	}
	if notice != "" {
		text = notice + "\n\n" + text
	}
	methods := purchaseMethodButtons(features, st.Vals["retry_method"])
	if len(methods) == 0 {
		methods = append(methods, btn("بازگشت به خانه", "home"))
	}
	return a.show(c, text+"\n\nبا انتخاب روش پرداخت، این مبلغ و شرایط را تأیید می‌کنید.", markup(methods, []telebot.Btn{btn("❌ لغو خرید", "cancel-purchase")}))
}

func (a *botApp) confirmPurchase(c telebot.Context, act actor, st conversation, method string) error {
	if !purchaseRetryMethodAllowed(method, st.Vals["retry_method"]) {
		return a.renderPurchaseInvoice(c, act, st, "نتیجه تلاش قبلی هنوز قطعی نیست و کلید پرداخت به همان روش متصل است. همان روش را دوباره انتخاب کنید؛ برای تغییر روش، خرید را لغو و از ابتدا شروع کنید.")
	}
	features := a.runtime(c, act.TelegramID)
	if method == "wallet" && !featureEnabled(features.Features, "wallet_enabled") || method == "direct" && !featureEnabled(features.Features, "direct_payments_enabled") {
		return a.renderPurchaseInvoice(c, act, st, "این روش پرداخت در حال حاضر فعال نیست.")
	}
	var instructions paymentInstructions
	if method == "direct" {
		if err := a.call(c, "GET", "/v1/payment-instructions", act.TelegramID, nil, &instructions); err != nil {
			return a.renderPurchaseInvoice(c, act, st, "اطلاعات پرداخت مستقیم دریافت نشد؛ هیچ فاکتوری ثبت نشده است. پس از بررسی دوباره تلاش کنید یا کیف پول را انتخاب کنید.")
		}
		if !validPaymentDestination(instructions) {
			return a.renderPurchaseInvoice(c, act, st, "اطلاعات کارت پرداخت کامل نیست؛ هیچ فاکتوری ثبت نشده است. مدیر باید شماره کارت و نام صاحب کارت را تنظیم کند.")
		}
	}
	quoteID, err := strconv.ParseInt(st.Vals["quote_id"], 10, 64)
	if err != nil || quoteID <= 0 || st.Vals["operation_key"] == "" {
		return a.home(c, act, "پیش‌فاکتور معتبر نیست؛ لطفاً خرید را دوباره آغاز کنید.")
	}
	var out purchase
	if err = a.call(c, "POST", "/v1/purchases", act.TelegramID, map[string]any{"quote_id": quoteID, "payment_method": method, "idempotency_key": "purchase-" + st.Vals["operation_key"], "display_name": st.Vals["name"]}, &out); err != nil {
		st.Vals["retry_method"] = method
		st.Expires = time.Now().Add(20 * time.Minute)
		a.setFlow(act.TelegramID, st)
		return a.renderPurchaseInvoice(c, act, st, "نتیجه ثبت پرداخت نامشخص است؛ این روش را با همان درخواست دوباره امتحان کنید. برای تغییر روش، خرید را لغو و از ابتدا شروع کنید.")
	}
	if out.Amount <= 0 || (method == "direct" && out.IntentID <= 0) {
		st.Vals["retry_method"] = method
		st.Expires = time.Now().Add(20 * time.Minute)
		a.setFlow(act.TelegramID, st)
		return a.renderPurchaseInvoice(c, act, st, "پاسخ ثبت پرداخت ناقص است؛ نتیجه نامشخص است. همان روش را با همان درخواست دوباره امتحان کنید.")
	}
	a.clearFlow(act.TelegramID)
	if method == "wallet" {
		return a.show(c, fmt.Sprintf("خرید ثبت شد. مبلغ %s تومان؛ وضعیت: %s.", formatToman(out.Amount), out.Status), markup([]telebot.Btn{btn("سرویس‌های من", "services"), btn("خانه", "home")}))
	}
	text := fmt.Sprintf("فاکتور شماره %d\nمبلغ: %s تومان\n%s", out.IntentID, formatToman(out.Amount), paymentInstructionDetails(instructions))
	return a.show(c, text, markup([]telebot.Btn{btn("📷 ارسال عکس رسید", fmt.Sprintf("receipt|payment|%d", out.IntentID))}, []telebot.Btn{btn("خانه", "home")}))
}

func purchaseMethodButtons(features runtimeConfig, retryMethod string) []telebot.Btn {
	if retryMethod == "wallet" {
		return []telebot.Btn{btn("🔁 تلاش دوباره با کیف پول", "confirm-purchase|wallet")}
	}
	if retryMethod == "direct" {
		return []telebot.Btn{btn("🔁 تلاش دوباره با پرداخت مستقیم", "confirm-purchase|direct")}
	}
	methods := []telebot.Btn{}
	if featureEnabled(features.Features, "wallet_enabled") {
		methods = append(methods, btn("👛 تأیید و پرداخت از کیف پول", "confirm-purchase|wallet"))
	}
	if featureEnabled(features.Features, "direct_payments_enabled") {
		methods = append(methods, btn("💳 تأیید و پرداخت مستقیم", "confirm-purchase|direct"))
	}
	return methods
}

func purchaseRetryMethodAllowed(selected, retryMethod string) bool {
	return retryMethod == "" || selected == retryMethod
}

func (a *botApp) showWorkItems(c telebot.Context, act actor) error {
	var items []map[string]any
	if err := a.call(c, "GET", "/v1/admin/work-items", act.TelegramID, nil, &items); err != nil {
		return a.sendFailure(c, err)
	}
	if len(items) == 0 {
		return a.show(c, "هیچ کار عملیاتی ثبت نشده است.", markup([]telebot.Btn{btn("مدیریت", "admin")}))
	}
	var text strings.Builder
	if len(items) > 20 {
		items = items[:20]
	}
	for _, item := range items {
		fmt.Fprintf(&text, "کار #%v · %v · وضعیت: %v · مرحله: %v · تلاش: %v\n", item["id"], item["kind"], item["status"], item["phase"], item["attempts"])
	}
	return a.show(c, strings.TrimSpace(text.String()), markup([]telebot.Btn{btn("مدیریت", "admin")}))
}

func (a *botApp) showRefunds(c telebot.Context, act actor) error {
	var items []struct {
		ID             int64  `json:"id"`
		SubscriptionID int64  `json:"subscription_id"`
		Status         string `json:"status"`
		Suggested      int64  `json:"suggested_amount_toman"`
		Cap            int64  `json:"refundable_cap_toman"`
		Reason         string `json:"reason"`
	}
	if err := a.call(c, "GET", "/v1/admin/refunds", act.TelegramID, nil, &items); err != nil {
		return a.sendFailure(c, err)
	}
	if len(items) == 0 {
		return a.show(c, "درخواست استرداد معوقی وجود ندارد.", markup([]telebot.Btn{btn("مدیریت", "admin")}))
	}
	rows := make([][]telebot.Btn, 0, len(items)+1)
	for _, item := range items {
		rows = append(rows, []telebot.Btn{btn(fmt.Sprintf("استرداد #%d · %s تومان", item.ID, formatToman(item.Suggested)), fmt.Sprintf("review-refund|%d", item.ID))})
	}
	rows = append(rows, []telebot.Btn{btn("مدیریت", "admin")})
	return a.show(c, "درخواست‌های استرداد معوق را برای مشاهده جزئیات انتخاب کنید.", markup(rows...))
}

func (a *botApp) reviewRefund(c telebot.Context, act actor, id int64) error {
	var items []struct {
		ID             int64  `json:"id"`
		SubscriptionID int64  `json:"subscription_id"`
		Status         string `json:"status"`
		Suggested      int64  `json:"suggested_amount_toman"`
		Cap            int64  `json:"refundable_cap_toman"`
		Reason         string `json:"reason"`
	}
	if err := a.call(c, "GET", "/v1/admin/refunds", act.TelegramID, nil, &items); err != nil {
		return a.sendFailure(c, err)
	}
	for _, item := range items {
		if item.ID != id {
			continue
		}
		text := fmt.Sprintf("درخواست استرداد #%d\nسرویس: #%d\nوضعیت: %s\nمبلغ پیشنهادی: %s تومان\nسقف مجاز: %s تومان\nدلیل: %s", item.ID, item.SubscriptionID, item.Status, formatToman(item.Suggested), formatToman(item.Cap), item.Reason)
		return a.show(c, text, markup([]telebot.Btn{btn("رد درخواست", fmt.Sprintf("reject-refund|%d", item.ID))}, []telebot.Btn{btn("بازگشت", "refunds")}))
	}
	return a.show(c, "درخواست استرداد پیدا نشد یا قبلاً بررسی شده است.", markup([]telebot.Btn{btn("بازگشت", "refunds")}))
}

func (a *botApp) reviewPendingItem(c telebot.Context, act actor, kind string, id int64) error {
	if !isPrivateChat(c.Chat()) || !isAdmin(act) {
		return a.show(c, "بررسی رسید فقط در گفت‌وگوی خصوصی مدیر در دسترس است.")
	}
	path, label := "/v1/admin/payments", "پرداخت سرویس"
	if kind == "topup" {
		path, label = "/v1/admin/topups", "شارژ کیف پول"
	} else if kind != "payment" {
		return a.show(c, "نوع درخواست نامعتبر است.")
	}
	var items []adminReviewItem
	if err := a.call(c, "GET", path, act.TelegramID, nil, &items); err != nil {
		return a.sendFailure(c, err)
	}
	for _, item := range items {
		if item.ID != id {
			continue
		}
		text := adminReviewSummary(label, item)
		hasEvidence := hasAdminReviewEvidence(item)
		if hasEvidence {
			caption := fmt.Sprintf("مدرک رسید · %s #%d · %s تومان · %s", label, item.ID, formatToman(item.Amount), adminReviewApplicant(item))
			if err := c.Send(&telebot.Photo{File: telebot.File{FileID: item.TelegramFileID}, Caption: caption}); err != nil {
				return a.show(c, "ارسال تصویر رسید به گفت‌وگوی خصوصی مدیر ناموفق بود؛ دکمه تأیید فعال نشده است.", markup([]telebot.Btn{btn("بازگشت", pendingMenuAction(kind))}))
			}
		} else {
			text += "\n\nتصویر رسید در دسترس نیست؛ تأیید غیرفعال شد."
		}
		rows := [][]telebot.Btn{}
		if hasEvidence {
			approveAction := "approve"
			if kind == "topup" {
				approveAction = "approvetopup"
			}
			rows = append(rows, []telebot.Btn{btn("✅ تأیید پس از بررسی رسید", fmt.Sprintf("%s|%d", approveAction, item.ID))})
		}
		rows = append(rows, []telebot.Btn{btn("رد درخواست", fmt.Sprintf("reject-%s|%d", kind, item.ID))}, []telebot.Btn{btn("بازگشت", pendingMenuAction(kind))})
		return a.show(c, text, markup(rows...))
	}
	return a.show(c, "درخواست پیدا نشد یا رسید آن قبلاً بررسی شده است.", markup([]telebot.Btn{btn("بازگشت", pendingMenuAction(kind))}))
}

func pendingMenuAction(kind string) string {
	if kind == "topup" {
		return "pendingtopups"
	}
	return "pending"
}

func adminReviewSummary(label string, item adminReviewItem) string {
	return fmt.Sprintf("%s · درخواست #%d\nحساب: #%d\nدرخواست‌کننده: %s\nمبلغ: %s تومان\nوضعیت: %s\nزمان: %s", label, item.ID, item.AccountID, adminReviewApplicant(item), formatToman(item.Amount), item.Status, item.CreatedAt)
}

func adminReviewApplicant(item adminReviewItem) string {
	if item.TelegramID > 0 {
		return fmt.Sprintf("تلگرام %d", item.TelegramID)
	}
	return fmt.Sprintf("شناسه actor در backend #%d", item.ActorID)
}

func adminReviewNeedsReceipt(item adminReviewItem) bool {
	return item.Status == "receipt_submitted"
}

func hasAdminReviewEvidence(item adminReviewItem) bool {
	return strings.TrimSpace(item.TelegramFileID) != "" && adminReviewNeedsReceipt(item)
}

func adminRejectPath(kind string, id int64) (string, bool) {
	if id <= 0 {
		return "", false
	}
	switch kind {
	case "payment":
		return fmt.Sprintf("/v1/payment-intents/%d/reject", id), true
	case "topup":
		return fmt.Sprintf("/v1/admin/topups/%d/reject", id), true
	default:
		return "", false
	}
}

func adminRefundRejectPath(id int64) (string, bool) {
	if id <= 0 {
		return "", false
	}
	return fmt.Sprintf("/v1/admin/refunds/%d/reject", id), true
}

func formatPlanDetails(p plan, kind string) string {
	var b strings.Builder
	if kind == "test" {
		data := "نامحدود"
		if p.MaxBytes > 0 {
			data = fmt.Sprintf("%.2f GB", float64(p.MaxBytes)/1073741824)
		}
		fmt.Fprintf(&b, "🧪 %s\nمدت اعتبار: %s (از اولین اتصال)\nحجم: %s", p.Name, humanDuration(p.ExpireSeconds), data)
		if p.TestIPLimit > 0 {
			fmt.Fprintf(&b, "\nIP هم‌زمان: %d", p.TestIPLimit)
		}
		if p.MaxPerDay > 0 {
			fmt.Fprintf(&b, "\nسقف روزانه طرح: %d", p.MaxPerDay)
		}
	} else if p.IsLimited {
		fmt.Fprintf(&b, "📦 %s (حجمی)\nقیمت هر GB: %s تومان\nحداقل حجم: %d GB\nماه اضافه: %s تومان\nIP هم‌زمان: %d تا %d\nهر IP اضافه: %s تومان/ماه", p.Name, formatToman(p.PriceGB), p.MinGB, formatToman(p.PriceExtraMonth), p.BaseIP, p.MaxIP, formatToman(p.PriceExtraIP))
	} else {
		fmt.Fprintf(&b, "📦 %s (نامحدود)\nقیمت پایه: %s تومان/ماه\nIP هم‌زمان: %d تا %d\nهر IP اضافه: %s تومان/ماه", p.Name, formatToman(p.BasePrice), p.BaseIP, p.MaxIP, formatToman(p.PriceExtraIP))
	}
	if strings.TrimSpace(p.Description) != "" {
		fmt.Fprintf(&b, "\n%s", strings.TrimSpace(p.Description))
	}
	if strings.TrimSpace(p.UsageDescription) != "" {
		fmt.Fprintf(&b, "\nراهنما: %s", strings.TrimSpace(p.UsageDescription))
	}
	if kind == "paid" && len(p.DiscountTiers) > 0 {
		b.WriteString("\nتخفیف خرید بلندمدت:")
		for _, tier := range p.DiscountTiers {
			fmt.Fprintf(&b, "\nاز %d ماه: %s%%", tier.Months, formatBasisPoints(tier.BasisPoints))
		}
	}
	if kind == "test" {
		b.WriteString("\nسهمیه و تأیید در زمان ثبت درخواست توسط backend بررسی می‌شود.")
	}
	return b.String()
}

func formatBasisPoints(bps int64) string {
	whole, fraction := bps/100, bps%100
	if fraction == 0 {
		return strconv.FormatInt(whole, 10)
	}
	return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%d.%02d", whole, fraction), "0"), ".")
}

func humanDuration(seconds int64) string {
	if seconds <= 0 {
		return "نامشخص"
	}
	days := seconds / 86400
	if days > 0 {
		return fmt.Sprintf("%d روز", days)
	}
	hours := seconds / 3600
	if hours > 0 {
		return fmt.Sprintf("%d ساعت", hours)
	}
	return fmt.Sprintf("%d دقیقه", seconds/60)
}

func formatIPLimit(ip int) string {
	if ip == 0 {
		return "نامحدود"
	}
	return strconv.Itoa(ip)
}

func formatToman(amount int64) string {
	text := strconv.FormatInt(amount, 10)
	for i := len(text) - 3; i > 0; i -= 3 {
		text = text[:i] + "," + text[i:]
	}
	return text
}

func paymentInstructionDetails(instructions paymentInstructions) string {
	var b strings.Builder
	if instructions.CardNumber != "" {
		fmt.Fprintf(&b, "شماره کارت: %s\n", instructions.CardNumber)
	}
	if instructions.CardOwner != "" {
		fmt.Fprintf(&b, "صاحب کارت: %s\n", instructions.CardOwner)
	}
	if instructions.Instructions != "" {
		fmt.Fprintf(&b, "%s\n", instructions.Instructions)
	}
	return strings.TrimSpace(b.String())
}

func validPaymentDestination(instructions paymentInstructions) bool {
	return strings.TrimSpace(instructions.CardNumber) != "" && strings.TrimSpace(instructions.CardOwner) != ""
}

func topupInstructionsText(instructions paymentInstructions) string {
	var b strings.Builder
	b.WriteString("برای شارژ کیف پول، مبلغ را به تومان وارد کنید. پس از ثبت درخواست، عکس رسید را ارسال کنید.\n\n")
	b.WriteString(paymentInstructionDetails(instructions))
	if instructions.MinTopupToman > 0 {
		fmt.Fprintf(&b, "\nحداقل مبلغ شارژ: %s تومان", formatToman(instructions.MinTopupToman))
	}
	return strings.TrimSpace(b.String())
}

func validateTopupAmount(amount, minimum int64) error {
	if amount <= 0 {
		return fmt.Errorf("amount must be positive")
	}
	if minimum > 0 && amount < minimum {
		return fmt.Errorf("minimum top-up is %d", minimum)
	}
	return nil
}
