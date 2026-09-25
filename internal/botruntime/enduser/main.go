package enduser

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"example.com/xui-commerce/backend/internal/botruntime/backend"
	"gopkg.in/telebot.v3"
)

var adminTelegramID int64 = 96937669

type actor struct {
	TelegramID     int64  `json:"telegram_id"`
	Role           string `json:"role"`
	ApprovalStatus string `json:"approval_status"`
}
type plan struct {
	ID                 int64          `json:"id"`
	Name               string         `json:"name"`
	Description        string         `json:"description"`
	Kind               string         `json:"kind"`
	IsLimited          bool           `json:"is_limited"`
	BasePrice          int64          `json:"base_price_toman"`
	PricePerExtraIP    int64          `json:"price_per_extra_ip_toman"`
	PriceGB            int64          `json:"price_per_gb_toman"`
	PricePerExtraMonth int64          `json:"price_per_extra_month_toman"`
	MinGB              int            `json:"min_data_gb"`
	BaseIP             int            `json:"base_ip_limit"`
	MaxIP              int            `json:"max_ip_limit"`
	MaxBytes           int64          `json:"max_data_bytes"`
	ExpireSeconds      int64          `json:"expire_seconds"`
	UsageDescription   string         `json:"usage_description"`
	DiscountTiers      []discountTier `json:"discount_tiers"`
}
type discountTier struct {
	Months      int `json:"months"`
	BasisPoints int `json:"basis_points"`
}
type quote struct {
	ID                   int64  `json:"id"`
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
	Price                int64  `json:"final_price_toman"`
	Currency             string `json:"currency"`
}
type purchase struct {
	OrderID        int64  `json:"order_id"`
	Status         string `json:"status"`
	IntentID       int64  `json:"payment_intent_id"`
	SubscriptionID int64  `json:"subscription_id"`
	Amount         int64  `json:"amount_toman"`
}
type subscriptionView struct {
	ID                int64    `json:"id"`
	Email             string   `json:"email"`
	DisplayName       string   `json:"display_name"`
	Status            string   `json:"status"`
	Kind              string   `json:"kind"`
	IPLimit           int      `json:"ip_limit"`
	TrafficLimitBytes int64    `json:"traffic_limit_bytes"`
	ExpiryTimeMS      int64    `json:"expiry_time_ms"`
	Links             []string `json:"links"`
}
type subscriptionLinkPage struct {
	Subscription subscriptionView
	LinkIndex    int
}

const retailServicesPageSize = 6

func subscriptionLinkPages(subscriptions []subscriptionView) []subscriptionLinkPage {
	pages := make([]subscriptionLinkPage, 0, len(subscriptions))
	for _, sub := range subscriptions {
		hasLink := false
		for i, link := range sub.Links {
			if strings.TrimSpace(link) != "" {
				pages = append(pages, subscriptionLinkPage{sub, i})
				hasLink = true
			}
		}
		if !hasLink {
			pages = append(pages, subscriptionLinkPage{sub, -1})
		}
	}
	return pages
}

type receiptState struct {
	Kind string
	ID   int64
}
type activeReceiptRequest struct {
	ID        int64  `json:"id"`
	Status    string `json:"status"`
	Amount    int64  `json:"amount_toman"`
	CreatedAt string `json:"created_at"`
}
type activePaymentIntentResponse struct {
	PaymentIntent  *activeReceiptRequest   `json:"payment_intent"`
	PaymentIntents []*activeReceiptRequest `json:"payment_intents"`
	NextCursor     *int64                  `json:"next_cursor"`
}
type activeTopupResponse struct {
	Topup      *activeReceiptRequest   `json:"topup"`
	Topups     []*activeReceiptRequest `json:"topups"`
	NextCursor *int64                  `json:"next_cursor"`
}
type receiptPageCursor struct {
	Payment int64
	Topup   int64
}
type activeReceiptPage struct {
	Payments    []*activeReceiptRequest
	Topups      []*activeReceiptRequest
	PaymentNext *int64
	TopupNext   *int64
}
type conversation struct {
	Nonce                    string
	Step                     string
	PlanID                   int64
	Method                   string
	Months                   int
	IPLimit                  int
	DataGB                   int
	Name                     string
	Receipt                  *receiptState
	ReceiptPhotoFileID       string
	ReceiptPagePaymentBefore int64
	ReceiptPageTopupBefore   int64
	ReceiptPagePaymentNext   int64
	ReceiptPageTopupNext     int64
	ReceiptPageHistory       []receiptPageCursor
	AdminReceiptKind         string
	AdminReceiptID           int64
	AdminPendingKind         string
	AdminPendingOffset       int
	Admin                    string
	AdminID                  int64
	PanelURL                 string
	Draft                    *adminPlan
	OperationKey             string
	QuoteID                  int64
	QuotePrice               int64
	RefundSubID              int64
	Updated                  time.Time
}
type adminPlan struct {
	ID                 int64   `json:"id,omitempty"`
	Name               string  `json:"name"`
	Kind               string  `json:"kind"`
	Enabled            bool    `json:"enabled"`
	IsLimited          bool    `json:"is_limited"`
	Description        string  `json:"description"`
	BasePrice          int64   `json:"base_price_toman"`
	PricePerExtraIP    int64   `json:"price_per_extra_ip_toman"`
	PricePerGB         int64   `json:"price_per_gb_toman"`
	PricePerExtraMonth int64   `json:"price_per_extra_month_toman"`
	BaseIP             int     `json:"base_ip_limit"`
	MaxIP              int     `json:"max_ip_limit"`
	MinGB              int     `json:"min_data_gb"`
	MaxBytes           int64   `json:"max_data_bytes"`
	ExpireSeconds      int64   `json:"expire_seconds"`
	TestIPLimit        int     `json:"test_ip_limit"`
	MaxPerDay          int     `json:"max_per_day"`
	Flow               string  `json:"flow"`
	InboundIDs         []int64 `json:"inbound_ids"`
	UsageDescription   string  `json:"usage_description"`
}
type adminConfig struct {
	DeploymentID        string            `json:"deployment_id"`
	Channel             string            `json:"channel"`
	Plans               []adminPlan       `json:"plans"`
	PaymentInstructions map[string]string `json:"payment_instructions"`
	Settings            struct {
		RetailTrialResetDays      int               `json:"retail_trial_reset_days"`
		MinTopupToman             int64             `json:"min_topup_toman"`
		UnapprovedTrialDailyLimit int               `json:"unapproved_trial_daily_limit"`
		Features                  map[string]bool   `json:"features"`
		Text                      map[string]string `json:"text"`
	} `json:"settings"`
	Panel struct {
		ID              string `json:"id"`
		BaseURL         string `json:"base_url"`
		TokenConfigured bool   `json:"token_configured"`
	} `json:"panel"`
}
type publicFeatures struct {
	Features map[string]bool   `json:"features"`
	Text     map[string]string `json:"text"`
}
type botApp struct {
	api    *backend.Client
	mu     sync.Mutex
	states map[int64]conversation
}

func Run() error { return run() }
func run() error {
	if raw := strings.TrimSpace(os.Getenv("ADMIN_TELEGRAM_ID")); raw != "" {
		id, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || id <= 0 {
			return fmt.Errorf("ADMIN_TELEGRAM_ID must be a positive integer")
		}
		adminTelegramID = id
	}
	token := strings.TrimSpace(os.Getenv("TELEGRAM_BOT_TOKEN"))
	base := strings.TrimSpace(os.Getenv("BACKEND_URL"))
	secret := os.Getenv("BACKEND_TOKEN")
	if token == "" {
		return fmt.Errorf("TELEGRAM_BOT_TOKEN is required")
	}
	timeout := 20 * time.Second
	if raw := os.Getenv("BACKEND_TIMEOUT"); raw != "" {
		if d, err := time.ParseDuration(raw); err == nil {
			timeout = d
		}
	}
	api, err := backend.New(base, secret, timeout)
	if err != nil {
		return err
	}
	b, err := telebot.NewBot(telebot.Settings{Token: token, Poller: &telebot.LongPoller{Timeout: 10 * time.Second}})
	if err != nil {
		return err
	}
	app := &botApp{api: api, states: make(map[int64]conversation)}
	app.register(b)
	// A blank BotFather menu removes stale commands previously published by
	// older builds. Users enter through Telegram's Start affordance or buttons.
	if err := b.DeleteCommands(); err != nil {
		log.Printf("could not clear Telegram command menu: %v", err)
	}
	done := make(chan struct{})
	go func() { b.Start(); close(done) }()
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	select {
	case <-signals:
		b.Stop()
		<-done
	case <-done:
	}
	return nil
}
func (a *botApp) register(b *telebot.Bot) {
	b.Handle("/start", a.start)
	b.Handle("/admin", a.adminCommand)
	registerCallbackRoutes(b, a.callback)
	b.Handle(telebot.OnText, a.text)
	b.Handle(telebot.OnPhoto, a.photo)
}

func registerCallbackRoutes(b *telebot.Bot, handler telebot.HandlerFunc) {
	// Register the button endpoint so telebot decodes its unique and payload
	// before invoking the handler. OnCallback remains a fallback for unknown
	// callback payloads, which are rejected as stale in callback().
	b.Handle(&telebot.Btn{Unique: "nav"}, handler)
	b.Handle(telebot.OnCallback, handler)
}

func newNonce() string {
	var data [5]byte
	if _, err := rand.Read(data[:]); err != nil {
		return fmt.Sprintf("%x", time.Now().UnixNano())
	}
	return hex.EncodeToString(data[:])
}
func (a *botApp) state(user int64) conversation {
	a.mu.Lock()
	defer a.mu.Unlock()
	st := a.states[user]
	if st.Nonce == "" || time.Since(st.Updated) > 30*time.Minute {
		st = conversation{Nonce: newNonce(), Updated: time.Now()}
		a.states[user] = st
	}
	return st
}
func (a *botApp) setState(user int64, st conversation) {
	a.mu.Lock()
	defer a.mu.Unlock()
	cutoff := time.Now().Add(-30 * time.Minute)
	for id, old := range a.states {
		if old.Updated.Before(cutoff) {
			delete(a.states, id)
		}
	}
	st.Nonce = newNonce()
	st.Updated = time.Now()
	a.states[user] = st
}
func (a *botApp) clearState(user int64) {
	a.mu.Lock()
	delete(a.states, user)
	a.mu.Unlock()
}
func (a *botApp) consumeCallbackState(user int64, nonce string) (conversation, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	st, ok := a.states[user]
	if !ok || nonce == "" || st.Nonce != nonce || time.Since(st.Updated) > 30*time.Minute {
		return conversation{}, false
	}
	// Rotate the stored nonce atomically before routing. Duplicate callback
	// deliveries and concurrent taps can therefore consume an action only once.
	consumed := st
	st.Nonce = newNonce()
	st.Updated = time.Now()
	a.states[user] = st
	return consumed, true
}
func (a *botApp) next(st conversation) conversation {
	st.Nonce = newNonce()
	st.Updated = time.Now()
	return st
}
func (a *botApp) resolve(c telebot.Context) (actor, error) {
	id := c.Sender().ID
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var out actor
	err := a.api.Call(ctx, "POST", "/v1/actors/resolve", 0, map[string]any{"telegram_id": id}, &out)
	return out, err
}
func (a *botApp) start(c telebot.Context) error {
	a.clearState(c.Sender().ID)
	act, err := a.resolve(c)
	if err != nil {
		return sendFailure(c, err)
	}
	if resumed, resumeErr := a.resumeReceiptMenu(c, act); resumeErr == nil && resumed {
		return nil
	} else if resumeErr != nil {
		log.Printf("active receipt lookup failed: category=backend")
	}
	return a.home(c, "👋 به پنل کاربری خوش آمدید\nسرویس وی‌پی‌ان خود را مدیریت کنید یا سرویس جدید خریداری نمایید.", false)
}

const receiptMenuPageSize = 6
const receiptHistoryLimit = 50
const adminReceiptMenuPageSize = 8

func activeReceiptPath(path string, before int64) string {
	if before > 0 {
		return fmt.Sprintf("%s?before_id=%d", path, before)
	}
	return path
}

func (a *botApp) activeReceipts(c telebot.Context, act actor, beforePayment, beforeTopup int64) (activeReceiptPage, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var page activeReceiptPage
	if beforePayment != -1 {
		var payment activePaymentIntentResponse
		if err := a.api.Call(ctx, "GET", activeReceiptPath("/v1/payment-intents/active", beforePayment), act.TelegramID, nil, &payment); err != nil {
			return activeReceiptPage{}, err
		}
		page.Payments = payment.PaymentIntents
		if page.Payments == nil && payment.PaymentIntent != nil {
			page.Payments = []*activeReceiptRequest{payment.PaymentIntent}
		}
		page.PaymentNext = payment.NextCursor
	}
	if beforeTopup != -1 {
		var topup activeTopupResponse
		if err := a.api.Call(ctx, "GET", activeReceiptPath("/v1/wallet/topups/active", beforeTopup), act.TelegramID, nil, &topup); err != nil {
			return activeReceiptPage{}, err
		}
		page.Topups = topup.Topups
		if page.Topups == nil && topup.Topup != nil {
			page.Topups = []*activeReceiptRequest{topup.Topup}
		}
		page.TopupNext = topup.NextCursor
	}
	return page, nil
}

func (a *botApp) resumeReceiptMenu(c telebot.Context, act actor) (bool, error) {
	return a.showActiveReceiptPage(c, act, receiptPageCursor{}, nil, "", false)
}

func nextReceiptCursor(items []*activeReceiptRequest, backendNext *int64) int64 {
	if len(items) > receiptMenuPageSize {
		return items[receiptMenuPageSize-1].ID
	}
	if backendNext != nil && *backendNext > 0 {
		return *backendNext
	}
	return -1
}

func appendReceiptRows(m *telebot.ReplyMarkup, rows *[]telebot.Row, st conversation, kind string, items []*activeReceiptRequest, text *strings.Builder) {
	label := "پرداخت مستقیم"
	action := "resume-payment"
	if kind == "topup" {
		label = "شارژ کیف پول"
		action = "resume-topup"
	}
	if len(items) == 0 {
		return
	}
	fmt.Fprintf(text, "%s‌های باز:\n", label)
	limit := len(items)
	if limit > receiptMenuPageSize {
		limit = receiptMenuPageSize
	}
	for _, item := range items[:limit] {
		if item == nil {
			continue
		}
		if item.Status == "awaiting_receipt" {
			fmt.Fprintf(text, "#%d — %s تومان، منتظر رسید\n", item.ID, formatToman(item.Amount))
			*rows = append(*rows, m.Row(m.Data(fmt.Sprintf("📷 ارسال رسید %s #%d", label, item.ID), "nav", st.Nonce, action, strconv.FormatInt(item.ID, 10))))
		} else if item.Status == "receipt_submitted" {
			fmt.Fprintf(text, "#%d — %s تومان، رسید در انتظار بررسی\n", item.ID, formatToman(item.Amount))
		}
	}
}

func (a *botApp) showActiveReceiptPage(c telebot.Context, act actor, cursor receiptPageCursor, history []receiptPageCursor, photoFileID string, edit bool) (bool, error) {
	page, err := a.activeReceipts(c, act, cursor.Payment, cursor.Topup)
	if err != nil {
		return false, err
	}
	if len(page.Payments) == 0 && len(page.Topups) == 0 && page.PaymentNext == nil && page.TopupNext == nil {
		return false, nil
	}
	paymentNext := nextReceiptCursor(page.Payments, page.PaymentNext)
	topupNext := nextReceiptCursor(page.Topups, page.TopupNext)
	state := conversation{
		ReceiptPhotoFileID:       photoFileID,
		ReceiptPagePaymentBefore: cursor.Payment,
		ReceiptPageTopupBefore:   cursor.Topup,
		ReceiptPagePaymentNext:   paymentNext,
		ReceiptPageTopupNext:     topupNext,
		ReceiptPageHistory:       append([]receiptPageCursor(nil), history...),
	}
	a.setState(c.Sender().ID, state)
	st := a.state(c.Sender().ID)
	m := &telebot.ReplyMarkup{}
	var rows []telebot.Row
	var text strings.Builder
	if cursor.Payment > 0 || cursor.Topup > 0 {
		text.WriteString("درخواست‌های قدیمی‌تر:\n")
	}
	appendReceiptRows(m, &rows, st, "payment", page.Payments, &text)
	appendReceiptRows(m, &rows, st, "topup", page.Topups, &text)
	if len(rows) == 0 && paymentNext == -1 && topupNext == -1 {
		return false, nil
	}
	if paymentNext != -1 || topupNext != -1 {
		rows = append(rows, m.Row(m.Data("درخواست‌های قدیمی‌تر »", "nav", st.Nonce, "receipt-page-next")))
	}
	if len(history) > 0 {
		rows = append(rows, m.Row(m.Data("« صفحه قبلی", "nav", st.Nonce, "receipt-page-prev")))
	}
	rows = append(rows, m.Row(m.Data("🏠 خانه", "nav", st.Nonce, "home")))
	m.Inline(rows...)
	message := strings.TrimSpace(text.String())
	if photoFileID != "" {
		message = "عکس رسید دریافت شد. درخواست درست را انتخاب کنید: \n" + message
	} else {
		message = "درخواست‌های پرداخت و شارژ شما:\n" + message
	}
	if edit {
		return true, c.Edit(message, m)
	}
	return true, c.Send(message, m)
}

func (a *botApp) resumeReceipt(c telebot.Context, kind string, args []string, st conversation) error {
	if len(args) != 1 {
		return a.freshHome(c, "درخواست رسید نامعتبر است.")
	}
	id, err := strconv.ParseInt(args[0], 10, 64)
	if err != nil || id <= 0 {
		return a.freshHome(c, "درخواست رسید نامعتبر است.")
	}
	act, err := a.resolve(c)
	if err != nil {
		return sendFailure(c, err)
	}
	page, err := a.activeReceipts(c, act, st.ReceiptPagePaymentBefore, st.ReceiptPageTopupBefore)
	if err != nil {
		return sendFailure(c, err)
	}
	var active *activeReceiptRequest
	items := page.Payments
	if kind == "topup" {
		items = page.Topups
	}
	for _, item := range items {
		if item != nil && item.ID == id {
			active = item
			break
		}
	}
	if active == nil || active.Status != "awaiting_receipt" {
		return a.freshHome(c, "این درخواست دیگر منتظر رسید نیست.")
	}
	a.setState(c.Sender().ID, conversation{Receipt: &receiptState{Kind: kind, ID: id}, Step: "receipt-photo"})
	if st.ReceiptPhotoFileID != "" {
		return a.submitReceipt(c, receiptState{Kind: kind, ID: id}, st.ReceiptPhotoFileID)
	}
	return a.prompt(c, "عکس رسید را ارسال کنید.", true)
}

func (a *botApp) changeReceiptPage(c telebot.Context, st conversation, next bool) error {
	cursor := receiptPageCursor{Payment: st.ReceiptPagePaymentBefore, Topup: st.ReceiptPageTopupBefore}
	history := append([]receiptPageCursor(nil), st.ReceiptPageHistory...)
	if next {
		if st.ReceiptPagePaymentNext == -1 && st.ReceiptPageTopupNext == -1 {
			return a.freshHome(c, "درخواست قدیمی‌تری پیدا نشد.")
		}
		if len(history) == receiptHistoryLimit {
			history = history[1:]
		}
		history = append(history, cursor)
		cursor = receiptPageCursor{Payment: st.ReceiptPagePaymentNext, Topup: st.ReceiptPageTopupNext}
	} else {
		if len(history) == 0 {
			return a.freshHome(c, "صفحه قبلی پیدا نشد.")
		}
		cursor = history[len(history)-1]
		history = history[:len(history)-1]
	}
	act, err := a.resolve(c)
	if err != nil {
		return sendFailure(c, err)
	}
	shown, err := a.showActiveReceiptPage(c, act, cursor, history, st.ReceiptPhotoFileID, true)
	if err != nil {
		return sendFailure(c, err)
	}
	if !shown {
		return a.freshHome(c, "درخواست فعالی برای ارسال رسید پیدا نشد.")
	}
	return nil
}

func (a *botApp) adminCommand(c telebot.Context) error {
	if !adminCommandSender(c.Sender(), c.Chat()) {
		return c.Send("دسترسی مجاز نیست.")
	}
	a.setState(c.Sender().ID, conversation{})
	return a.adminHome(c, false)
}
func adminCommandSender(sender *telebot.User, chat *telebot.Chat) bool {
	return sender != nil && sender.ID == adminTelegramID && chat != nil && chat.Type == telebot.ChatPrivate
}
func isNavCallback(cb *telebot.Callback) bool {
	return cb != nil && cb.Unique == "nav"
}
func keepsAdminReceiptReview(action string) bool {
	switch action {
	case "approve-payment", "approve-topup", "reject-payment", "reject-topup", "confirm-reject-payment", "confirm-reject-topup":
		return true
	default:
		return false
	}
}
func (a *botApp) home(c telebot.Context, message string, edit bool) error {
	st := a.next(conversation{})
	a.setState(c.Sender().ID, st)
	text, m, err := a.homeView(c, message)
	if err != nil {
		return sendFailure(c, err)
	}
	return present(c, text, m, edit)
}
func (a *botApp) homeView(c telebot.Context, message string) (string, *telebot.ReplyMarkup, error) {
	st := a.state(c.Sender().ID)
	act, err := a.resolve(c)
	if err != nil {
		return "", nil, err
	}
	features, err := a.getFeaturesForActor(act)
	if err != nil {
		return "", nil, err
	}
	m := &telebot.ReplyMarkup{}
	rows := make([]telebot.Row, 0, 4)
	if featureEnabled(features, "purchases_enabled") {
		buy := m.Data(menuText(features, "menu_purchases", "💼 خرید سرویس"), "nav", st.Nonce, "plans-paid")
		if featureEnabled(features, "trials_enabled") {
			rows = append(rows, m.Row(m.Data(menuText(features, "menu_trials", "🧪 تست رایگان"), "nav", st.Nonce, "plans-test"), buy))
		} else {
			rows = append(rows, m.Row(buy))
		}
	} else if featureEnabled(features, "trials_enabled") {
		rows = append(rows, m.Row(m.Data(menuText(features, "menu_trials", "🧪 تست رایگان"), "nav", st.Nonce, "plans-test")))
	}
	service := m.Data(menuText(features, "menu_services", "📋 سرویس‌های من"), "nav", st.Nonce, "services")
	if featureEnabled(features, "wallet_enabled") {
		rows = append(rows, m.Row(service, m.Data(menuText(features, "menu_wallet", "👛 کیف پول"), "nav", st.Nonce, "wallet")))
	} else {
		rows = append(rows, m.Row(service))
	}
	rows = append(rows, m.Row(m.Data(menuText(features, "menu_support", "🆘 پشتیبانی"), "nav", st.Nonce, "support")))
	if act.Role == "admin" && adminCommandSender(c.Sender(), c.Chat()) {
		rows = append(rows, m.Row(m.Data("⚙️ مدیریت", "nav", st.Nonce, "admin")))
	}
	m.Inline(rows...)
	if custom := strings.TrimSpace(features.Text["welcome"]); custom != "" && (message == "صفحه اصلی" || strings.Contains(message, "به پنل کاربری خوش آمدید")) {
		message = custom
	}
	return message, m, nil
}
func (a *botApp) freshHome(c telebot.Context, message string) error {
	a.setState(c.Sender().ID, conversation{})
	text, m, err := a.homeView(c, message)
	if err != nil {
		st := a.state(c.Sender().ID)
		m = &telebot.ReplyMarkup{}
		m.Inline(m.Row(m.Data("🔄 تلاش دوباره", "nav", st.Nonce, "home")))
		text = "منو در دسترس نیست. برای بارگذاری دوباره دکمه زیر را بزنید."
	}
	if err := c.Edit(text, m); err == nil {
		return nil
	}
	if c.Callback() != nil {
		if msg := c.Message(); msg != nil {
			_, _ = c.Bot().EditReplyMarkup(msg, &telebot.ReplyMarkup{})
		}
	}
	return c.Send(text, m)
}

func (a *botApp) callbackFailure(c telebot.Context, action string, st conversation) error {
	if (action == "method-wallet" || action == "method-direct" || action == "retry-purchase") && st.QuoteID > 0 {
		st.Step = ""
		st = a.next(st)
		a.setState(c.Sender().ID, st)
		st = a.state(c.Sender().ID)
		m := &telebot.ReplyMarkup{}
		m.Inline(m.Row(m.Data("🔁 همان پرداخت را دوباره بررسی کنید", "nav", st.Nonce, "retry-purchase")), m.Row(m.Data("🏠 خانه", "nav", st.Nonce, "home")))
		return present(c, "پرداخت تأیید نشد. اگر پاسخ backend نامشخص مانده باشد، تلاش دوباره با همان شناسه فقط یک سفارش ثبت می‌کند.", m, true)
	}
	return a.freshHome(c, "درخواست انجام نشد. از منوی تازه دوباره تلاش کنید.")
}
func (a *botApp) getFeatures(c telebot.Context) (publicFeatures, error) {
	act, err := a.resolve(c)
	if err != nil {
		return publicFeatures{}, err
	}
	return a.getFeaturesForActor(act)
}
func (a *botApp) getFeaturesForActor(act actor) (publicFeatures, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var out publicFeatures
	err := a.api.Call(ctx, "GET", "/v1/features", act.TelegramID, nil, &out)
	return out, err
}
func featureEnabled(features publicFeatures, key string) bool {
	value, ok := features.Features[key]
	return !ok || value
}
func menuText(features publicFeatures, key, fallback string) string {
	if text := strings.TrimSpace(features.Text[key]); text != "" {
		return text
	}
	return fallback
}
func present(c telebot.Context, message string, markup *telebot.ReplyMarkup, edit bool) error {
	if edit {
		return c.Edit(message, markup)
	}
	return c.Send(message, markup)
}
func (a *botApp) callback(c telebot.Context) error {
	cb := c.Callback()
	if cb == nil || c.Sender() == nil {
		return nil
	}
	c.Set("retail_bot_app", a)
	if !isNavCallback(cb) {
		if err := c.Respond(&telebot.CallbackResponse{Text: "این گزینه منقضی شد.", ShowAlert: true}); err != nil {
			log.Printf("callback acknowledgement failed")
		}
		return a.freshHome(c, "از منوی تازه ادامه دهید.")
	}
	fields := strings.Split(c.Data(), "|")
	if len(fields) < 2 {
		if err := c.Respond(&telebot.CallbackResponse{Text: "این گزینه منقضی شد.", ShowAlert: true}); err != nil {
			log.Printf("callback acknowledgement failed")
		}
		return a.freshHome(c, "از منوی تازه ادامه دهید.")
	}
	st, ok := a.consumeCallbackState(c.Sender().ID, fields[0])
	if !ok {
		if err := c.Respond(&telebot.CallbackResponse{Text: "صفحه به‌روز شد.", ShowAlert: true}); err != nil {
			log.Printf("callback acknowledgement failed")
		}
		return a.freshHome(c, "این دکمه منقضی شده بود. از منوی تازه ادامه دهید.")
	}
	if err := c.Respond(); err != nil {
		log.Printf("callback acknowledgement failed: %v", err)
	}
	command, args := fields[1], fields[2:]
	if !keepsAdminReceiptReview(command) {
		st.AdminReceiptKind = ""
		st.AdminReceiptID = 0
		a.setState(c.Sender().ID, st)
		st = a.state(c.Sender().ID)
	}
	c.Set("retail_callback_action", command)
	c.Set("retail_callback_state", st)
	if err := a.route(c, command, args, st); err != nil {
		status, category := failureDiagnostic(err)
		log.Printf("callback route failed: category=%s status=%d", category, status)
		return a.callbackFailure(c, command, st)
	}
	return nil
}
func (a *botApp) route(c telebot.Context, action string, args []string, st conversation) error {
	switch action {
	case "home":
		return a.home(c, "صفحه اصلی", true)
	case "plans-paid":
		features, e := a.getFeatures(c)
		if e != nil {
			return sendFailure(c, e)
		}
		if !featureEnabled(features, "purchases_enabled") {
			return a.home(c, "خرید در حال حاضر غیرفعال است.", true)
		}
		return a.showPlans(c, "paid", true)
	case "plans-test":
		features, e := a.getFeatures(c)
		if e != nil {
			return sendFailure(c, e)
		}
		if !featureEnabled(features, "trials_enabled") {
			return a.home(c, "تست در حال حاضر غیرفعال است.", true)
		}
		return a.showPlans(c, "test", true)
	case "select-paid", "select-test":
		if len(args) != 1 {
			return a.home(c, "درخواست نامعتبر است.", true)
		}
		id, err := strconv.ParseInt(args[0], 10, 64)
		if err != nil || id <= 0 {
			return a.home(c, "طرح نامعتبر است.", true)
		}
		if action == "select-test" {
			return a.createTrial(c, id)
		}
		st.PlanID = id
		st.OperationKey = callbackOperationKey(c, "purchase", strconv.FormatInt(id, 10))
		return a.showPurchaseDuration(c, st, true)
	case "purchase-months":
		if len(args) != 1 {
			return a.home(c, "مدت اشتراک نامعتبر است.", true)
		}
		n, e := strconv.Atoi(args[0])
		if e != nil || n < 1 || n > 36 {
			return a.home(c, "مدت اشتراک نامعتبر است.", true)
		}
		st.Months = n
		return a.afterPurchaseDuration(c, st, true)
	case "purchase-data":
		if len(args) != 1 {
			return a.home(c, "حجم انتخاب شده نامعتبر است.", true)
		}
		n, e := strconv.Atoi(args[0])
		if e != nil || n < 0 || n > 100000 {
			return a.home(c, "حجم انتخاب شده نامعتبر است.", true)
		}
		st.DataGB = n
		return a.showPurchaseIP(c, st, true)
	case "purchase-ip":
		if len(args) != 1 {
			return a.home(c, "محدودیت IP نامعتبر است.", true)
		}
		n, e := strconv.Atoi(args[0])
		if e != nil || n < 0 || n > 100 {
			return a.home(c, "محدودیت IP نامعتبر است.", true)
		}
		st.IPLimit = n
		st.Step = "purchase-name"
		st = a.next(st)
		a.setState(c.Sender().ID, st)
		m := &telebot.ReplyMarkup{}
		m.Inline(m.Row(m.Data("🎲 نام پیش‌فرض", "nav", st.Nonce, "purchase-default-name")), m.Row(m.Data("« بازگشت", "nav", st.Nonce, "purchase-ip-back")))
		return present(c, "نام دلخواه سرویس را بفرستید یا نام پیش‌فرض را انتخاب کنید.", m, true)
	case "purchase-default-name":
		st.Name = ""
		return a.showInvoice(c, st, true)
	case "purchase-ip-back":
		return a.showPurchaseIP(c, st, true)
	case "purchase-plan-back":
		return a.showPlans(c, "paid", true)
	case "purchase-duration-custom":
		st.Step = "purchase-months"
		st = a.next(st)
		a.setState(c.Sender().ID, st)
		return a.prompt(c, "لطفاً تعداد ماه را بفرستید (۱ تا ۳۶).", true)
	case "purchase-data-custom":
		st.Step = "purchase-gb"
		st = a.next(st)
		a.setState(c.Sender().ID, st)
		return a.prompt(c, "لطفاً حجم مورد نظر را به GB بفرستید.", true)
	case "purchase-ip-custom":
		st.Step = "purchase-ip"
		st = a.next(st)
		a.setState(c.Sender().ID, st)
		return a.prompt(c, "تعداد IP هم‌زمان را وارد کنید.", true)
	case "support":
		features, e := a.getFeatures(c)
		if e != nil {
			return sendFailure(c, e)
		}
		text := strings.TrimSpace(features.Text["support"])
		if text == "" {
			text = "برای پشتیبانی لطفاً با مدیر ربات در ارتباط باشید."
		}
		m := &telebot.ReplyMarkup{}
		next := a.state(c.Sender().ID)
		m.Inline(m.Row(m.Data("« بازگشت", "nav", next.Nonce, "home")))
		return present(c, text, m, true)
	case "method-wallet", "method-direct":
		if st.QuoteID <= 0 {
			return a.freshHome(c, "فاکتور منقضی شده است. خرید را دوباره از منو آغاز کنید.")
		}
		features, e := a.getFeatures(c)
		if e != nil {
			return sendFailure(c, e)
		}
		if !featureEnabled(features, "purchases_enabled") {
			return a.home(c, "خرید در حال حاضر غیرفعال است.", true)
		}
		if action == "method-wallet" && !featureEnabled(features, "wallet_enabled") {
			return a.home(c, "پرداخت با کیف پول غیرفعال است.", true)
		}
		if action == "method-direct" && !featureEnabled(features, "direct_payments_enabled") {
			return a.home(c, "پرداخت مستقیم غیرفعال است.", true)
		}
		st.Method = "wallet"
		if action == "method-direct" {
			st.Method = "direct"
		}
		c.Set("retail_callback_state", st)
		return a.completePurchase(c, st)
	case "retry-purchase":
		if st.QuoteID <= 0 || (st.Method != "wallet" && st.Method != "direct") {
			return a.freshHome(c, "فاکتور منقضی شده است. خرید را دوباره آغاز کنید.")
		}
		return a.completePurchase(c, st)
	case "topup":
		features, e := a.getFeatures(c)
		if e != nil {
			return sendFailure(c, e)
		}
		if !featureEnabled(features, "topups_enabled") {
			return a.home(c, "شارژ کیف پول در حال حاضر غیرفعال است.", true)
		}
		st.Step = "topup-amount"
		st.OperationKey = callbackOperationKey(c, "topup", "")
		st = a.next(st)
		a.setState(c.Sender().ID, st)
		return a.prompt(c, "مبلغ شارژ را به تومان بفرستید.", true)
	case "wallet":
		features, e := a.getFeatures(c)
		if e != nil {
			return sendFailure(c, e)
		}
		if !featureEnabled(features, "wallet_enabled") {
			return a.home(c, "کیف پول در حال حاضر غیرفعال است.", true)
		}
		return a.wallet(c, true)
	case "ledger":
		features, e := a.getFeatures(c)
		if e != nil {
			return sendFailure(c, e)
		}
		if !featureEnabled(features, "wallet_enabled") {
			return a.home(c, "کیف پول در حال حاضر غیرفعال است.", true)
		}
		return a.ledger(c, true)
	case "services":
		return a.services(c, true)
	case "services-page":
		if len(args) != 1 {
			return a.services(c, true)
		}
		offset, e := strconv.Atoi(args[0])
		if e != nil || offset < 0 {
			return a.services(c, true)
		}
		return a.servicesPage(c, true, offset)
	case "services-view":
		if len(args) != 1 {
			return a.services(c, true)
		}
		id, e := strconv.ParseInt(args[0], 10, 64)
		if e != nil || id <= 0 {
			return a.services(c, true)
		}
		return a.serviceDetail(c, id, true)
	case "cancel":
		if len(args) != 1 {
			return a.home(c, "درخواست نامعتبر است.", true)
		}
		id, e := strconv.ParseInt(args[0], 10, 64)
		if e != nil || id <= 0 {
			return a.home(c, "اشتراک نامعتبر است.", true)
		}
		return a.cancelSubscription(c, id)
	case "refund-request":
		if len(args) != 1 {
			return a.services(c, true)
		}
		id, e := strconv.ParseInt(args[0], 10, 64)
		if e != nil || id <= 0 {
			return a.services(c, true)
		}
		st.RefundSubID = id
		st.OperationKey = callbackOperationKey(c, "refund-request", strconv.FormatInt(id, 10))
		st.Step = "refund-reason"
		st = a.next(st)
		a.setState(c.Sender().ID, st)
		return a.prompt(c, "دلیل درخواست بازپرداخت را بنویسید. مبلغ نهایی و تأیید پس از بررسی شرایط خرید توسط مدیریت انجام می‌شود.", true)
	case "receipt-payment", "receipt-topup":
		if len(args) != 1 {
			return a.home(c, "شناسه نامعتبر است.", true)
		}
		id, e := strconv.ParseInt(args[0], 10, 64)
		if e != nil || id <= 0 {
			return a.home(c, "شناسه نامعتبر است.", true)
		}
		kind := "payment"
		if action == "receipt-topup" {
			kind = "topup"
		}
		st.Receipt = &receiptState{Kind: kind, ID: id}
		st.Step = "receipt-photo"
		st = a.next(st)
		a.setState(c.Sender().ID, st)
		return a.prompt(c, "عکس رسید را ارسال کنید.", true)
	case "resume-payment", "resume-topup":
		kind := "payment"
		if action == "resume-topup" {
			kind = "topup"
		}
		return a.resumeReceipt(c, kind, args, st)
	case "receipt-page-next":
		return a.changeReceiptPage(c, st, true)
	case "receipt-page-prev":
		return a.changeReceiptPage(c, st, false)
	case "admin":
		return a.adminHome(c, true)
	case "pending-payments":
		return a.pending(c, true)
	case "pending-topups":
		return a.pendingTopups(c, true)
	case "view-payment-receipt", "view-topup-receipt":
		kind := "payment"
		if action == "view-topup-receipt" {
			kind = "topup"
		}
		return a.viewPendingReceipt(c, kind, args, st)
	case "pending-payments-next", "pending-payments-prev":
		return a.changePendingReceiptPage(c, "payment", action == "pending-payments-next")
	case "pending-topups-next", "pending-topups-prev":
		return a.changePendingReceiptPage(c, "topup", action == "pending-topups-next")
	case "reject-payment", "reject-topup", "reject-refund":
		kind := strings.TrimPrefix(action, "reject-")
		return a.confirmReject(c, kind, args, st)
	case "confirm-reject-payment", "confirm-reject-topup", "confirm-reject-refund":
		kind := strings.TrimPrefix(action, "confirm-reject-")
		return a.rejectAdminItem(c, kind, args, true, st)
	case "cancel-reject-payment":
		return a.pending(c, true)
	case "cancel-reject-topup":
		return a.pendingTopups(c, true)
	case "cancel-reject-refund":
		if len(args) == 1 {
			id, e := strconv.ParseInt(args[0], 10, 64)
			if e == nil && id > 0 {
				return a.reviewRefund(c, id, true)
			}
		}
		return a.pendingRefunds(c, true)
	case "pending-refunds":
		return a.pendingRefunds(c, true)
	case "review-refund":
		if len(args) != 1 {
			return a.pendingRefunds(c, true)
		}
		id, e := strconv.ParseInt(args[0], 10, 64)
		if e != nil || id <= 0 {
			return a.pendingRefunds(c, true)
		}
		return a.reviewRefund(c, id, true)
	case "approve-refund":
		if len(args) != 1 {
			return a.pendingRefunds(c, true)
		}
		id, e := strconv.ParseInt(args[0], 10, 64)
		if e != nil || id <= 0 {
			return a.pendingRefunds(c, true)
		}
		return a.approveRefund(c, id, true)
	case "admin-work-items":
		return a.adminWorkItems(c, true)
	case "approve-payment":
		return a.approveReviewedReceipt(c, "payment", args, st)
	case "approve-topup":
		return a.approveReviewedReceipt(c, "topup", args, st)
	case "config":
		return a.adminConfig(c, true)
	case "config-plans":
		return a.adminPlans(c, true)
	case "config-edit-plan", "config-new-plan":
		if action == "config-new-plan" {
			st.Admin = "new-plan-name"
			st = a.next(st)
			a.setState(c.Sender().ID, st)
			return a.prompt(c, "نام طرح جدید را ارسال کنید.", true)
		}
		var id int64
		if len(args) > 0 {
			id, _ = strconv.ParseInt(args[0], 10, 64)
		}
		return a.editPlanPrompt(c, id, true)
	case "pf":
		if len(args) != 2 {
			return a.adminPlans(c, true)
		}
		id, e := strconv.ParseInt(args[0], 10, 64)
		if e != nil || id <= 0 {
			return a.adminPlans(c, true)
		}
		field, ok := planFieldFromCode(args[1])
		if !ok {
			return a.adminPlans(c, true)
		}
		return a.editPlanField(c, id, field)
	case "pt":
		if len(args) != 2 {
			return a.adminPlans(c, true)
		}
		id, e := strconv.ParseInt(args[0], 10, 64)
		if e != nil || id <= 0 {
			return a.adminPlans(c, true)
		}
		field := "enabled"
		if args[1] == "l" {
			field = "is_limited"
		} else if args[1] != "e" {
			return a.adminPlans(c, true)
		}
		return a.togglePlan(c, id, field)
	case "new-plan-kind":
		if len(args) != 1 || st.Draft == nil || st.Draft.Name == "" || (args[0] != "paid" && args[0] != "test") {
			return a.adminPlans(c, true)
		}
		st.Draft.Kind = args[0]
		return a.createPlan(c, st.Draft)
	case "config-payment":
		return a.adminPayment(c, true)
	case "config-payment-field":
		if len(args) != 1 {
			return a.adminConfig(c, true)
		}
		st.Admin = "payment:" + args[0]
		st = a.next(st)
		a.setState(c.Sender().ID, st)
		return a.prompt(c, "مقدار جدید را ارسال کنید.", true)
	case "config-settings":
		return a.adminSettings(c, true)
	case "config-trial-days":
		st.Admin = "trial-days"
		st = a.next(st)
		a.setState(c.Sender().ID, st)
		return a.prompt(c, "تعداد روز بین تست‌های هر طرح را وارد کنید؛ صفر یا کمتر یعنی فقط یک‌بار.", true)
	case "config-min-topup":
		st.Admin = "min-topup"
		st = a.next(st)
		a.setState(c.Sender().ID, st)
		return a.prompt(c, "حداقل مبلغ شارژ را به تومان وارد کنید؛ صفر یعنی حداقل غیرفعال.", true)
	case "ft", "config-feature":
		if len(args) != 1 {
			return a.adminSettings(c, true)
		}
		return a.toggleFeature(c, args[0])
	case "config-text":
		if len(args) != 1 {
			return a.adminSettings(c, true)
		}
		st.Admin = "text:" + args[0]
		st = a.next(st)
		a.setState(c.Sender().ID, st)
		return a.prompt(c, "متن جدید را ارسال کنید.", true)
	case "tx":
		if len(args) != 1 {
			return a.adminSettings(c, true)
		}
		st.Admin = "text:" + args[0]
		st = a.next(st)
		a.setState(c.Sender().ID, st)
		return a.prompt(c, "متن جدید را ارسال کنید.", true)
	case "config-text-new":
		st.Admin = "text-key"
		st = a.next(st)
		a.setState(c.Sender().ID, st)
		return a.prompt(c, "کلید کوتاه انگلیسی برای متن جدید را وارد کنید.", true)
	case "config-panel":
		return a.adminPanel(c, true)
	case "config-panel-url":
		err := beginPanelURLPrompt(c.Chat(), func() {
			st.Admin = "panel-url"
			st = a.next(st)
			a.setState(c.Sender().ID, st)
		}, func() error {
			return a.prompt(c, "آدرس پایه پنل را وارد کنید.", true)
		})
		if errors.Is(err, errPanelPrivateChat) {
			return c.Send("تنظیم کلید پنل فقط در گفت‌وگوی خصوصی با ربات مجاز است.")
		}
		return err
	default:
		return a.home(c, "این گزینه در دسترس نیست.", true)
	}
}
func (a *botApp) prompt(c telebot.Context, text string, edit bool) error {
	m := &telebot.ReplyMarkup{}
	st := a.state(c.Sender().ID)
	m.Inline(m.Row(m.Data("↩️ خانه", "nav", st.Nonce, "home")))
	return present(c, text, m, edit)
}

func (a *botApp) planByID(c telebot.Context, id int64) (plan, error) {
	act, err := a.resolve(c)
	if err != nil {
		return plan{}, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var items []plan
	if err = a.api.Call(ctx, "GET", "/v1/plans?kind=paid", act.TelegramID, nil, &items); err != nil {
		return plan{}, err
	}
	for _, p := range items {
		if p.ID == id {
			return p, nil
		}
	}
	return plan{}, fmt.Errorf("selected plan is unavailable")
}

func (a *botApp) showPurchaseDuration(c telebot.Context, st conversation, edit bool) error {
	p, err := a.planByID(c, st.PlanID)
	if err != nil {
		return sendFailure(c, err)
	}
	st.Step = ""
	st = a.next(st)
	a.setState(c.Sender().ID, st)
	st = a.state(c.Sender().ID)
	m := &telebot.ReplyMarkup{}
	m.Inline(
		m.Row(m.Data("۱ ماهه", "nav", st.Nonce, "purchase-months", "1"), m.Data("۳ ماهه", "nav", st.Nonce, "purchase-months", "3")),
		m.Row(m.Data("۶ ماهه", "nav", st.Nonce, "purchase-months", "6"), m.Data("✏️ مدت دلخواه", "nav", st.Nonce, "purchase-duration-custom")),
		m.Row(m.Data("« بازگشت", "nav", st.Nonce, "purchase-plan-back")),
	)
	text := fmt.Sprintf("💼 طرح خرید سرویس\n\n📦 %s\nقیمت پایه: %s تومان در ماه\nتعداد IP هم‌زمان: %d تا %d\n\nمدت زمان سرویس را انتخاب کنید:", p.Name, numberLabel(p.BasePrice), p.BaseIP, p.MaxIP)
	if p.IsLimited {
		text = fmt.Sprintf("💼 طرح خرید سرویس\n\n📦 %s\nقیمت هر گیگابایت: %s تومان\nحداقل ترافیک: %d گیگابایت\nتعداد IP هم‌زمان: %d تا %d\n\nمدت زمان سرویس را انتخاب کنید:", p.Name, numberLabel(p.PriceGB), p.MinGB, p.BaseIP, p.MaxIP)
	}
	if p.Description != "" {
		text += "\n\n" + p.Description
	}
	if len(p.DiscountTiers) > 0 {
		text += "\n\n💰 تخفیف خرید بلندمدت:"
		for _, tier := range p.DiscountTiers {
			text += fmt.Sprintf("\n%d ماه به بالا: %s%%", tier.Months, formatBasisPoints(tier.BasisPoints))
		}
	}
	return present(c, text, m, edit)
}

func (a *botApp) afterPurchaseDuration(c telebot.Context, st conversation, edit bool) error {
	p, err := a.planByID(c, st.PlanID)
	if err != nil {
		return sendFailure(c, err)
	}
	if p.IsLimited {
		return a.showPurchaseData(c, st, p, edit)
	}
	return a.showPurchaseIP(c, st, edit)
}

func (a *botApp) showPurchaseData(c telebot.Context, st conversation, p plan, edit bool) error {
	st = a.next(st)
	a.setState(c.Sender().ID, st)
	st = a.state(c.Sender().ID)
	m := &telebot.ReplyMarkup{}
	min := p.MinGB
	if min < 1 {
		min = 1
	}
	m.Inline(
		m.Row(m.Data(fmt.Sprintf("%d گیگابایت", min), "nav", st.Nonce, "purchase-data", strconv.Itoa(min)), m.Data(fmt.Sprintf("%d گیگابایت", min+10), "nav", st.Nonce, "purchase-data", strconv.Itoa(min+10))),
		m.Row(m.Data(fmt.Sprintf("%d گیگابایت", min+30), "nav", st.Nonce, "purchase-data", strconv.Itoa(min+30)), m.Data(fmt.Sprintf("%d گیگابایت", min+50), "nav", st.Nonce, "purchase-data", strconv.Itoa(min+50))),
		m.Row(m.Data("✏️ حجم دلخواه", "nav", st.Nonce, "purchase-data-custom")),
		m.Row(m.Data("« بازگشت", "nav", st.Nonce, "plans-paid")),
	)
	return present(c, fmt.Sprintf("📦 %s — %d ماهه\nلطفاً ترافیک مورد نظر را انتخاب کنید (حداقل %d گیگابایت):", p.Name, st.Months, min), m, edit)
}

func (a *botApp) showPurchaseIP(c telebot.Context, st conversation, edit bool) error {
	p, err := a.planByID(c, st.PlanID)
	if err != nil {
		return sendFailure(c, err)
	}
	st = a.next(st)
	a.setState(c.Sender().ID, st)
	st = a.state(c.Sender().ID)
	m := &telebot.ReplyMarkup{}
	rows := make([]telebot.Row, 0, 10)
	for ip := p.BaseIP; ip <= p.MaxIP && len(rows) < 8; ip++ {
		label := fmt.Sprintf("%d IP هم‌زمان", ip)
		if ip == 0 {
			label = "IP هم‌زمان نامحدود"
		}
		rows = append(rows, m.Row(m.Data(label, "nav", st.Nonce, "purchase-ip", strconv.Itoa(ip))))
	}
	rows = append(rows, m.Row(m.Data("✏️ تعداد دلخواه", "nav", st.Nonce, "purchase-ip-custom")))
	rows = append(rows, m.Row(m.Data("« بازگشت", "nav", st.Nonce, "purchase-plan-back")))
	m.Inline(rows...)
	return present(c, fmt.Sprintf("📦 %s — %d ماهه\nتعداد IP هم‌زمان را انتخاب کنید:", p.Name, st.Months), m, edit)
}

func (a *botApp) showInvoice(c telebot.Context, st conversation, edit bool) error {
	features, err := a.getFeatures(c)
	if err != nil {
		return sendFailure(c, err)
	}
	act, err := a.resolve(c)
	if err != nil {
		return sendFailure(c, err)
	}
	if st.OperationKey == "" {
		st.OperationKey = callbackOperationKey(c, "purchase", strconv.FormatInt(st.PlanID, 10))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var q quote
	if err = a.api.Call(ctx, "POST", "/v1/quotes", act.TelegramID, map[string]any{"plan_id": st.PlanID, "months": st.Months, "ip_limit": st.IPLimit, "data_gb": st.DataGB, "idempotency_key": "quote-" + st.OperationKey}, &q); err != nil {
		return sendFailure(c, err)
	}
	if q.ID <= 0 {
		return a.freshHome(c, "پیش‌فاکتور ساخته نشد. لطفاً خرید را دوباره آغاز کنید.")
	}
	st.QuoteID, st.QuotePrice, st.Step = q.ID, q.Price, ""
	var balance int64
	if featureEnabled(features, "wallet_enabled") {
		var wallet map[string]any
		if err = a.api.Call(ctx, "GET", "/v1/wallet", act.TelegramID, nil, &wallet); err != nil {
			return sendFailure(c, err)
		}
		balance, _ = numericInt64(wallet["balance_toman"])
	}
	st = a.next(st)
	a.setState(c.Sender().ID, st)
	st = a.state(c.Sender().ID)
	m := &telebot.ReplyMarkup{}
	rows := make([]telebot.Row, 0, 3)
	if featureEnabled(features, "wallet_enabled") && balance >= q.Price {
		rows = append(rows, m.Row(m.Data("👛 پرداخت از کیف پول", "nav", st.Nonce, "method-wallet")))
	}
	if featureEnabled(features, "direct_payments_enabled") {
		rows = append(rows, m.Row(m.Data("💳 پرداخت مستقیم (کارت به کارت)", "nav", st.Nonce, "method-direct")))
	}
	rows = append(rows, m.Row(m.Data("« بازگشت", "nav", st.Nonce, "plans-paid")))
	m.Inline(rows...)
	name := strings.TrimSpace(st.Name)
	if name == "" {
		name = "نام پیش‌فرض ربات"
	}
	data := "نامحدود"
	if q.DataGB > 0 {
		data = fmt.Sprintf("%d گیگابایت", q.DataGB)
	}
	text := fmt.Sprintf("🧾 پیش‌فاکتور سرویس\n\nطرح: %s\nنام سرویس: %s\nمدت: %d ماه (%d روز)\nIP هم‌زمان: %d\nحجم: %s\n\nمبلغ کل: %s %s", q.PlanName, name, q.Months, q.DurationDays, q.IPLimit, data, formatToman(q.Price), q.Currency)
	if q.DiscountToman > 0 {
		text += fmt.Sprintf("\nتخفیف: %s %s", formatToman(q.DiscountToman), q.Currency)
	}
	if featureEnabled(features, "wallet_enabled") {
		text += fmt.Sprintf("\nموجودی کیف پول: %s تومان", formatToman(balance))
		if balance < q.Price {
			text += "\nموجودی برای این خرید کافی نیست."
		}
	}
	text += "\n\nروش پرداخت را برای تأیید سفارش انتخاب کنید:"
	return present(c, text, m, edit)
}

func numericInt64(value any) (int64, bool) {
	switch v := value.(type) {
	case float64:
		return int64(v), true
	case int64:
		return v, true
	case int:
		return int64(v), true
	case json.Number:
		n, err := v.Int64()
		return n, err == nil
	case string:
		n, err := strconv.ParseInt(v, 10, 64)
		return n, err == nil
	default:
		return 0, false
	}
}

func (a *botApp) showPlans(c telebot.Context, kind string, edit bool) error {
	act, err := a.resolve(c)
	if err != nil {
		return sendFailure(c, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var items []plan
	if err = a.api.Call(ctx, "GET", "/v1/plans?kind="+kind, act.TelegramID, nil, &items); err != nil {
		return sendFailure(c, err)
	}
	if len(items) == 0 {
		return a.home(c, "در حال حاضر طرحی در دسترس نیست.", true)
	}
	if len(items) > 12 {
		items = items[:12]
	}
	st := a.state(c.Sender().ID)
	m := &telebot.ReplyMarkup{}
	rows := make([]telebot.Row, 0, len(items)+1)
	var summary strings.Builder
	for _, p := range items {
		if kind == "test" {
			data := "نامحدود"
			if p.MaxBytes > 0 {
				data = fmt.Sprintf("%.2f گیگابایت", float64(p.MaxBytes)/1073741824)
			}
			fmt.Fprintf(&summary, "📦 %s\n⏱️ مدت اعتبار: %s (پس از اولین اتصال)\n📊 حجم مجاز: %s\n🔄 سهمیه هنگام ثبت درخواست بررسی می‌شود.\n", p.Name, humanDuration(p.ExpireSeconds), data)
			if p.UsageDescription != "" {
				fmt.Fprintf(&summary, "%s\n", p.UsageDescription)
			}
			if p.Description != "" {
				fmt.Fprintf(&summary, "%s\n", p.Description)
			}
			if len(p.DiscountTiers) > 0 {
				for _, tier := range p.DiscountTiers {
					fmt.Fprintf(&summary, "تخفیف %d ماهه به بالا: %s%%\n", tier.Months, formatBasisPoints(tier.BasisPoints))
				}
			}
			summary.WriteString("\n")
		} else if p.IsLimited {
			fmt.Fprintf(&summary, "📦 %s (محدود)\nقیمت هر گیگابایت: %s تومان\nحداقل ترافیک: %d گیگابایت\nماهانه اضافه: +%s تومان\nIP هم‌زمان: %d تا %d\n", p.Name, numberLabel(p.PriceGB), p.MinGB, numberLabel(p.PricePerExtraMonth), p.BaseIP, p.MaxIP)
		} else {
			fmt.Fprintf(&summary, "📦 %s (نامحدود)\nقیمت پایه: %s تومان در ماه\nIP هم‌زمان: %d تا %d\n", p.Name, numberLabel(p.BasePrice), p.BaseIP, p.MaxIP)
		}
		if kind == "paid" {
			fmt.Fprintf(&summary, "هزینه هر IP اضافه: +%s تومان در ماه\n", numberLabel(p.PricePerExtraIP))
		}
		if p.UsageDescription != "" && kind == "paid" {
			fmt.Fprintf(&summary, "%s\n", p.UsageDescription)
		}
		if p.Description != "" && kind == "paid" {
			fmt.Fprintf(&summary, "%s\n", p.Description)
		}
		if kind == "paid" {
			for _, tier := range p.DiscountTiers {
				fmt.Fprintf(&summary, "تخفیف %d ماهه به بالا: %s%%\n", tier.Months, formatBasisPoints(tier.BasisPoints))
			}
		}
		summary.WriteString("\n")
		label := p.Name
		if len([]rune(label)) > 28 {
			label = string([]rune(label)[:28])
		}
		actName := "select-paid"
		if kind == "test" {
			actName = "select-test"
		}
		rows = append(rows, m.Row(m.Data("📦 "+label, "nav", st.Nonce, actName, strconv.FormatInt(p.ID, 10))))
	}
	rows = append(rows, m.Row(m.Data("« بازگشت", "nav", st.Nonce, "home")))
	m.Inline(rows...)
	return present(c, strings.TrimSpace(summary.String()), m, edit)
}

func humanDuration(seconds int64) string {
	if seconds <= 0 {
		return "نامشخص"
	}
	days := seconds / 86400
	if days >= 30 && days%30 == 0 {
		return fmt.Sprintf("%d ماه (%d روز)", days/30, days)
	}
	if days > 0 {
		return fmt.Sprintf("%d روز", days)
	}
	hours := seconds / 3600
	if hours > 0 {
		return fmt.Sprintf("%d ساعت", hours)
	}
	return fmt.Sprintf("%d دقیقه", seconds/60)
}

func formatBasisPoints(points int) string {
	return fmt.Sprintf("%d.%02d", points/100, points%100)
}

func (a *botApp) createTrial(c telebot.Context, planID int64) error {
	act, err := a.resolve(c)
	if err != nil {
		return sendFailure(c, err)
	}
	key := callbackOperationKey(c, "trial", strconv.FormatInt(planID, 10))
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var out purchase
	if err = a.api.Call(ctx, "POST", "/v1/trials", act.TelegramID, map[string]any{"plan_id": planID, "idempotency_key": key}, &out); err != nil {
		return sendFailure(c, err)
	}
	return a.home(c, "درخواست تست به‌صورت پایدار ثبت شد؛ فعال‌سازی پس از تأیید پنل ادامه می‌یابد.", true)
}
func (a *botApp) text(c telebot.Context) error {
	if c.Message() == nil || c.Sender() == nil {
		return nil
	}
	st := a.state(c.Sender().ID)
	if st.Step == "" && st.Admin == "" {
		return c.Send("برای ادامه یکی از گزینه‌های منو را انتخاب کنید.")
	}
	value := strings.TrimSpace(c.Text())
	if value == "" {
		return c.Send("ورودی خالی است. لطفاً دوباره بفرستید.")
	}
	if st.Admin != "" {
		return a.adminTextInput(c, st, value)
	}
	switch st.Step {
	case "purchase-months":
		n, e := strconv.Atoi(value)
		if e != nil || n < 1 || n > 36 {
			return c.Send("مدت را به‌صورت عددی بین ۱ تا ۳۶ ماه بفرستید.")
		}
		st.Months = n
		return a.afterPurchaseDuration(c, st, false)
	case "purchase-ip":
		n, e := strconv.Atoi(value)
		if e != nil || n < 0 || n > 100 {
			return c.Send("تعداد IP معتبر نیست. عددی بین ۰ تا ۱۰۰ بفرستید.")
		}
		st.IPLimit = n
		st.Step = "purchase-name"
		st = a.next(st)
		a.setState(c.Sender().ID, st)
		return c.Send("نام دلخواه سرویس را بفرستید یا «-» را برای نام پیش‌فرض ارسال کنید.")
	case "purchase-gb":
		n, e := strconv.Atoi(value)
		if e != nil || n < 0 || n > 100000 {
			return c.Send("حجم نامعتبر است.")
		}
		st.DataGB = n
		return a.showPurchaseIP(c, st, false)
	case "purchase-name":
		if value != "-" {
			st.Name = value
		}
		return a.showInvoice(c, st, false)
	case "topup-amount":
		amount, e := strconv.ParseInt(value, 10, 64)
		if e != nil || amount <= 0 {
			return c.Send("مبلغ باید عدد صحیح مثبت به تومان باشد.")
		}
		return a.createTopup(c, amount)
	case "refund-reason":
		if len([]rune(value)) > 500 {
			return c.Send("دلیل درخواست حداکثر ۵۰۰ نویسه باشد.")
		}
		return a.createRefundRequest(c, st, value)
	default:
		return c.Send("این مرحله منقضی شده است. از منو دوباره شروع کنید.")
	}
}
func (a *botApp) completePurchase(c telebot.Context, st conversation) error {
	act, err := a.resolve(c)
	if err != nil {
		return sendFailure(c, err)
	}
	if st.QuoteID <= 0 {
		return a.freshHome(c, "پیش‌فاکتور منقضی شده است. لطفاً خرید را دوباره آغاز کنید.")
	}
	key := st.OperationKey
	if key == "" {
		key = stableKey(c.Sender().ID, c.Chat().ID, int64(c.Message().ID), "purchase")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var instruction map[string]any
	if st.Method == "direct" {
		if err = a.api.Call(ctx, "GET", "/v1/payment-instructions", act.TelegramID, nil, &instruction); err != nil {
			return sendFailure(c, err)
		}
		if !hasPaymentDestination(instruction) {
			return a.freshHome(c, "اطلاعات پرداخت مستقیم هنوز توسط مدیریت تنظیم نشده است.")
		}
	}
	var out purchase
	if err = a.api.Call(ctx, "POST", "/v1/purchases", act.TelegramID, map[string]any{"quote_id": st.QuoteID, "payment_method": st.Method, "idempotency_key": "purchase-" + key, "display_name": st.Name}, &out); err != nil {
		return sendFailure(c, err)
	}
	if st.Method != "direct" {
		return a.home(c, fmt.Sprintf("خرید ثبت شد. مبلغ %d تومان، وضعیت: %s.", out.Amount, out.Status), false)
	}
	r := &receiptState{Kind: "payment", ID: out.IntentID}
	st.Receipt = r
	st.Step = "receipt-photo"
	a.setState(c.Sender().ID, st)
	m := &telebot.ReplyMarkup{}
	s := a.state(c.Sender().ID)
	m.Inline(m.Row(m.Data("📷 ارسال عکس رسید", "nav", s.Nonce, "receipt-payment", strconv.FormatInt(out.IntentID, 10))), m.Row(m.Data("🏠 خانه", "nav", s.Nonce, "home")))
	text := fmt.Sprintf("فاکتور مستقیم شماره %d\nمبلغ: %s تومان\n\n%s\n\nپس از پرداخت، دکمه ارسال رسید را بزنید.", out.IntentID, formatToman(out.Amount), formatPaymentInstructions(instruction))
	return c.Send(text, m)
}
func (a *botApp) wallet(c telebot.Context, edit bool) error {
	act, err := a.resolve(c)
	if err != nil {
		return sendFailure(c, err)
	}
	var out map[string]any
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err = a.api.Call(ctx, "GET", "/v1/wallet", act.TelegramID, nil, &out); err != nil {
		return sendFailure(c, err)
	}
	st := a.state(c.Sender().ID)
	m := &telebot.ReplyMarkup{}
	rows := []telebot.Row{m.Row(m.Data("📥 شارژ کیف پول", "nav", st.Nonce, "topup"))}
	if c.Sender().ID == adminTelegramID && act.Role == "admin" {
		rows = append(rows, m.Row(m.Data("⏳ تراکنش‌های در انتظار شارژ", "nav", st.Nonce, "pending-topups")))
	}
	rows = append(rows, m.Row(m.Data("« بازگشت", "nav", st.Nonce, "home")))
	m.Inline(rows...)
	return present(c, fmt.Sprintf("👛 موجودی کیف پول شما: %s تومان", formatToman(out["balance_toman"])), m, edit)
}

func formatToman(value any) string {
	var n int64
	switch v := value.(type) {
	case float64:
		n = int64(v)
	case int64:
		n = v
	case int:
		n = int64(v)
	case json.Number:
		n, _ = v.Int64()
	case string:
		n, _ = strconv.ParseInt(v, 10, 64)
	default:
		return fmt.Sprint(value)
	}
	s := strconv.FormatInt(n, 10)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}
func (a *botApp) ledger(c telebot.Context, edit bool) error {
	act, err := a.resolve(c)
	if err != nil {
		return sendFailure(c, err)
	}
	var out []map[string]any
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err = a.api.Call(ctx, "GET", "/v1/wallet/ledger", act.TelegramID, nil, &out); err != nil {
		return sendFailure(c, err)
	}
	var b strings.Builder
	if len(out) == 0 {
		b.WriteString("تراکنشی ثبت نشده است.")
	} else {
		for _, row := range out {
			fmt.Fprintf(&b, "%v تومان | %v | %v\n", row["amount_toman"], row["type"], row["description"])
		}
	}
	st := a.state(c.Sender().ID)
	m := &telebot.ReplyMarkup{}
	m.Inline(m.Row(m.Data("👛 موجودی", "nav", st.Nonce, "wallet"), m.Data("🏠 خانه", "nav", st.Nonce, "home")))
	return present(c, strings.TrimSpace(b.String()), m, edit)
}
func (a *botApp) createTopup(c telebot.Context, amount int64) error {
	act, err := a.resolve(c)
	if err != nil {
		return sendFailure(c, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var instruction map[string]any
	if err = a.api.Call(ctx, "GET", "/v1/payment-instructions", act.TelegramID, nil, &instruction); err != nil {
		return sendFailure(c, err)
	}
	if !hasPaymentDestination(instruction) {
		return c.Send("اطلاعات پرداخت هنوز تنظیم نشده است. لطفاً با پشتیبانی تماس بگیرید.")
	}
	minimum, _ := numericInt64(instruction["min_topup_toman"])
	if minimum > 0 && amount < minimum {
		return c.Send(fmt.Sprintf("حداقل مبلغ شارژ %s تومان است. مبلغ را دوباره وارد کنید.", formatToman(minimum)))
	}
	var out map[string]any
	key := a.state(c.Sender().ID).OperationKey
	if key == "" {
		key = stableKey(c.Sender().ID, c.Chat().ID, int64(c.Message().ID), "topup")
	}
	if err = a.api.Call(ctx, "POST", "/v1/wallet/topups", act.TelegramID, map[string]any{"amount_toman": amount, "idempotency_key": key}, &out); err != nil {
		return sendFailure(c, err)
	}
	id := fmt.Sprint(out["topup_id"])
	parsed, _ := strconv.ParseInt(id, 10, 64)
	st := a.state(c.Sender().ID)
	st.Receipt = &receiptState{Kind: "topup", ID: parsed}
	st.Step = "receipt-photo"
	a.setState(c.Sender().ID, st)
	st = a.state(c.Sender().ID)
	m := &telebot.ReplyMarkup{}
	m.Inline(m.Row(m.Data("📷 ارسال عکس رسید", "nav", st.Nonce, "receipt-topup", id)), m.Row(m.Data("🏠 خانه", "nav", st.Nonce, "home")))
	text := fmt.Sprintf("درخواست شارژ شماره %s ثبت شد.\nمبلغ: %s تومان\n\n%s\n\nپس از واریز، عکس رسید را ارسال کنید.", id, formatToman(amount), formatPaymentInstructions(instruction))
	return c.Send(text, m)
}

func formatPaymentInstructions(instruction map[string]any) string {
	var b strings.Builder
	if card := strings.TrimSpace(fmt.Sprint(instruction["card_number"])); card != "" && card != "<nil>" {
		fmt.Fprintf(&b, "شماره کارت: %s\n", card)
	}
	if owner := strings.TrimSpace(fmt.Sprint(instruction["card_owner"])); owner != "" && owner != "<nil>" {
		fmt.Fprintf(&b, "صاحب کارت: %s\n", owner)
	}
	if details := strings.TrimSpace(fmt.Sprint(instruction["instructions"])); details != "" && details != "<nil>" {
		fmt.Fprintf(&b, "%s\n", details)
	}
	if minimum, ok := numericInt64(instruction["min_topup_toman"]); ok && minimum > 0 {
		fmt.Fprintf(&b, "حداقل شارژ: %s تومان\n", formatToman(minimum))
	} else {
		b.WriteString("حداقل شارژ: تعیین نشده\n")
	}
	if b.Len() == 0 {
		return "اطلاعات پرداخت تنظیم نشده است. با پشتیبانی تماس بگیرید."
	}
	return strings.TrimSpace(b.String())
}

func hasPaymentDestination(instruction map[string]any) bool {
	for _, key := range []string{"card_number", "card_owner", "instructions"} {
		value := strings.TrimSpace(fmt.Sprint(instruction[key]))
		if value != "" && value != "<nil>" {
			return true
		}
	}
	return false
}
func (a *botApp) photo(c telebot.Context) error {
	st := a.state(c.Sender().ID)
	msg := c.Message()
	if msg == nil || msg.Photo == nil {
		return c.Send("عکس رسید دریافت نشد.")
	}
	if st.Receipt == nil {
		act, err := a.resolve(c)
		if err != nil {
			return sendFailure(c, err)
		}
		page, err := a.activeReceipts(c, act, 0, 0)
		if err != nil {
			return sendFailure(c, err)
		}
		var awaiting []receiptState
		for _, item := range page.Payments {
			if item != nil && item.Status == "awaiting_receipt" {
				awaiting = append(awaiting, receiptState{Kind: "payment", ID: item.ID})
			}
		}
		for _, item := range page.Topups {
			if item != nil && item.Status == "awaiting_receipt" {
				awaiting = append(awaiting, receiptState{Kind: "topup", ID: item.ID})
			}
		}
		hasMore := page.PaymentNext != nil || page.TopupNext != nil
		if len(awaiting) == 0 && !hasMore {
			hasSubmitted := false
			for _, item := range page.Payments {
				hasSubmitted = hasSubmitted || item != nil && item.Status == "receipt_submitted"
			}
			for _, item := range page.Topups {
				hasSubmitted = hasSubmitted || item != nil && item.Status == "receipt_submitted"
			}
			if hasSubmitted {
				return a.home(c, "رسید قبلی شما در انتظار بررسی مدیریت است.", false)
			}
			return c.Send("درخواست فعالی برای ارسال رسید پیدا نشد. ابتدا خرید یا شارژ را شروع کنید.")
		}
		if len(awaiting) == 1 && !hasMore {
			return a.submitReceipt(c, awaiting[0], msg.Photo.FileID)
		}
		shown, err := a.showActiveReceiptPage(c, act, receiptPageCursor{}, nil, msg.Photo.FileID, false)
		if err != nil {
			return sendFailure(c, err)
		}
		if !shown {
			return c.Send("درخواست فعالی برای ارسال رسید پیدا نشد. ابتدا خرید یا شارژ را شروع کنید.")
		}
		return nil
	}
	return a.submitReceipt(c, *st.Receipt, msg.Photo.FileID)
}

func (a *botApp) submitReceipt(c telebot.Context, receipt receiptState, fileID string) error {
	act, err := a.resolve(c)
	if err != nil {
		return sendFailure(c, err)
	}
	path := fmt.Sprintf("/v1/payment-intents/%d/receipt", receipt.ID)
	if receipt.Kind == "topup" {
		path = fmt.Sprintf("/v1/wallet/topups/%d/receipt", receipt.ID)
	} else if receipt.Kind != "payment" {
		return c.Send("نوع درخواست رسید نامعتبر است.")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err = a.api.Call(ctx, "POST", path, act.TelegramID, map[string]any{"telegram_file_id": fileID}, nil); err != nil {
		return sendFailure(c, err)
	}
	a.clearState(c.Sender().ID)
	return a.home(c, "رسید ثبت شد و برای بررسی اپراتور در صف قرار گرفت.", false)
}
func (a *botApp) services(c telebot.Context, edit bool) error {
	return a.servicesPage(c, edit, 0)
}
func (a *botApp) servicesPage(c telebot.Context, edit bool, offset int) error {
	act, err := a.resolve(c)
	if err != nil {
		return sendFailure(c, err)
	}
	var out []subscriptionView
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err = a.api.Call(ctx, "GET", "/v1/subscriptions", act.TelegramID, nil, &out); err != nil {
		return sendFailure(c, err)
	}
	if len(out) == 0 {
		st := a.state(c.Sender().ID)
		m := &telebot.ReplyMarkup{}
		m.Inline(m.Row(m.Data("« بازگشت", "nav", st.Nonce, "home")))
		return present(c, "📋 شما در حال حاضر هیچ اشتراکی ندارید.\nبرای شروع از گزینه‌های تست رایگان یا خرید سرویس استفاده کنید.", m, edit)
	}
	totalPages := (len(out) + retailServicesPageSize - 1) / retailServicesPageSize
	if offset < 0 {
		offset = 0
	}
	if offset >= totalPages {
		offset = totalPages - 1
	}
	start := offset * retailServicesPageSize
	end := start + retailServicesPageSize
	if end > len(out) {
		end = len(out)
	}
	m := &telebot.ReplyMarkup{}
	st := a.state(c.Sender().ID)
	rows := make([]telebot.Row, 0, retailServicesPageSize+3)
	var text strings.Builder
	fmt.Fprintf(&text, "📋 سرویس‌های من (صفحه %d از %d)\n\n", offset+1, totalPages)
	for _, sub := range out[start:end] {
		icon := "🔴"
		if sub.Status == "active" {
			icon = "🟢"
		}
		expires := "بدون تاریخ انقضا"
		if sub.ExpiryTimeMS < 0 {
			expires = "شروع پس از اولین اتصال"
		} else if sub.ExpiryTimeMS > 0 {
			expires = "انقضا: " + time.UnixMilli(sub.ExpiryTimeMS).UTC().Format("2006-01-02")
		}
		name := sub.DisplayName
		if name == "" {
			name = sub.Email
		}
		fmt.Fprintf(&text, "%s %s — %s\n", icon, name, expires)
		rows = append(rows, m.Row(m.Data(icon+" "+truncateButton(name), "nav", st.Nonce, "services-view", strconv.FormatInt(sub.ID, 10))))
	}
	navigation := make([]telebot.Btn, 0, 2)
	if offset > 0 {
		navigation = append(navigation, m.Data("◀️ قبلی", "nav", st.Nonce, "services-page", strconv.Itoa(offset-1)))
	}
	if offset+1 < totalPages {
		navigation = append(navigation, m.Data("بعدی ▶️", "nav", st.Nonce, "services-page", strconv.Itoa(offset+1)))
	}
	if len(navigation) > 0 {
		rows = append(rows, m.Row(navigation...))
	}
	rows = append(rows, m.Row(m.Data("🏠 خانه", "nav", st.Nonce, "home")))
	m.Inline(rows...)
	return present(c, strings.TrimSpace(text.String()), m, edit)
}

func (a *botApp) serviceDetail(c telebot.Context, id int64, edit bool) error {
	act, err := a.resolve(c)
	if err != nil {
		return sendFailure(c, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var items []subscriptionView
	if err = a.api.Call(ctx, "GET", "/v1/subscriptions", act.TelegramID, nil, &items); err != nil {
		return sendFailure(c, err)
	}
	var sub *subscriptionView
	for i := range items {
		if items[i].ID == id {
			sub = &items[i]
			break
		}
	}
	if sub == nil {
		return a.services(c, edit)
	}
	status := "🔴 غیرفعال"
	if sub.Status == "active" {
		status = "🟢 فعال"
	}
	name := sub.DisplayName
	if name == "" {
		name = sub.Email
	}
	text := fmt.Sprintf("📋 %s\n%s\nنوع: %s\nایمیل: %s\nمحدودیت IP: %d\nحجم: %.2f گیگابایت", name, status, sub.Kind, sub.Email, sub.IPLimit, float64(sub.TrafficLimitBytes)/1073741824)
	if sub.TrafficLimitBytes <= 0 {
		text = strings.Replace(text, "حجم: 0.00 گیگابایت", "حجم: نامحدود", 1)
	}
	if sub.ExpiryTimeMS < 0 {
		text += "\nمدت اعتبار: پس از اولین اتصال"
	} else if sub.ExpiryTimeMS > 0 {
		text += "\nانقضا: " + time.UnixMilli(sub.ExpiryTimeMS).UTC().Format("2006-01-02 15:04 UTC")
	}
	m := &telebot.ReplyMarkup{}
	st := a.state(c.Sender().ID)
	rows := make([]telebot.Row, 0, len(sub.Links)+2)
	for i, link := range sub.Links {
		if strings.HasPrefix(link, "https://") || strings.HasPrefix(link, "http://") {
			rows = append(rows, m.Row(m.URL(fmt.Sprintf("🔗 دریافت لینک اتصال %d", i+1), link)))
		} else {
			text += fmt.Sprintf("\n\n🔗 لینک اتصال %d:\n%s", i+1, link)
		}
	}
	rows = append(rows, m.Row(m.Data("🗑 درخواست لغو سرویس", "nav", st.Nonce, "cancel", strconv.FormatInt(sub.ID, 10))))
	if sub.Kind == "paid" {
		rows = append(rows, m.Row(m.Data("💸 درخواست بازپرداخت", "nav", st.Nonce, "refund-request", strconv.FormatInt(sub.ID, 10))))
	}
	rows = append(rows, m.Row(m.Data("« بازگشت", "nav", st.Nonce, "services")))
	m.Inline(rows...)
	return present(c, text, m, edit)
}

func truncateButton(value string) string {
	runes := []rune(value)
	if len(runes) > 32 {
		return string(runes[:32])
	}
	return value
}
func (a *botApp) cancelSubscription(c telebot.Context, id int64) error {
	act, err := a.resolve(c)
	if err != nil {
		return sendFailure(c, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var out map[string]any
	key := callbackOperationKey(c, "cancel", strconv.FormatInt(id, 10))
	if err = a.api.Call(ctx, "POST", fmt.Sprintf("/v1/subscriptions/%d/cancel", id), act.TelegramID, map[string]any{"idempotency_key": key}, &out); err != nil {
		return sendFailure(c, err)
	}
	return a.home(c, "درخواست لغو ثبت شد و وضعیت پنل در حال تطبیق است.", true)
}

func (a *botApp) createRefundRequest(c telebot.Context, st conversation, reason string) error {
	act, err := a.resolve(c)
	if err != nil {
		return sendFailure(c, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var out refundRequestView
	path := fmt.Sprintf("/v1/subscriptions/%d/refunds", st.RefundSubID)
	body := map[string]any{"idempotency_key": st.OperationKey, "reason": strings.TrimSpace(reason)}
	if err = a.api.Call(ctx, "POST", path, act.TelegramID, body, &out); err != nil {
		return sendFailure(c, err)
	}
	a.clearState(c.Sender().ID)
	return a.home(c, fmt.Sprintf("درخواست بازپرداخت #%d برای بررسی مدیریت ثبت شد.", out.ID), false)
}

func (a *botApp) adminHome(c telebot.Context, edit bool) error {
	act, err := a.requireRetailAdmin(c)
	if err != nil {
		return sendFailure(c, err)
	}
	if c.Sender().ID != adminTelegramID || act.Role != "admin" {
		return a.home(c, "دسترسی مجاز نیست.", edit)
	}
	st := a.state(c.Sender().ID)
	m := &telebot.ReplyMarkup{}
	m.Inline(
		m.Row(m.Data("🧾 پرداخت‌های در انتظار", "nav", st.Nonce, "pending-payments"), m.Data("👛 شارژهای در انتظار", "nav", st.Nonce, "pending-topups")),
		m.Row(m.Data("↩️ بازپرداخت‌های در انتظار", "nav", st.Nonce, "pending-refunds")),
		m.Row(m.Data("🔧 کارهای زیرساخت", "nav", st.Nonce, "admin-work-items")),
		m.Row(m.Data("⚙️ تنظیمات فروشگاه", "nav", st.Nonce, "config")),
		m.Row(m.Data("🏠 خانه", "nav", st.Nonce, "home")),
	)
	return present(c, "مدیریت فروشگاه", m, edit)
}

type refundRequestView struct {
	ID                   int64  `json:"id"`
	SubscriptionID       int64  `json:"subscription_id"`
	Status               string `json:"status"`
	SuggestedAmountToman int64  `json:"suggested_amount_toman"`
	RefundableCapToman   int64  `json:"refundable_cap_toman"`
	Reason               string `json:"reason"`
}

type pendingReceiptView struct {
	ID             int64  `json:"id"`
	TelegramID     int64  `json:"telegram_id"`
	Amount         int64  `json:"amount_toman"`
	Status         string `json:"status"`
	TelegramFileID string `json:"telegram_file_id"`
}

func (a *botApp) pendingRefunds(c telebot.Context, edit bool) error {
	act, err := a.requireRetailAdmin(c)
	if err != nil {
		return sendFailure(c, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var out []refundRequestView
	if err = a.api.Call(ctx, "GET", "/v1/admin/refunds", act.TelegramID, nil, &out); err != nil {
		return sendFailure(c, err)
	}
	if len(out) == 0 {
		return a.adminMenuMessage(c, "بازپرداخت معوقی وجود ندارد.", edit)
	}
	if len(out) > 10 {
		out = out[:10]
	}
	st := a.state(c.Sender().ID)
	m := &telebot.ReplyMarkup{}
	rows := make([]telebot.Row, 0, len(out)+1)
	var text strings.Builder
	text.WriteString("بازپرداخت‌های در انتظار بررسی:\n")
	for _, item := range out {
		fmt.Fprintf(&text, "#%d | سرویس #%d | مبلغ پیشنهادی %s تومان | دلیل: %s\n", item.ID, item.SubscriptionID, formatToman(item.SuggestedAmountToman), item.Reason)
		rows = append(rows, m.Row(m.Data(fmt.Sprintf("بررسی بازپرداخت #%d", item.ID), "nav", st.Nonce, "review-refund", strconv.FormatInt(item.ID, 10))))
	}
	rows = append(rows, m.Row(m.Data("↩️ مدیریت", "nav", st.Nonce, "admin")))
	m.Inline(rows...)
	return present(c, strings.TrimSpace(text.String()), m, edit)
}

func (a *botApp) loadPendingRefund(c telebot.Context, id int64) (refundRequestView, error) {
	act, err := a.requireRetailAdmin(c)
	if err != nil {
		return refundRequestView{}, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var out []refundRequestView
	if err = a.api.Call(ctx, "GET", "/v1/admin/refunds", act.TelegramID, nil, &out); err != nil {
		return refundRequestView{}, err
	}
	for _, item := range out {
		if item.ID == id {
			return item, nil
		}
	}
	return refundRequestView{}, fmt.Errorf("refund request is no longer pending")
}

func (a *botApp) reviewRefund(c telebot.Context, id int64, edit bool) error {
	item, err := a.loadPendingRefund(c, id)
	if err != nil {
		return sendFailure(c, err)
	}
	st := a.state(c.Sender().ID)
	m := &telebot.ReplyMarkup{}
	rows := make([]telebot.Row, 0, 2)
	if item.RefundableCapToman > 0 && item.SuggestedAmountToman > 0 {
		rows = append(rows, m.Row(m.Data("✅ تأیید مبلغ پیشنهادی", "nav", st.Nonce, "approve-refund", strconv.FormatInt(item.ID, 10))))
	}
	rows = append(rows, m.Row(m.Data("❌ رد درخواست بازپرداخت", "nav", st.Nonce, "reject-refund", strconv.FormatInt(item.ID, 10))))
	rows = append(rows, m.Row(m.Data("↩️ بازپرداخت‌ها", "nav", st.Nonce, "pending-refunds")))
	m.Inline(rows...)
	text := fmt.Sprintf("بازپرداخت #%d برای سرویس #%d\nدرخواست: %s تومان\nسقف از شرایط خرید ثبت‌شده: %s تومان\nدلیل مشتری: %s\n\nتأیید فقط پس از لغو تأییدشده سرویس انجام می‌شود.", item.ID, item.SubscriptionID, formatToman(item.SuggestedAmountToman), formatToman(item.RefundableCapToman), item.Reason)
	if item.RefundableCapToman <= 0 {
		text += "\nاین درخواست شرایط خرید immutable ندارد و از این صفحه قابل تأیید نیست؛ بررسی دستی لازم است."
	}
	return present(c, text, m, edit)
}

func (a *botApp) approveRefund(c telebot.Context, id int64, edit bool) error {
	item, err := a.loadPendingRefund(c, id)
	if err != nil {
		return sendFailure(c, err)
	}
	if item.RefundableCapToman <= 0 || item.SuggestedAmountToman <= 0 || item.SuggestedAmountToman > item.RefundableCapToman {
		return a.reviewRefund(c, id, edit)
	}
	act, err := a.requireRetailAdmin(c)
	if err != nil {
		return sendFailure(c, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var out map[string]any
	key := callbackOperationKey(c, "refund-approve", strconv.FormatInt(id, 10))
	body := map[string]any{"amount_toman": item.SuggestedAmountToman, "audit_note": "Approved suggested amount after backend verified service cancellation.", "idempotency_key": key, "manual_override": false}
	if err = a.api.Call(ctx, "POST", fmt.Sprintf("/v1/admin/refunds/%d/approve", id), act.TelegramID, body, &out); err != nil {
		return sendFailure(c, err)
	}
	return a.adminMenuMessage(c, fmt.Sprintf("بازپرداخت #%d تأیید شد. اعتبار جدید کیف پول: %s تومان", id, formatToman(out["wallet_balance_toman"])), edit)
}

func (a *botApp) adminWorkItems(c telebot.Context, edit bool) error {
	act, err := a.requireRetailAdmin(c)
	if err != nil {
		return sendFailure(c, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var out []map[string]any
	if err = a.api.Call(ctx, "GET", "/v1/admin/work-items", act.TelegramID, nil, &out); err != nil {
		return sendFailure(c, err)
	}
	if len(out) == 0 {
		return a.adminMenuMessage(c, "کار زیرساختی ثبت نشده است.", edit)
	}
	if len(out) > 12 {
		out = out[:12]
	}
	var text strings.Builder
	text.WriteString("آخرین کارهای پایدار backend:\n")
	for _, item := range out {
		fmt.Fprintf(&text, "#%v | %v | %v/%v | تلاش: %v\n", item["id"], item["kind"], item["status"], item["phase"], item["attempts"])
	}
	return a.adminMenuMessage(c, strings.TrimSpace(text.String()), edit)
}
func (a *botApp) pending(c telebot.Context, edit bool) error {
	act, err := a.requireRetailAdmin(c)
	if err != nil {
		return sendFailure(c, err)
	}
	var out []pendingReceiptView
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err = a.api.Call(ctx, "GET", "/v1/admin/payments", act.TelegramID, nil, &out); err != nil {
		return sendFailure(c, err)
	}
	return a.renderPendingReceiptList(c, "payment", out, edit)
}
func (a *botApp) pendingTopups(c telebot.Context, edit bool) error {
	act, err := a.requireRetailAdmin(c)
	if err != nil {
		return sendFailure(c, err)
	}
	var out []pendingReceiptView
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err = a.api.Call(ctx, "GET", "/v1/admin/topups", act.TelegramID, nil, &out); err != nil {
		return sendFailure(c, err)
	}
	return a.renderPendingReceiptList(c, "topup", out, edit)
}

func (a *botApp) renderPendingReceiptList(c telebot.Context, kind string, out []pendingReceiptView, edit bool) error {
	st := a.state(c.Sender().ID)
	offset := 0
	if st.AdminPendingKind == kind {
		offset = st.AdminPendingOffset
	}
	if offset < 0 {
		offset = 0
	}
	if offset >= len(out) && len(out) > 0 {
		offset = ((len(out) - 1) / adminReceiptMenuPageSize) * adminReceiptMenuPageSize
	}
	if len(out) == 0 {
		st.AdminPendingKind = kind
		st.AdminPendingOffset = 0
		st.AdminReceiptKind = ""
		st.AdminReceiptID = 0
		a.setState(c.Sender().ID, st)
		message := "پرداخت معوقی وجود ندارد."
		if kind == "topup" {
			message = "درخواست شارژ معوقی وجود ندارد."
		}
		return a.adminMenuMessage(c, message, edit)
	}
	end := offset + adminReceiptMenuPageSize
	if end > len(out) {
		end = len(out)
	}
	st.AdminPendingKind = kind
	st.AdminPendingOffset = offset
	st.AdminReceiptKind = ""
	st.AdminReceiptID = 0
	a.setState(c.Sender().ID, st)
	st = a.state(c.Sender().ID)
	m := &telebot.ReplyMarkup{}
	rows := make([]telebot.Row, 0, end-offset+3)
	var b strings.Builder
	label, action := "پرداخت", "view-payment-receipt"
	if kind == "topup" {
		label, action = "شارژ", "view-topup-receipt"
	}
	fmt.Fprintf(&b, "%s‌های در انتظار (%d–%d از %d):\n", label, offset+1, end, len(out))
	for _, item := range out[offset:end] {
		id := strconv.FormatInt(item.ID, 10)
		fmt.Fprintf(&b, "%s %s — %s تومان\n", label, id, formatToman(item.Amount))
		rows = append(rows, m.Row(m.Data("📷 مشاهده رسید "+id, "nav", st.Nonce, action, id)))
	}
	if offset > 0 {
		prevAction := "pending-payments-prev"
		if kind == "topup" {
			prevAction = "pending-topups-prev"
		}
		rows = append(rows, m.Row(m.Data("« قبلی", "nav", st.Nonce, prevAction)))
	}
	if end < len(out) {
		nextAction := "pending-payments-next"
		if kind == "topup" {
			nextAction = "pending-topups-next"
		}
		rows = append(rows, m.Row(m.Data("بعدی »", "nav", st.Nonce, nextAction)))
	}
	rows = append(rows, m.Row(m.Data("↩️ مدیریت", "nav", st.Nonce, "admin")))
	m.Inline(rows...)
	return present(c, strings.TrimSpace(b.String()), m, edit)
}

func (a *botApp) changePendingReceiptPage(c telebot.Context, kind string, next bool) error {
	st := a.state(c.Sender().ID)
	if st.AdminPendingKind != kind {
		st.AdminPendingOffset = 0
	}
	if next {
		st.AdminPendingOffset += adminReceiptMenuPageSize
	} else {
		st.AdminPendingOffset -= adminReceiptMenuPageSize
	}
	if st.AdminPendingOffset < 0 {
		st.AdminPendingOffset = 0
	}
	st.AdminPendingKind = kind
	a.setState(c.Sender().ID, st)
	if kind == "topup" {
		return a.pendingTopups(c, true)
	}
	return a.pending(c, true)
}

func (a *botApp) loadPendingReceipt(c telebot.Context, kind string, id int64) (pendingReceiptView, error) {
	act, err := a.requireRetailAdmin(c)
	if err != nil {
		return pendingReceiptView{}, err
	}
	path := "/v1/admin/payments"
	if kind == "topup" {
		path = "/v1/admin/topups"
	} else if kind != "payment" {
		return pendingReceiptView{}, fmt.Errorf("invalid receipt type")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var items []pendingReceiptView
	if err = a.api.Call(ctx, "GET", path, act.TelegramID, nil, &items); err != nil {
		return pendingReceiptView{}, err
	}
	for _, item := range items {
		if item.ID == id {
			if item.TelegramFileID == "" || item.Status != "receipt_submitted" {
				return pendingReceiptView{}, fmt.Errorf("receipt evidence is unavailable")
			}
			return item, nil
		}
	}
	return pendingReceiptView{}, fmt.Errorf("receipt is no longer pending")
}

func parseAdminItemID(args []string) (int64, error) {
	if len(args) != 1 {
		return 0, fmt.Errorf("invalid request")
	}
	id, err := strconv.ParseInt(args[0], 10, 64)
	if err != nil || id <= 0 {
		return 0, fmt.Errorf("invalid request")
	}
	return id, nil
}

func (a *botApp) viewPendingReceipt(c telebot.Context, kind string, args []string, st conversation) error {
	id, err := parseAdminItemID(args)
	if err != nil {
		return a.adminMenuMessage(c, "شناسه رسید نامعتبر است.", true)
	}
	item, err := a.loadPendingReceipt(c, kind, id)
	if err != nil {
		return sendFailure(c, err)
	}
	label := "پرداخت مستقیم"
	if kind == "topup" {
		label = "شارژ کیف پول"
	}
	caption := fmt.Sprintf("رسید %s #%d — %s تومان", label, item.ID, formatToman(item.Amount))
	if item.TelegramID > 0 {
		caption += fmt.Sprintf("\nکاربر تلگرام: %d", item.TelegramID)
	}
	if err = c.Send(&telebot.Photo{File: telebot.File{FileID: item.TelegramFileID}, Caption: caption}); err != nil {
		log.Printf("admin receipt image delivery failed: category=telegram")
		return err
	}
	a.setState(c.Sender().ID, conversation{AdminReceiptKind: kind, AdminReceiptID: item.ID, AdminPendingKind: st.AdminPendingKind, AdminPendingOffset: st.AdminPendingOffset})
	st = a.state(c.Sender().ID)
	m := &telebot.ReplyMarkup{}
	approve, reject, back := "approve-payment", "reject-payment", "pending-payments"
	if kind == "topup" {
		approve, reject, back = "approve-topup", "reject-topup", "pending-topups"
	}
	m.Inline(
		m.Row(m.Data("✅ تأیید", "nav", st.Nonce, approve, strconv.FormatInt(item.ID, 10)), m.Data("❌ رد", "nav", st.Nonce, reject, strconv.FormatInt(item.ID, 10))),
		m.Row(m.Data("↩️ بازگشت", "nav", st.Nonce, back)),
	)
	return c.Send(caption+"\nبرای رد کردن ابتدا تأیید می‌کنید؟", m)
}

func (a *botApp) approveReviewedReceipt(c telebot.Context, kind string, args []string, st conversation) error {
	id, err := parseAdminItemID(args)
	if err != nil {
		return a.adminMenuMessage(c, "شناسه درخواست نامعتبر است.", true)
	}
	if st.AdminReceiptKind != kind || st.AdminReceiptID != id {
		return a.adminMenuMessage(c, "ابتدا رسید همان درخواست را به صورت خصوصی بررسی کنید.", true)
	}
	if _, err = a.loadPendingReceipt(c, kind, id); err != nil {
		return sendFailure(c, err)
	}
	path := "/v1/payment-intents/%d/approve"
	if kind == "topup" {
		path = "/v1/wallet/topups/%d/approve"
	}
	return a.adminAction(c, args, path, true)
}

func (a *botApp) confirmReject(c telebot.Context, kind string, args []string, st conversation) error {
	id, err := parseAdminItemID(args)
	if err != nil {
		return a.adminMenuMessage(c, "شناسه درخواست نامعتبر است.", true)
	}
	if kind == "refund" {
		if _, err = a.loadPendingRefund(c, id); err != nil {
			return sendFailure(c, err)
		}
	} else {
		if st.AdminReceiptKind != kind || st.AdminReceiptID != id {
			return a.adminMenuMessage(c, "ابتدا رسید همان درخواست را به صورت خصوصی بررسی کنید.", true)
		}
		if _, err = a.loadPendingReceipt(c, kind, id); err != nil {
			return sendFailure(c, err)
		}
	}
	st = a.state(c.Sender().ID)
	m := &telebot.ReplyMarkup{}
	confirm := "confirm-reject-" + kind
	cancel := "cancel-reject-" + kind
	var cancelArgs []string
	if kind == "refund" {
		cancelArgs = append(cancelArgs, strconv.FormatInt(id, 10))
	}
	rows := []telebot.Row{
		m.Row(m.Data("بله، رد شود", "nav", st.Nonce, confirm, strconv.FormatInt(id, 10))),
	}
	cancelData := append([]string{st.Nonce, cancel}, cancelArgs...)
	rows = append(rows, m.Row(m.Data("لغو", "nav", cancelData...)))
	m.Inline(rows...)
	return present(c, fmt.Sprintf("رد %s #%d را تأیید می‌کنید؟ این تصمیم برای مشتری ثبت و به او اطلاع داده می‌شود.", kindLabel(kind), id), m, true)
}

func kindLabel(kind string) string {
	switch kind {
	case "payment":
		return "پرداخت"
	case "topup":
		return "درخواست شارژ"
	case "refund":
		return "درخواست بازپرداخت"
	default:
		return "درخواست"
	}
}

func (a *botApp) rejectAdminItem(c telebot.Context, kind string, args []string, edit bool, st conversation) error {
	id, err := parseAdminItemID(args)
	if err != nil {
		return a.adminMenuMessage(c, "شناسه درخواست نامعتبر است.", edit)
	}
	if kind == "payment" || kind == "topup" {
		if st.AdminReceiptKind != kind || st.AdminReceiptID != id {
			return a.adminMenuMessage(c, "ابتدا رسید همان درخواست را به صورت خصوصی بررسی کنید.", edit)
		}
		if _, err = a.loadPendingReceipt(c, kind, id); err != nil {
			return sendFailure(c, err)
		}
	}
	act, err := a.requireRetailAdmin(c)
	if err != nil {
		return sendFailure(c, err)
	}
	path := ""
	switch kind {
	case "payment":
		path = fmt.Sprintf("/v1/payment-intents/%d/reject", id)
	case "topup":
		path = fmt.Sprintf("/v1/admin/topups/%d/reject", id)
	case "refund":
		path = fmt.Sprintf("/v1/admin/refunds/%d/reject", id)
	default:
		return a.adminMenuMessage(c, "نوع درخواست نامعتبر است.", edit)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var out struct {
		Status          string `json:"status"`
		AlreadyRejected bool   `json:"already_rejected"`
	}
	if err = a.api.Call(ctx, "POST", path, act.TelegramID, map[string]any{}, &out); err != nil {
		return sendFailure(c, err)
	}
	if kind == "payment" || kind == "topup" {
		a.clearAdminReceiptReview(c)
	}
	return a.adminMenuMessage(c, fmt.Sprintf("%s #%d رد شد.", kindLabel(kind), id), edit)
}

func (a *botApp) adminMenuMessage(c telebot.Context, text string, edit bool) error {
	st := a.state(c.Sender().ID)
	m := &telebot.ReplyMarkup{}
	m.Inline(m.Row(m.Data("↩️ مدیریت", "nav", st.Nonce, "admin")), m.Row(m.Data("🏠 خانه", "nav", st.Nonce, "home")))
	return present(c, text, m, edit)
}
func (a *botApp) clearAdminReceiptReview(c telebot.Context) {
	st := a.state(c.Sender().ID)
	st.AdminReceiptKind = ""
	st.AdminReceiptID = 0
	a.setState(c.Sender().ID, st)
}
func (a *botApp) adminAction(c telebot.Context, args []string, path string, edit bool) error {
	act, err := a.requireRetailAdmin(c)
	if err != nil {
		return sendFailure(c, err)
	}
	if c.Sender().ID != adminTelegramID || act.Role != "admin" {
		return a.home(c, "دسترسی مجاز نیست.", edit)
	}
	if len(args) != 1 {
		return a.adminMenuMessage(c, "درخواست نامعتبر است.", edit)
	}
	id, e := strconv.ParseInt(args[0], 10, 64)
	if e != nil || id <= 0 {
		return a.adminMenuMessage(c, "شناسه نامعتبر است.", edit)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var out map[string]any
	if err = a.api.Call(ctx, "POST", fmt.Sprintf(path, id), act.TelegramID, map[string]any{}, &out); err != nil {
		return sendFailure(c, err)
	}
	a.clearAdminReceiptReview(c)
	return a.adminMenuMessage(c, "عملیات ثبت شد: "+fmt.Sprint(out["status"]), edit)
}
func (a *botApp) adminConfig(c telebot.Context, edit bool) error {
	if _, err := a.requireRetailAdmin(c); err != nil {
		return sendFailure(c, err)
	}
	cfg, err := a.loadConfig(c)
	if err != nil {
		return sendFailure(c, err)
	}
	st := a.state(c.Sender().ID)
	m := &telebot.ReplyMarkup{}
	m.Inline(m.Row(m.Data("📦 طرح‌ها و قیمت‌ها", "nav", st.Nonce, "config-plans")), m.Row(m.Data("💳 اطلاعات پرداخت", "nav", st.Nonce, "config-payment")), m.Row(m.Data("🧪 سیاست تست و امکانات", "nav", st.Nonce, "config-settings")), m.Row(m.Data("🖥 تنظیم پنل", "nav", st.Nonce, "config-panel")), m.Row(m.Data("↩️ مدیریت", "nav", st.Nonce, "admin")))
	return present(c, fmt.Sprintf("پیکربندی %s | شناسه استقرار: %s", cfg.Channel, cfg.DeploymentID), m, edit)
}
func (a *botApp) requireAdmin(c telebot.Context) (actor, error) {
	if c.Chat() == nil || c.Chat().Type != telebot.ChatPrivate {
		return actor{}, fmt.Errorf("مدیریت فقط در گفت‌وگوی خصوصی در دسترس است")
	}
	act, err := a.resolve(c)
	if err != nil {
		return act, err
	}
	if c.Sender().ID != adminTelegramID || act.Role != "admin" {
		return act, fmt.Errorf("دسترسی مجاز نیست")
	}
	return act, nil
}
func (a *botApp) requireRetailAdmin(c telebot.Context) (actor, error) {
	act, err := a.requireAdmin(c)
	if err != nil {
		return act, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var cfg adminConfig
	if err = a.api.Call(ctx, "GET", "/v1/admin/config", act.TelegramID, nil, &cfg); err != nil {
		return act, err
	}
	if cfg.Channel != "retail" {
		return act, fmt.Errorf("retail administration is restricted to retail deployments")
	}
	return act, nil
}
func (a *botApp) loadConfig(c telebot.Context) (adminConfig, error) {
	act, err := a.requireRetailAdmin(c)
	if err != nil {
		return adminConfig{}, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var cfg adminConfig
	err = a.api.Call(ctx, "GET", "/v1/admin/config", act.TelegramID, nil, &cfg)
	if err == nil && cfg.Channel != "retail" {
		err = fmt.Errorf("retail configuration is restricted to retail deployments")
	}
	return cfg, err
}
func (a *botApp) adminPlans(c telebot.Context, edit bool) error {
	cfg, err := a.loadConfig(c)
	if err != nil {
		return sendFailure(c, err)
	}
	st := a.state(c.Sender().ID)
	m := &telebot.ReplyMarkup{}
	rows := make([]telebot.Row, 0, len(cfg.Plans)+2)
	var text strings.Builder
	for _, p := range cfg.Plans {
		state := "خاموش"
		if p.Enabled {
			state = "روشن"
		}
		fmt.Fprintf(&text, "%d · %s · %s · %s\n", p.ID, p.Name, p.Kind, state)
		rows = append(rows, m.Row(m.Data(fmt.Sprintf("ویرایش %s", buttonValue(p.Name)), "nav", st.Nonce, "config-edit-plan", strconv.FormatInt(p.ID, 10))))
	}
	rows = append(rows, m.Row(m.Data("➕ طرح جدید", "nav", st.Nonce, "config-new-plan")), m.Row(m.Data("↩️ تنظیمات", "nav", st.Nonce, "config")))
	m.Inline(rows...)
	if text.Len() == 0 {
		text.WriteString("طرحی تعریف نشده است.")
	}
	return present(c, text.String(), m, edit)
}
func (a *botApp) editPlanPrompt(c telebot.Context, id int64, edit bool) error {
	cfg, err := a.loadConfig(c)
	if err != nil {
		return sendFailure(c, err)
	}
	var p *adminPlan
	for i := range cfg.Plans {
		if cfg.Plans[i].ID == id {
			p = &cfg.Plans[i]
			break
		}
	}
	if id <= 0 || p == nil {
		return a.adminPlans(c, edit)
	}
	return a.planEditor(c, *p, edit)
}
func (a *botApp) planEditor(c telebot.Context, p adminPlan, edit bool) error {
	st := a.state(c.Sender().ID)
	m := &telebot.ReplyMarkup{}
	rows := []telebot.Row{m.Row(m.Data("نام: "+buttonValue(p.Name), "nav", st.Nonce, "pf", strconv.FormatInt(p.ID, 10), planFieldCode("name")), m.Data("نوع: "+buttonValue(p.Kind), "nav", st.Nonce, "pf", strconv.FormatInt(p.ID, 10), planFieldCode("kind"))), m.Row(m.Data(boolLabel("طرح فعال", p.Enabled), "nav", st.Nonce, "pt", strconv.FormatInt(p.ID, 10), "e"), m.Data(boolLabel("حجم محدود", p.IsLimited), "nav", st.Nonce, "pt", strconv.FormatInt(p.ID, 10), "l")), m.Row(m.Data("توضیحات", "nav", st.Nonce, "pf", strconv.FormatInt(p.ID, 10), planFieldCode("description")), m.Data("قیمت پایه تومان: "+numberLabel(p.BasePrice), "nav", st.Nonce, "pf", strconv.FormatInt(p.ID, 10), planFieldCode("base_price_toman"))), m.Row(m.Data("قیمت IP اضافه: "+numberLabel(p.PricePerExtraIP), "nav", st.Nonce, "pf", strconv.FormatInt(p.ID, 10), planFieldCode("price_per_extra_ip_toman")), m.Data("قیمت هر GB: "+numberLabel(p.PricePerGB), "nav", st.Nonce, "pf", strconv.FormatInt(p.ID, 10), planFieldCode("price_per_gb_toman"))), m.Row(m.Data("قیمت ماه اضافه: "+numberLabel(p.PricePerExtraMonth), "nav", st.Nonce, "pf", strconv.FormatInt(p.ID, 10), planFieldCode("price_per_extra_month_toman")), m.Data("IP پایه/حداکثر: "+strconv.Itoa(p.BaseIP)+"/"+strconv.Itoa(p.MaxIP), "nav", st.Nonce, "pf", strconv.FormatInt(p.ID, 10), planFieldCode("ip_limits"))), m.Row(m.Data("حداقل GB: "+strconv.Itoa(p.MinGB), "nav", st.Nonce, "pf", strconv.FormatInt(p.ID, 10), planFieldCode("min_data_gb")), m.Data("حداکثر بایت: "+numberLabel(p.MaxBytes), "nav", st.Nonce, "pf", strconv.FormatInt(p.ID, 10), planFieldCode("max_data_bytes"))), m.Row(m.Data("مدت تست ثانیه: "+numberLabel(p.ExpireSeconds), "nav", st.Nonce, "pf", strconv.FormatInt(p.ID, 10), planFieldCode("expire_seconds")), m.Data("IP تست: "+strconv.Itoa(p.TestIPLimit), "nav", st.Nonce, "pf", strconv.FormatInt(p.ID, 10), planFieldCode("test_ip_limit"))), m.Row(m.Data("جریان: "+buttonValue(p.Flow), "nav", st.Nonce, "pf", strconv.FormatInt(p.ID, 10), planFieldCode("flow"))), m.Row(m.Data("شناسه‌های ورودی پنل", "nav", st.Nonce, "pf", strconv.FormatInt(p.ID, 10), planFieldCode("inbound_ids")), m.Data("راهنمای حجم", "nav", st.Nonce, "pf", strconv.FormatInt(p.ID, 10), planFieldCode("usage_description"))), m.Row(m.Data("↩️ طرح‌ها", "nav", st.Nonce, "config-plans"))}
	m.Inline(rows...)
	summary := fmt.Sprintf("تنظیم طرح %s (#%d). مقدار فعلی روی هر گزینه نمایش داده شده است.", p.Name, p.ID)
	return present(c, summary, m, edit)
}
func buttonValue(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "—"
	}
	r := []rune(s)
	if len(r) > 18 {
		return string(r[:18]) + "…"
	}
	return s
}
func numberLabel(n int64) string { return strconv.FormatInt(n, 10) }
func boolLabel(label string, value bool) string {
	state := "خاموش"
	if value {
		state = "روشن"
	}
	return label + ": " + state
}
func planFieldCode(field string) string {
	switch field {
	case "name":
		return "n"
	case "kind":
		return "k"
	case "description":
		return "d"
	case "base_price_toman":
		return "b"
	case "price_per_extra_ip_toman":
		return "i"
	case "price_per_gb_toman":
		return "g"
	case "price_per_extra_month_toman":
		return "m"
	case "ip_limits":
		return "p"
	case "min_data_gb":
		return "l"
	case "max_data_bytes":
		return "c"
	case "expire_seconds":
		return "e"
	case "test_ip_limit":
		return "t"
	case "flow":
		return "f"
	case "inbound_ids":
		return "u"
	case "usage_description":
		return "v"
	}
	return ""
}
func planFieldFromCode(code string) (string, bool) {
	fields := []string{"name", "kind", "description", "base_price_toman", "price_per_extra_ip_toman", "price_per_gb_toman", "price_per_extra_month_toman", "ip_limits", "min_data_gb", "max_data_bytes", "expire_seconds", "test_ip_limit", "flow", "inbound_ids", "usage_description"}
	for _, field := range fields {
		if planFieldCode(field) == code {
			return field, true
		}
	}
	return "", false
}
func (a *botApp) editPlanField(c telebot.Context, id int64, field string) error {
	cfg, err := a.loadConfig(c)
	if err != nil {
		return sendFailure(c, err)
	}
	for _, p := range cfg.Plans {
		if p.ID == id {
			st := a.state(c.Sender().ID)
			st.Admin = "planfield:" + field
			st.AdminID = id
			st = a.next(st)
			a.setState(c.Sender().ID, st)
			return a.prompt(c, "مقدار فعلی: "+planFieldValue(p, field)+"\n"+planFieldHint(field), true)
		}
	}
	return a.adminPlans(c, true)
}
func planFieldHint(field string) string {
	switch field {
	case "name":
		return "نام جدید را ارسال کنید."
	case "kind":
		return "paid یا test را ارسال کنید."
	case "description", "usage_description":
		return "متن جدید را ارسال کنید؛ برای پاک کردن، خط تیره بفرستید."
	case "ip_limits":
		return "IP پایه و حداکثر را با ویرگول بفرستید؛ نمونه: ۱,۳."
	case "inbound_ids":
		return "شناسه‌های عددی inbound را با ویرگول جدا کنید. فهرست خالی یعنی بدون inbound."
	case "base_price_toman", "price_per_extra_ip_toman", "price_per_gb_toman", "price_per_extra_month_toman":
		return "مبلغ را به تومان، به‌صورت عدد صحیح نامنفی بفرستید."
	case "max_data_bytes":
		return "حداکثر حجم را به بایت وارد کنید؛ صفر یعنی نامحدود."
	case "expire_seconds":
		return "مدت را به ثانیه، به‌صورت عدد صحیح نامنفی وارد کنید."
	default:
		return "مقدار را به‌صورت عدد صحیح نامنفی ارسال کنید."
	}
}
func planFieldValue(p adminPlan, field string) string {
	switch field {
	case "name":
		return p.Name
	case "kind":
		return p.Kind
	case "description":
		return p.Description
	case "base_price_toman":
		return numberLabel(p.BasePrice)
	case "price_per_extra_ip_toman":
		return numberLabel(p.PricePerExtraIP)
	case "price_per_gb_toman":
		return numberLabel(p.PricePerGB)
	case "price_per_extra_month_toman":
		return numberLabel(p.PricePerExtraMonth)
	case "ip_limits":
		return fmt.Sprintf("%d,%d", p.BaseIP, p.MaxIP)
	case "min_data_gb":
		return strconv.Itoa(p.MinGB)
	case "max_data_bytes":
		return numberLabel(p.MaxBytes)
	case "expire_seconds":
		return numberLabel(p.ExpireSeconds)
	case "test_ip_limit":
		return strconv.Itoa(p.TestIPLimit)
	case "max_per_day":
		return strconv.Itoa(p.MaxPerDay)
	case "flow":
		return p.Flow
	case "inbound_ids":
		parts := make([]string, len(p.InboundIDs))
		for i, id := range p.InboundIDs {
			parts[i] = strconv.FormatInt(id, 10)
		}
		return strings.Join(parts, ",")
	case "usage_description":
		return p.UsageDescription
	}
	return ""
}
func (a *botApp) togglePlan(c telebot.Context, id int64, field string) error {
	cfg, err := a.loadConfig(c)
	if err != nil {
		return sendFailure(c, err)
	}
	for _, p := range cfg.Plans {
		if p.ID == id {
			if field == "enabled" {
				p.Enabled = !p.Enabled
			} else if field == "is_limited" {
				p.IsLimited = !p.IsLimited
			} else {
				return a.planEditor(c, p, true)
			}
			return a.updatePlan(c, p)
		}
	}
	return a.adminPlans(c, true)
}
func (a *botApp) createPlan(c telebot.Context, p *adminPlan) error {
	act, err := a.requireRetailAdmin(c)
	if err != nil {
		return sendFailure(c, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var result struct {
		ID     int64       `json:"id"`
		Config adminConfig `json:"config"`
	}
	if err = a.api.Call(ctx, "POST", "/v1/admin/config/plans", act.TelegramID, p, &result); err != nil {
		return sendFailure(c, err)
	}
	a.resetPlanEditorState(c)
	for _, created := range result.Config.Plans {
		if created.ID == result.ID {
			return a.planEditor(c, created, c.Callback() != nil)
		}
	}
	return a.adminPlans(c, c.Callback() != nil)
}
func (a *botApp) updatePlan(c telebot.Context, p adminPlan) error {
	act, err := a.requireRetailAdmin(c)
	if err != nil {
		return sendFailure(c, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var cfg adminConfig
	if err = a.api.Call(ctx, "PUT", fmt.Sprintf("/v1/admin/config/plans/%d", p.ID), act.TelegramID, p, &cfg); err != nil {
		return sendFailure(c, err)
	}
	a.resetPlanEditorState(c)
	for _, updated := range cfg.Plans {
		if updated.ID == p.ID {
			return a.planEditor(c, updated, c.Callback() != nil)
		}
	}
	return a.adminPlans(c, c.Callback() != nil)
}

func (a *botApp) resetPlanEditorState(c telebot.Context) {
	st := a.state(c.Sender().ID)
	st.Step = ""
	st.Admin = ""
	st.AdminID = 0
	st.Draft = nil
	a.setState(c.Sender().ID, st)
}
func (a *botApp) adminPayment(c telebot.Context, edit bool) error {
	cfg, err := a.loadConfig(c)
	if err != nil {
		return sendFailure(c, err)
	}
	st := a.state(c.Sender().ID)
	m := &telebot.ReplyMarkup{}
	m.Inline(m.Row(m.Data("شماره کارت", "nav", st.Nonce, "config-payment-field", "card_number")), m.Row(m.Data("صاحب کارت", "nav", st.Nonce, "config-payment-field", "card_owner")), m.Row(m.Data("توضیحات پرداخت", "nav", st.Nonce, "config-payment-field", "instructions")), m.Row(m.Data("↩️ تنظیمات", "nav", st.Nonce, "config")))
	return present(c, fmt.Sprintf("اطلاعات پرداخت فعلی\nشماره کارت: %s\nصاحب کارت: %s\nتوضیحات: %s", cfg.PaymentInstructions["card_number"], cfg.PaymentInstructions["card_owner"], cfg.PaymentInstructions["instructions"]), m, edit)
}
func paymentInstructionPatch(current map[string]string, field, value string) map[string]string {
	updated := map[string]string{"card_number": current["card_number"], "card_owner": current["card_owner"], "instructions": current["instructions"]}
	updated[field] = value
	return updated
}
func (a *botApp) adminSettings(c telebot.Context, edit bool) error {
	cfg, err := a.loadConfig(c)
	if err != nil {
		return sendFailure(c, err)
	}
	st := a.state(c.Sender().ID)
	m := &telebot.ReplyMarkup{}
	rows := []telebot.Row{m.Row(m.Data("تغییر فاصله تست", "nav", st.Nonce, "config-trial-days")), m.Row(m.Data("تغییر حداقل شارژ", "nav", st.Nonce, "config-min-topup"))}
	var text strings.Builder
	fmt.Fprintf(&text, "فاصله تست خرده‌فروشی: %d روز\nحداقل مبلغ شارژ: %s تومان (صفر یعنی غیرفعال)\n", cfg.Settings.RetailTrialResetDays, formatToman(cfg.Settings.MinTopupToman))
	knownFeatures := []string{"purchases_enabled", "trials_enabled", "wallet_enabled", "topups_enabled", "direct_payments_enabled"}
	for _, key := range knownFeatures {
		if _, exists := cfg.Settings.Features[key]; !exists {
			cfg.Settings.Features[key] = true
		}
	}
	for key, on := range cfg.Settings.Features {
		label := "خاموش"
		if on {
			label = "روشن"
		}
		fmt.Fprintf(&text, "%s: %s\n", featureLabel(key), label)
		rows = append(rows, m.Row(m.Data("تغییر: "+featureLabel(key), "nav", st.Nonce, "ft", key)))
	}
	for key, value := range cfg.Settings.Text {
		fmt.Fprintf(&text, "متن %s: %s\n", key, value)
		rows = append(rows, m.Row(m.Data("تغییر متن: "+key, "nav", st.Nonce, "tx", key)))
	}
	rows = append(rows, m.Row(m.Data("➕ افزودن متن", "nav", st.Nonce, "config-text-new")))
	rows = append(rows, m.Row(m.Data("↩️ تنظیمات", "nav", st.Nonce, "config")))
	m.Inline(rows...)
	return present(c, text.String(), m, edit)
}
func featureLabel(key string) string {
	switch key {
	case "purchases_enabled":
		return "فروش طرح‌های خرید"
	case "trials_enabled":
		return "ارائه طرح تست"
	case "wallet_enabled":
		return "کیف پول"
	case "topups_enabled":
		return "شارژ کیف پول"
	case "direct_payments_enabled":
		return "پرداخت مستقیم"
	}
	return key
}
func (a *botApp) toggleFeature(c telebot.Context, key string) error {
	cfg, err := a.loadConfig(c)
	if err != nil {
		return sendFailure(c, err)
	}
	value, ok := cfg.Settings.Features[key]
	if !ok {
		value = true
	}
	act, err := a.requireRetailAdmin(c)
	if err != nil {
		return sendFailure(c, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var updated adminConfig
	err = a.api.Call(ctx, "PATCH", "/v1/admin/config/settings", act.TelegramID, map[string]any{"features": map[string]bool{key: !value}}, &updated)
	if err != nil {
		return sendFailure(c, err)
	}
	return a.adminSettings(c, true)
}
func (a *botApp) adminPanel(c telebot.Context, edit bool) error {
	cfg, err := a.loadConfig(c)
	if err != nil {
		return sendFailure(c, err)
	}
	st := a.state(c.Sender().ID)
	m := &telebot.ReplyMarkup{}
	m.Inline(m.Row(m.Data("تغییر آدرس و کلید پنل", "nav", st.Nonce, "config-panel-url")), m.Row(m.Data("↩️ تنظیمات", "nav", st.Nonce, "config")))
	configured := "تنظیم نشده"
	if cfg.Panel.TokenConfigured {
		configured = "تنظیم شده"
	}
	return present(c, fmt.Sprintf("پنل متصل به همین استقرار\nآدرس: %s\nکلید دسترسی: %s", cfg.Panel.BaseURL, configured), m, edit)
}
func (a *botApp) adminTextInput(c telebot.Context, st conversation, value string) error {
	act, err := a.requireRetailAdmin(c)
	if err != nil {
		return sendFailure(c, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var out adminConfig
	switch {
	case st.Admin == "new-plan-name":
		if len([]rune(value)) > 80 {
			return c.Send("نام طرح حداکثر ۸۰ نویسه باشد.")
		}
		st.Draft = &adminPlan{Name: value, Enabled: false, BaseIP: 1, MaxIP: 1, InboundIDs: []int64{}}
		st = a.next(st)
		a.setState(c.Sender().ID, st)
		st = a.state(c.Sender().ID)
		m := &telebot.ReplyMarkup{}
		m.Inline(m.Row(m.Data("طرح پولی", "nav", st.Nonce, "new-plan-kind", "paid"), m.Data("طرح تست", "nav", st.Nonce, "new-plan-kind", "test")), m.Row(m.Data("لغو", "nav", st.Nonce, "config-plans")))
		return c.Send("نوع طرح جدید را انتخاب کنید. طرح ابتدا غیرفعال ساخته می‌شود.", m)
	case st.Admin == "trial-days":
		days, e := strconv.Atoi(value)
		if e != nil || days > 3650 {
			return c.Send("روز باید عدد صحیح باشد و حداکثر ۳۶۵۰.")
		}
		err = a.api.Call(ctx, "PATCH", "/v1/admin/config/settings", act.TelegramID, map[string]any{"retail_trial_reset_days": days}, &out)
	case st.Admin == "min-topup":
		amount, e := strconv.ParseInt(value, 10, 64)
		if e != nil || amount < 0 {
			return c.Send("مبلغ باید عدد صحیح صفر یا بیشتر باشد.")
		}
		err = a.api.Call(ctx, "PATCH", "/v1/admin/config/settings", act.TelegramID, map[string]any{"min_topup_toman": amount}, &out)
	case strings.HasPrefix(st.Admin, "payment:"):
		field := strings.TrimPrefix(st.Admin, "payment:")
		if field != "card_number" && field != "card_owner" && field != "instructions" {
			return c.Send("گزینه تنظیمات نامعتبر است.")
		}
		cfg, e := a.loadConfig(c)
		if e != nil {
			return sendFailure(c, e)
		}
		err = a.api.Call(ctx, "PATCH", "/v1/admin/config/payment-instructions", act.TelegramID, paymentInstructionPatch(cfg.PaymentInstructions, field, value), &out)
	case strings.HasPrefix(st.Admin, "text:"):
		key := strings.TrimPrefix(st.Admin, "text:")
		err = a.api.Call(ctx, "PATCH", "/v1/admin/config/settings", act.TelegramID, map[string]any{"text": map[string]string{key: value}}, &out)
	case st.Admin == "text-key":
		key := strings.TrimSpace(value)
		if len(key) < 2 || len(key) > 40 {
			return c.Send("کلید باید ۲ تا ۴۰ نویسه باشد.")
		}
		for _, r := range key {
			if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_') {
				return c.Send("فقط حروف کوچک انگلیسی، عدد و زیرخط مجاز است.")
			}
		}
		st.Admin = "text:" + key
		st = a.next(st)
		a.setState(c.Sender().ID, st)
		return a.prompt(c, "متن جدید را ارسال کنید.", true)
	case strings.HasPrefix(st.Admin, "planfield:"):
		field := strings.TrimPrefix(st.Admin, "planfield:")
		cfg, e := a.loadConfig(c)
		if e != nil {
			return sendFailure(c, e)
		}
		var p *adminPlan
		for i := range cfg.Plans {
			if cfg.Plans[i].ID == st.AdminID {
				p = &cfg.Plans[i]
				break
			}
		}
		if p == nil {
			return a.adminPlans(c, false)
		}
		switch field {
		case "name":
			if len([]rune(value)) > 80 {
				return c.Send("نام حداکثر ۸۰ نویسه باشد.")
			}
			p.Name = value
		case "kind":
			if value != "paid" && value != "test" {
				return c.Send("نوع باید paid یا test باشد.")
			}
			p.Kind = value
		case "description":
			if value != "-" {
				p.Description = value
			} else {
				p.Description = ""
			}
		case "base_price_toman", "price_per_extra_ip_toman", "price_per_gb_toman", "price_per_extra_month_toman", "max_data_bytes", "expire_seconds":
			n, e := strconv.ParseInt(value, 10, 64)
			if e != nil || n < 0 {
				return c.Send("مقدار باید عدد صحیح نامنفی باشد.")
			}
			switch field {
			case "base_price_toman":
				p.BasePrice = n
			case "price_per_extra_ip_toman":
				p.PricePerExtraIP = n
			case "price_per_gb_toman":
				p.PricePerGB = n
			case "price_per_extra_month_toman":
				p.PricePerExtraMonth = n
			case "max_data_bytes":
				p.MaxBytes = n
			case "expire_seconds":
				p.ExpireSeconds = n
			}
		case "min_data_gb", "test_ip_limit", "max_per_day":
			n, e := strconv.Atoi(value)
			if e != nil || n < 0 {
				return c.Send("مقدار باید عدد صحیح نامنفی باشد.")
			}
			switch field {
			case "min_data_gb":
				p.MinGB = n
			case "test_ip_limit":
				p.TestIPLimit = n
			case "max_per_day":
				p.MaxPerDay = n
			}
		case "ip_limits":
			parts := strings.Split(value, ",")
			if len(parts) != 2 {
				return c.Send("دو عدد با ویرگول بفرستید؛ نمونه: ۱,۳.")
			}
			base, e1 := strconv.Atoi(strings.TrimSpace(parts[0]))
			max, e2 := strconv.Atoi(strings.TrimSpace(parts[1]))
			if e1 != nil || e2 != nil || base < 0 || max < base {
				return c.Send("حد IP نامعتبر است.")
			}
			p.BaseIP, p.MaxIP = base, max
		case "flow":
			p.Flow = value
		case "inbound_ids":
			if value == "-" {
				p.InboundIDs = []int64{}
				return a.updatePlan(c, *p)
			}
			parts := strings.Split(value, ",")
			ids := make([]int64, 0, len(parts))
			for _, part := range parts {
				part = strings.TrimSpace(part)
				if part == "" {
					continue
				}
				n, e := strconv.ParseInt(part, 10, 64)
				if e != nil || n <= 0 {
					return c.Send("شناسه‌های inbound را با ویرگول، به‌شکل اعداد مثبت بفرستید.")
				}
				ids = append(ids, n)
			}
			if len(ids) > 64 {
				return c.Send("حداکثر ۶۴ inbound مجاز است.")
			}
			p.InboundIDs = ids
		case "usage_description":
			if value != "-" {
				p.UsageDescription = value
			} else {
				p.UsageDescription = ""
			}
		default:
			return c.Send("فیلد طرح نامعتبر است.")
		}
		return a.updatePlan(c, *p)
	case st.Admin == "panel-url":
		if c.Chat().Type != telebot.ChatPrivate {
			return c.Send("تنظیم کلید پنل فقط در گفت‌وگوی خصوصی با ربات مجاز است.")
		}
		st.PanelURL = value
		st.Admin = "panel-token"
		st = a.next(st)
		a.setState(c.Sender().ID, st)
		return c.Send("کلید دسترسی پنل را بفرستید. این مقدار به backend می‌رود و در پاسخ یا صفحه تنظیمات نمایش داده نمی‌شود.", &telebot.ReplyMarkup{ForceReply: true, Placeholder: "کلید پنل"})
	case st.Admin == "panel-token":
		if c.Chat().Type != telebot.ChatPrivate {
			return c.Send("تنظیم کلید پنل فقط در گفت‌وگوی خصوصی با ربات مجاز است.")
		}
		err = submitPanelToken(c.Chat(), c.Delete, func() error {
			return a.api.Call(ctx, "PUT", "/v1/admin/config/panel", act.TelegramID, map[string]string{"base_url": st.PanelURL, "token": value}, &out)
		})
		if errors.Is(err, errPanelTokenDelete) {
			a.clearState(c.Sender().ID)
			return c.Send("پیام کلید حذف نشد و تنظیمی ذخیره نشد. لطفاً پیام کلید را خودتان حذف کنید و سپس تنظیم را دوباره از گفت‌وگوی خصوصی آغاز کنید.")
		}
	default:
		return c.Send("این مرحله منقضی شده است. از منوی مدیریت دوباره شروع کنید.")
	}
	if err != nil {
		return sendFailure(c, err)
	}
	a.clearState(c.Sender().ID)
	return a.adminConfig(c, false)
}

var (
	errPanelPrivateChat = errors.New("panel configuration requires a private chat")
	errPanelTokenDelete = errors.New("panel token message could not be deleted")
)

func beginPanelURLPrompt(chat *telebot.Chat, setState func(), prompt func() error) error {
	if chat == nil || chat.Type != telebot.ChatPrivate {
		return errPanelPrivateChat
	}
	setState()
	return prompt()
}

func submitPanelToken(chat *telebot.Chat, deleteMessage func() error, submit func() error) error {
	if chat == nil || chat.Type != telebot.ChatPrivate {
		return errPanelPrivateChat
	}
	if deleteMessage == nil || deleteMessage() != nil {
		return errPanelTokenDelete
	}
	return submit()
}

func stableKey(senderID, chatID, messageID int64, operation string) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%d:%d:%d:%s", senderID, chatID, messageID, operation)))
	return hex.EncodeToString(sum[:])
}
func sessionKey(senderID, chatID, messageID int64, session, operation, target string) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%d:%d:%d:%s:%s:%s", senderID, chatID, messageID, session, operation, target)))
	return hex.EncodeToString(sum[:])
}
func callbackOperationKey(c telebot.Context, operation, target string) string {
	session := ""
	if c.Callback() != nil {
		parts := strings.Split(c.Data(), "|")
		if len(parts) > 0 {
			session = parts[0]
		}
	}
	messageID := int64(0)
	if c.Callback() != nil && c.Callback().Message != nil {
		messageID = int64(c.Callback().Message.ID)
	}
	return sessionKey(c.Sender().ID, c.Chat().ID, messageID, session, operation, target)
}
func sendFailure(c telebot.Context, err error) error {
	status, category := failureDiagnostic(err)
	log.Printf("user action failed: category=%s status=%d", category, status)
	if c.Callback() != nil {
		if app, ok := c.Get("retail_bot_app").(*botApp); ok {
			action, _ := c.Get("retail_callback_action").(string)
			st, _ := c.Get("retail_callback_state").(conversation)
			return app.callbackFailure(c, action, st)
		}
	}
	return c.Send("درخواست انجام نشد. لطفاً دوباره از منو تلاش کنید یا با پشتیبانی تماس بگیرید.")
}
func failureDiagnostic(err error) (int, string) {
	var apiErr *backend.APIError
	if errors.As(err, &apiErr) {
		code := apiErr.Code
		if code == "" {
			code = "backend_error"
		}
		if len(code) > 48 {
			return apiErr.Status, "backend_error"
		}
		for _, r := range code {
			if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-') {
				return apiErr.Status, "backend_error"
			}
		}
		return apiErr.Status, code
	}
	return 0, "transport_or_internal"
}
