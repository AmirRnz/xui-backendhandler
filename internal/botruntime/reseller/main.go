package reseller

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"example.com/xui-commerce/backend/internal/botruntime/backend"
	"gopkg.in/telebot.v3"
)

var adminTelegramID int64 = 96937669

const callbackUnique = "go"

type actor struct {
	TelegramID     int64  `json:"telegram_id"`
	Role           string `json:"role"`
	ApprovalStatus string `json:"approval_status"`
}
type plan struct {
	ID                 int64          `json:"id"`
	Name               string         `json:"name"`
	Kind               string         `json:"kind"`
	Enabled            bool           `json:"enabled"`
	IsLimited          bool           `json:"is_limited"`
	Description        string         `json:"description"`
	BasePrice          int64          `json:"base_price_toman"`
	PriceExtraIP       int64          `json:"price_per_extra_ip_toman"`
	PriceGB            int64          `json:"price_per_gb_toman"`
	PriceExtraMonth    int64          `json:"price_per_extra_month_toman"`
	BaseIP             int            `json:"base_ip_limit"`
	MaxIP              int            `json:"max_ip_limit"`
	MinGB              int            `json:"min_data_gb"`
	MaxBytes           int64          `json:"max_data_bytes"`
	ExpireSeconds      int64          `json:"expire_seconds"`
	TestIPLimit        int            `json:"test_ip_limit"`
	MaxPerDay          int            `json:"max_per_day"`
	Flow               string         `json:"flow"`
	InboundIDs         []int          `json:"inbound_ids"`
	UsageDescription   string         `json:"usage_description"`
	DiscountTiers      []discountTier `json:"discount_tiers"`
	IsGlobal           bool           `json:"is_global"`
	AllowedTelegramIDs []int64        `json:"allowed_telegram_ids"`
}
type discountTier struct {
	Months      int   `json:"months"`
	BasisPoints int64 `json:"basis_points"`
}
type quote struct {
	ID    int64 `json:"id"`
	Price int64 `json:"final_price_toman"`
}
type purchase struct {
	Status   string `json:"status"`
	IntentID int64  `json:"payment_intent_id"`
	Amount   int64  `json:"amount_toman"`
}
type receiptState struct {
	Kind string
	ID   int64
}
type activeReceipt struct {
	ID        int64  `json:"id"`
	Status    string `json:"status"`
	Amount    int64  `json:"amount_toman"`
	CreatedAt string `json:"created_at"`
}
type activeReceiptPage struct {
	Items      []activeReceipt
	NextCursor *int64
}
type activeReceiptSet struct {
	Payments activeReceiptPage
	Topups   activeReceiptPage
}
type adminReviewItem struct {
	ID             int64  `json:"id"`
	AccountID      int64  `json:"account_id"`
	ActorID        int64  `json:"actor_id"`
	TelegramID     int64  `json:"telegram_id"`
	Amount         int64  `json:"amount_toman"`
	Status         string `json:"status"`
	CreatedAt      string `json:"created_at"`
	TelegramFileID string `json:"telegram_file_id"`
}
type subscription struct {
	ID                int64    `json:"id"`
	Email             string   `json:"email"`
	DisplayName       string   `json:"display_name"`
	PlanID            int64    `json:"plan_id"`
	Status            string   `json:"status"`
	Kind              string   `json:"kind"`
	IPLimit           int      `json:"ip_limit"`
	TrafficLimitBytes int64    `json:"traffic_limit_bytes"`
	ExpiryTimeMS      int64    `json:"expiry_time_ms"`
	Links             []string `json:"links"`
}
type conversation struct {
	Step             string
	Vals             map[string]string
	PlanID           int64
	PlanDraft        *plan
	DraftInboundPage int
	Method           string
	Expires          time.Time
}
type botApp struct {
	api      *backend.Client
	mu       sync.Mutex
	flows    map[int64]conversation
	receipts map[int64]receiptState
	menus    map[int64]string
}
type settings struct {
	RetailTrialResetDays      int               `json:"retail_trial_reset_days"`
	UnapprovedTrialDailyLimit int               `json:"unapproved_trial_daily_limit"`
	ResellerApprovedRequired  bool              `json:"reseller_approved_required"`
	MinTopupToman             int64             `json:"min_topup_toman"`
	Features                  map[string]bool   `json:"features"`
	Text                      map[string]string `json:"text"`
}
type paymentInstructions struct {
	CardNumber    string `json:"card_number"`
	CardOwner     string `json:"card_owner"`
	Instructions  string `json:"instructions"`
	MinTopupToman int64  `json:"min_topup_toman"`
}
type panelConfig struct {
	ID              string `json:"id"`
	BaseURL         string `json:"base_url"`
	TokenConfigured bool   `json:"token_configured"`
}
type adminConfig struct {
	DeploymentID string              `json:"deployment_id"`
	Channel      string              `json:"channel"`
	Plans        []plan              `json:"plans"`
	Payment      paymentInstructions `json:"payment_instructions"`
	Settings     settings            `json:"settings"`
	Panel        panelConfig         `json:"panel"`
}
type runtimeConfig struct {
	Features map[string]bool   `json:"features"`
	Text     map[string]string `json:"text"`
}
type accessRequestResponse struct {
	Status        string `json:"status"`
	NextRequestAt string `json:"next_request_at"`
}

var (
	errPanelPrivateChat = errors.New("panel configuration requires a private chat")
	errPanelTokenDelete = errors.New("panel token message could not be deleted")
)

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
	base, secret := strings.TrimSpace(os.Getenv("BACKEND_URL")), os.Getenv("BACKEND_TOKEN")
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
	app := &botApp{api: api, flows: map[int64]conversation{}, receipts: map[int64]receiptState{}, menus: map[int64]string{}}
	app.register(b)
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
	b.Handle("/start", func(c telebot.Context) error {
		a.clearFlow(c.Sender().ID)
		act, err := a.resolve(c)
		if err != nil {
			return a.sendFailure(c, err)
		}
		return a.home(c, act, "به پنل سرویس reseller خوش آمدید.")
	})
	b.Handle("/admin", func(c telebot.Context) error {
		if !isPrivateChat(c.Chat()) {
			return c.Send("دستور مدیریت فقط در گفت‌وگوی خصوصی در دسترس است.")
		}
		a.clearFlow(c.Sender().ID)
		act, err := a.resolve(c)
		if err != nil {
			return a.sendFailure(c, err)
		}
		if !canOpenAdmin(c.Chat(), act) {
			return a.show(c, "این بخش در دسترس نیست.")
		}
		return a.adminHome(c, act)
	})
	registerCallbackHandlers(b, a.callback)
	b.Handle(telebot.OnText, a.text)
	b.Handle(telebot.OnPhoto, a.photo)
}
func (a *botApp) resolve(c telebot.Context) (actor, error) {
	if c.Sender() == nil {
		return actor{}, fmt.Errorf("فرستنده نامشخص است")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var out actor
	err := a.api.Call(ctx, "POST", "/v1/actors/resolve", c.Sender().ID, map[string]any{"telegram_id": c.Sender().ID}, &out)
	return out, err
}
func isAdmin(act actor) bool { return act.TelegramID == adminTelegramID && act.Role == "admin" }
func canOpenAdmin(chat *telebot.Chat, act actor) bool {
	return isPrivateChat(chat) && isAdmin(act)
}
func registerCallbackHandlers(b *telebot.Bot, handler telebot.HandlerFunc) {
	callbackEndpoint := telebot.Btn{Unique: callbackUnique}
	b.Handle(&callbackEndpoint, handler)
	b.Handle(telebot.OnCallback, handler)
}
func markup(rows ...[]telebot.Btn) *telebot.ReplyMarkup {
	m := &telebot.ReplyMarkup{}
	converted := make([]telebot.Row, 0, len(rows))
	for _, row := range rows {
		converted = append(converted, telebot.Row(row))
	}
	m.Inline(converted...)
	return m
}
func btn(label, data string) telebot.Btn {
	return telebot.Btn{Text: label, Unique: callbackUnique, Data: data}
}

func mainMenuRows(runtime runtimeConfig, act actor) [][]telebot.Btn {
	support := []telebot.Btn{btn(textOr(runtime.Text, "support_button", "🆘 پشتیبانی"), "support")}
	if act.ApprovalStatus != "approved" {
		rows := make([][]telebot.Btn, 0, 3)
		if featureEnabled(runtime.Features, "trials_enabled") {
			rows = append(rows, []telebot.Btn{btn(textOr(runtime.Text, "trial_button", "🧪 دریافت تست"), "plans|test")})
		}
		if act.ApprovalStatus == "pending" {
			rows = append(rows, []telebot.Btn{btn(textOr(runtime.Text, "request_access_button", "📝 درخواست دسترسی نمایندگی"), "request-access")})
		}
		return append(rows, support)
	}

	rows := make([][]telebot.Btn, 0, 3)
	shopping := make([]telebot.Btn, 0, 2)
	if featureEnabled(runtime.Features, "trials_enabled") {
		shopping = append(shopping, btn(textOr(runtime.Text, "trial_button", "🧪 تست رایگان"), "plans|test"))
	}
	if featureEnabled(runtime.Features, "purchases_enabled") {
		shopping = append(shopping, btn(textOr(runtime.Text, "buy_button", "💼 خرید سرویس"), "plans|paid"))
	}
	if len(shopping) > 0 {
		rows = append(rows, shopping)
	}
	account := []telebot.Btn{btn(textOr(runtime.Text, "services_button", "📋 سرویس‌های من"), "services")}
	if featureEnabled(runtime.Features, "wallet_enabled") {
		account = append(account, btn(textOr(runtime.Text, "wallet_button", "👛 کیف پول"), "wallet"))
	}
	rows = append(rows, account, support)
	return rows
}

func appendAdminMenuRow(rows [][]telebot.Btn, chat *telebot.Chat, act actor) [][]telebot.Btn {
	if canOpenAdmin(chat, act) {
		return append(rows, []telebot.Btn{btn("⚙️ مدیریت", "admin")})
	}
	return rows
}

func walletMenuRows(runtime runtimeConfig) [][]telebot.Btn {
	actions := []telebot.Btn{btn("گردش کیف پول", "ledger")}
	if featureEnabled(runtime.Features, "topups_enabled") {
		actions = append(actions, btn("شارژ کیف پول", "topup"))
	}
	return [][]telebot.Btn{actions, {btn("خانه", "home")}}
}

func supportMessage(runtime runtimeConfig) string {
	support := strings.TrimSpace(runtime.Text["support_username"])
	if support == "" {
		return "برای پشتیبانی لطفا با ادمین در ارتباط باشید."
	}
	if !strings.HasPrefix(support, "@") {
		support = "@" + support
	}
	return fmt.Sprintf("برای پشتیبانی لطفا با آی‌دی زیر در ارتباط باشید:\n%s", support)
}

func accessRequestMessage(act actor) string {
	switch act.ApprovalStatus {
	case "pending":
		return "وضعیت حساب شما در انتظار تایید مدیر است. اگر هنوز درخواست دسترسی نداده‌اید، برای ثبت آن با پشتیبانی تماس بگیرید."
	case "approved":
		return "شما قبلا درخواست دسترسی داده‌اید یا تایید شده‌اید."
	case "rejected":
		return "درخواست دسترسی شما تایید نشده است. برای پیگیری با پشتیبانی ارتباط بگیرید."
	default:
		return "وضعیت دسترسی شما مشخص نیست. برای پیگیری با پشتیبانی ارتباط بگیرید."
	}
}

func accessRequestResultMessage(result accessRequestResponse) (string, error) {
	switch result.Status {
	case "submitted":
		return "✅ درخواست دسترسی شما ثبت شد و برای بررسی مدیر ارسال خواهد شد.", nil
	case "already_pending":
		message := "درخواست قبلی شما در انتظار بررسی مدیر است."
		if next, err := time.Parse(time.RFC3339, result.NextRequestAt); err == nil {
			message += " امکان ارسال یادآوری بعدی از " + next.UTC().Format("2006-01-02 15:04 UTC") + " وجود دارد."
		}
		return message, nil
	default:
		return "", fmt.Errorf("backend returned an unknown access request status")
	}
}

func (a *botApp) show(c telebot.Context, what interface{}, opts ...interface{}) error {
	token := callbackToken()
	if c.Sender() != nil {
		a.mu.Lock()
		if a.menus == nil {
			a.menus = map[int64]string{}
		}
		a.menus[c.Sender().ID] = token
		a.mu.Unlock()
	}
	for _, opt := range opts {
		if keyboard, ok := opt.(*telebot.ReplyMarkup); ok && keyboard != nil {
			bindMenuToken(keyboard, token)
		}
	}
	err := c.EditOrSend(what, opts...)
	if err == nil {
		return nil
	}
	message := strings.ToLower(err.Error())
	if strings.Contains(message, "message is not modified") || strings.Contains(message, "message to edit not found") || strings.Contains(message, "message can't be edited") {
		return c.Send(what, opts...)
	}
	return err
}
func bindMenuToken(keyboard *telebot.ReplyMarkup, token string) {
	if keyboard == nil {
		return
	}
	for row := range keyboard.InlineKeyboard {
		for button := range keyboard.InlineKeyboard[row] {
			if keyboard.InlineKeyboard[row][button].Data != "" {
				keyboard.InlineKeyboard[row][button].Data += "|v" + token
			}
		}
	}
}
func callbackToken() string {
	var raw [6]byte
	if _, err := rand.Read(raw[:]); err == nil {
		return hex.EncodeToString(raw[:])
	}
	sum := sha256.Sum256([]byte(strconv.FormatInt(time.Now().UnixNano(), 10)))
	return hex.EncodeToString(sum[:6])
}
func (a *botApp) callbackData(userID int64, raw string) ([]string, bool) {
	// Telegram sends the callback unique name as a control-prefixed first field.
	// telebot normally removes it only when a handler is registered for that
	// unique name; the catch-all callback handler can receive the wire form.
	prefix := "\f" + callbackUnique + "|"
	if strings.HasPrefix(raw, prefix) {
		raw = strings.TrimPrefix(raw, prefix)
	} else if strings.HasPrefix(raw, "\f") {
		return nil, false
	}
	parts := strings.Split(raw, "|")
	if len(parts) < 2 || !strings.HasPrefix(parts[len(parts)-1], "v") {
		return nil, false
	}
	token := strings.TrimPrefix(parts[len(parts)-1], "v")
	a.mu.Lock()
	current := a.menus[userID]
	if token == "" || current == "" || token != current {
		a.mu.Unlock()
		return nil, false
	}
	delete(a.menus, userID)
	a.mu.Unlock()
	return parts[:len(parts)-1], true
}
func (a *botApp) home(c telebot.Context, act actor, message string) error {
	runtime := a.runtime(c, act.TelegramID)
	rows := appendAdminMenuRow(mainMenuRows(runtime, act), c.Chat(), act)
	homepage := message == "صفحه اصلی" || message == "به پنل سرویس reseller خوش آمدید."
	if homepage {
		if pending, err := a.activeReceipts(c, act.TelegramID); err == nil && len(pending) > 0 {
			rows = append([][]telebot.Btn{{btn("🧾 ادامه پرداخت یا شارژ", "resume")}}, rows...)
		}
		if act.ApprovalStatus == "approved" {
			message = textOr(runtime.Text, "home_title", "👋 به پنل کاربری خوش آمدید\nسرویس وی‌پی‌ان خود را مدیریت کنید یا سرویس جدید خریداری نمایید.")
		} else {
			message = textOr(runtime.Text, "home_title", "👋 به ربات نمایندگی خوش آمدید\nبرای دسترسی به امکانات کامل خرید و مدیریت سرویس، لطفا درخواست دسترسی خود را ثبت کنید.")
		}
	}
	return a.show(c, message, markup(rows...))
}
func (a *botApp) runtime(c telebot.Context, actorID int64) runtimeConfig {
	var cfg runtimeConfig
	if err := a.call(c, "GET", "/v1/features", actorID, nil, &cfg); err != nil {
		return runtimeConfig{}
	}
	return cfg
}
func textOr(values map[string]string, key, fallback string) string {
	if value := strings.TrimSpace(values[key]); value != "" {
		return value
	}
	return fallback
}
func (a *botApp) callback(c telebot.Context) error {
	if c.Sender() == nil {
		if err := c.Respond(); err != nil {
			log.Printf("callback acknowledgement failed: %v", err)
		}
		return nil
	}
	data, current := a.callbackData(c.Sender().ID, c.Data())
	if !current {
		if err := c.Respond(&telebot.CallbackResponse{Text: "این دکمه قدیمی شده است؛ از آخرین منوی ارسال‌شده استفاده کنید."}); err != nil {
			log.Printf("stale callback acknowledgement failed: %v", err)
		}
		return nil
	}
	if err := c.Respond(); err != nil {
		log.Printf("callback acknowledgement failed: %v", err)
	}
	act, err := a.resolve(c)
	if err != nil {
		return a.sendFailure(c, err)
	}
	if isAdmin(act) && adminCallbackAction(data[0]) && !isPrivateChat(c.Chat()) {
		return a.show(c, "گزینه‌های مدیریت فقط در گفت‌وگوی خصوصی در دسترس هستند.", markup([]telebot.Btn{btn("خانه", "home")}))
	}
	switch data[0] {
	case "home":
		a.clearFlow(act.TelegramID)
		return a.homeFor(c, "صفحه اصلی")
	case "resume":
		return a.showResumeMenu(c, act)
	case "resume-list":
		if len(data) < 4 {
			return a.home(c, act, "پارامتر فهرست فاکتورها نامعتبر است.")
		}
		beforeID, beforeErr := strconv.ParseInt(data[2], 10, 64)
		offset, offsetErr := strconv.Atoi(data[3])
		if beforeErr != nil || offsetErr != nil || beforeID < 0 || offset < 0 {
			return a.home(c, act, "پارامتر فهرست فاکتورها نامعتبر است.")
		}
		return a.showResumeList(c, act, data[1], beforeID, offset)
	case "resume-receipt":
		if len(data) < 3 {
			return a.home(c, act, "شناسه فاکتور نامعتبر است.")
		}
		id, err := strconv.ParseInt(data[2], 10, 64)
		if err != nil || id <= 0 {
			return a.home(c, act, "شناسه فاکتور نامعتبر است.")
		}
		return a.resumeReceipt(c, act, data[1], id)
	case "wallet":
		return a.wallet(c, act)
	case "ledger":
		return a.ledger(c, act)
	case "topup":
		features := a.runtime(c, act.TelegramID)
		if !featureEnabled(features.Features, "topups_enabled") {
			return a.home(c, act, "شارژ کیف پول در حال حاضر غیرفعال است.")
		}
		var instructions paymentInstructions
		if err := a.call(c, "GET", "/v1/payment-instructions", act.TelegramID, nil, &instructions); err != nil {
			return a.sendFailure(c, err)
		}
		a.setFlow(act.TelegramID, conversation{Step: "topup", Vals: map[string]string{"min_topup_toman": strconv.FormatInt(instructions.MinTopupToman, 10)}, Expires: time.Now().Add(20 * time.Minute)})
		return a.show(c, topupInstructionsText(instructions), markup([]telebot.Btn{btn("لغو", "home")}))
	case "services":
		return a.services(c, act)
	case "support":
		return a.show(c, supportMessage(a.runtime(c, act.TelegramID)), markup([]telebot.Btn{btn("بازگشت", "home")}))
	case "request-access":
		if act.Role != "reseller" || act.ApprovalStatus != "pending" {
			return a.show(c, accessRequestMessage(act), markup([]telebot.Btn{btn("بازگشت", "home")}))
		}
		var result accessRequestResponse
		if err := a.call(c, "POST", "/v1/reseller/access-requests", act.TelegramID, map[string]any{}, &result); err != nil {
			return a.sendFailure(c, err)
		}
		message, err := accessRequestResultMessage(result)
		if err != nil {
			return a.sendFailure(c, err)
		}
		return a.show(c, message, markup([]telebot.Btn{btn("بازگشت", "home")}))
	case "service":
		if len(data) < 2 {
			return a.homeFor(c, "شناسه سرویس نامعتبر است.")
		}
		id, e := strconv.ParseInt(data[1], 10, 64)
		if e != nil || id <= 0 {
			return a.homeFor(c, "شناسه سرویس نامعتبر است.")
		}
		return a.subscriptionDetails(c, act, id, 0)
	case "servicepage":
		if len(data) < 3 {
			return a.homeFor(c, "صفحه سرویس نامعتبر است.")
		}
		id, e1 := strconv.ParseInt(data[1], 10, 64)
		page, e2 := strconv.Atoi(data[2])
		if e1 != nil || e2 != nil || id <= 0 || page < 0 {
			return a.homeFor(c, "صفحه سرویس نامعتبر است.")
		}
		return a.subscriptionDetails(c, act, id, page)
	case "plans":
		if len(data) < 2 {
			return a.homeFor(c, "انتخاب نامعتبر است.")
		}
		return a.showPlans(c, act, data[1])
	case "trial":
		if len(data) < 2 {
			return a.homeFor(c, "انتخاب نامعتبر است.")
		}
		id, e := strconv.ParseInt(data[1], 10, 64)
		if e != nil {
			return a.homeFor(c, "طرح نامعتبر است.")
		}
		return a.submitTrial(c, act, id)
	case "select":
		if len(data) < 2 {
			return a.homeFor(c, "انتخاب نامعتبر است.")
		}
		id, e := strconv.ParseInt(data[1], 10, 64)
		if e != nil {
			return a.homeFor(c, "طرح نامعتبر است.")
		}
		return a.startPurchase(c, act, id)
	case "duration":
		if len(data) < 2 {
			return a.homeFor(c, "مدت اشتراک نامعتبر است.")
		}
		months, e := strconv.Atoi(data[1])
		if e != nil || months < 1 || months > 36 {
			return a.homeFor(c, "مدت اشتراک باید بین ۱ تا ۳۶ ماه باشد.")
		}
		st, ok := a.currentPurchaseFlow(act.TelegramID, "months")
		if !ok {
			return a.homeFor(c, "فرآیند خرید منقضی شده است.")
		}
		st.Vals["months"] = strconv.Itoa(months)
		return a.advancePurchase(c, act, st)
	case "duration-custom":
		st, ok := a.currentPurchaseFlow(act.TelegramID, "months")
		if !ok {
			return a.homeFor(c, "فرآیند خرید منقضی شده است.")
		}
		st.Step = "months"
		return a.setFlowAndPrompt(c, act.TelegramID, st, "تعداد ماه را بین ۱ تا ۳۶ وارد کنید.")
	case "back-duration":
		st, ok := a.purchaseFlow(act.TelegramID)
		if !ok {
			return a.homeFor(c, "فرآیند خرید منقضی شده است.")
		}
		p, e := a.planByID(c, st.PlanID)
		if e != nil {
			return a.sendFailure(c, e)
		}
		st.Step = "months"
		a.setFlow(act.TelegramID, st)
		return a.showPurchaseDuration(c, p)
	case "back-data":
		st, ok := a.purchaseFlow(act.TelegramID)
		if !ok {
			return a.homeFor(c, "فرآیند خرید منقضی شده است.")
		}
		p, e := a.planByID(c, st.PlanID)
		if e != nil {
			return a.sendFailure(c, e)
		}
		return a.showPurchaseData(c, p, st)
	case "back-ip":
		st, ok := a.purchaseFlow(act.TelegramID)
		if !ok {
			return a.homeFor(c, "فرآیند خرید منقضی شده است.")
		}
		return a.showPurchaseIP(c, act, st)
	case "data":
		if len(data) < 2 {
			return a.homeFor(c, "حجم انتخاب شده نامعتبر است.")
		}
		gb, e := strconv.Atoi(data[1])
		if e != nil || gb < 0 || gb > 100000 {
			return a.homeFor(c, "حجم انتخاب شده نامعتبر است.")
		}
		st, ok := a.currentPurchaseFlow(act.TelegramID, "gb")
		if !ok {
			return a.homeFor(c, "فرآیند خرید منقضی شده است.")
		}
		st.Vals["gb"] = strconv.Itoa(gb)
		return a.showPurchaseIP(c, act, st)
	case "data-custom":
		st, ok := a.currentPurchaseFlow(act.TelegramID, "gb")
		if !ok {
			return a.homeFor(c, "فرآیند خرید منقضی شده است.")
		}
		st.Step = "gb"
		return a.setFlowAndPrompt(c, act.TelegramID, st, "حجم دلخواه را به GB وارد کنید.")
	case "ip":
		if len(data) < 2 {
			return a.homeFor(c, "محدودیت IP نامعتبر است.")
		}
		ip, e := strconv.Atoi(data[1])
		if e != nil || ip < 0 {
			return a.homeFor(c, "محدودیت IP نامعتبر است.")
		}
		st, ok := a.currentPurchaseFlow(act.TelegramID, "ip")
		if !ok {
			return a.homeFor(c, "فرآیند خرید منقضی شده است.")
		}
		p, err := a.planByID(c, st.PlanID)
		if err != nil {
			return a.sendFailure(c, err)
		}
		if !validPurchaseIPLimit(ip, p) {
			return a.showPurchaseIP(c, act, st)
		}
		st.Vals["ip"] = strconv.Itoa(ip)
		return a.promptPurchaseName(c, act, st)
	case "ip-custom":
		st, ok := a.currentPurchaseFlow(act.TelegramID, "ip")
		if !ok {
			return a.homeFor(c, "فرآیند خرید منقضی شده است.")
		}
		p, err := a.planByID(c, st.PlanID)
		if err != nil {
			return a.sendFailure(c, err)
		}
		st.Step = "ip-number"
		return a.setFlowAndPrompt(c, act.TelegramID, st, fmt.Sprintf("تعداد IP هم‌زمان را بین %d تا %d وارد کنید.", p.BaseIP, p.MaxIP))
	case "purchase-default-name":
		st, ok := a.currentPurchaseFlow(act.TelegramID, "name")
		if !ok {
			return a.homeFor(c, "فرآیند خرید منقضی شده است.")
		}
		return a.createPurchaseQuote(c, act, st, "")
	case "confirm-purchase":
		if len(data) < 2 || (data[1] != "wallet" && data[1] != "direct") {
			return a.homeFor(c, "روش پرداخت نامعتبر است.")
		}
		st, ok := a.currentPurchaseFlow(act.TelegramID, "purchase-confirm")
		if !ok {
			return a.homeFor(c, "پیش‌فاکتور منقضی شده است. خرید را دوباره آغاز کنید.")
		}
		return a.confirmPurchase(c, act, st, data[1])
	case "cancel-purchase":
		a.clearFlow(act.TelegramID)
		return a.home(c, act, "خرید لغو شد.")
	case "retry-topup":
		st, ok := a.currentPurchaseFlow(act.TelegramID, "topup-submit")
		if !ok {
			return a.home(c, act, "درخواست شارژ منقضی شده است. از منوی کیف پول دوباره شروع کنید.")
		}
		amount, err := strconv.ParseInt(st.Vals["amount_toman"], 10, 64)
		if err != nil || amount <= 0 || st.Vals["operation_key"] == "" {
			return a.home(c, act, "درخواست شارژ نامعتبر است. از منوی کیف پول دوباره شروع کنید.")
		}
		return a.createTopup(c, act, amount, st.Vals["operation_key"])
	case "receipt":
		if len(data) < 3 {
			return a.homeFor(c, "شناسه نامعتبر است.")
		}
		id, e := strconv.ParseInt(data[2], 10, 64)
		if e != nil {
			return a.homeFor(c, "شناسه نامعتبر است.")
		}
		a.mu.Lock()
		a.receipts[act.TelegramID] = receiptState{Kind: data[1], ID: id}
		a.mu.Unlock()
		return a.show(c, "اکنون عکس رسید را ارسال کنید.", markup([]telebot.Btn{btn("لغو", "home")}))
	case "cancel":
		if len(data) < 2 {
			return a.homeFor(c, "شناسه نامعتبر است.")
		}
		id, e := strconv.ParseInt(data[1], 10, 64)
		if e != nil {
			return a.homeFor(c, "شناسه نامعتبر است.")
		}
		return a.show(c, fmt.Sprintf("درخواست لغو سرویس %d را تأیید می‌کنید؟", id), markup([]telebot.Btn{btn("تأیید لغو", fmt.Sprintf("cancelconfirm|%d", id)), btn("بازگشت", "services")}, []telebot.Btn{btn("خانه", "home")}))
	case "cancelconfirm":
		if len(data) < 2 {
			return a.home(c, act, "شناسه سرویس نامعتبر است.")
		}
		id, e := strconv.ParseInt(data[1], 10, 64)
		if e != nil || id <= 0 {
			return a.home(c, act, "شناسه سرویس نامعتبر است.")
		}
		return a.cancelSubscription(c, act, id)
	case "admin":
		if !isAdmin(act) {
			return a.homeFor(c, "این بخش در دسترس نیست.")
		}
		return a.adminHome(c, act)
	case "work-items":
		if !isAdmin(act) {
			return a.homeFor(c, "این بخش در دسترس نیست.")
		}
		return a.showWorkItems(c, act)
	case "refunds":
		if !isAdmin(act) {
			return a.homeFor(c, "این بخش در دسترس نیست.")
		}
		return a.showRefunds(c, act)
	case "review-refund":
		if !isAdmin(act) || len(data) < 2 {
			return a.homeFor(c, "این بخش در دسترس نیست.")
		}
		id, e := strconv.ParseInt(data[1], 10, 64)
		if e != nil || id <= 0 {
			return a.home(c, act, "شناسه استرداد نامعتبر است.")
		}
		return a.reviewRefund(c, act, id)
	case "reject-refund":
		if !isAdmin(act) || len(data) < 2 {
			return a.homeFor(c, "این بخش در دسترس نیست.")
		}
		id, e := strconv.ParseInt(data[1], 10, 64)
		if e != nil || id <= 0 {
			return a.home(c, act, "شناسه استرداد نامعتبر است.")
		}
		return a.show(c, fmt.Sprintf("رد درخواست استرداد شماره %d را تأیید می‌کنید؟", id), markup([]telebot.Btn{btn("بله، رد شود", fmt.Sprintf("confirm-refund-reject|%d", id))}, []telebot.Btn{btn("بازگشت", "refunds")}))
	case "confirm-refund-reject":
		if !isAdmin(act) || len(data) < 2 {
			return a.homeFor(c, "این بخش در دسترس نیست.")
		}
		id, e := strconv.ParseInt(data[1], 10, 64)
		if e != nil || id <= 0 {
			return a.home(c, act, "شناسه استرداد نامعتبر است.")
		}
		path, ok := adminRefundRejectPath(id)
		if !ok {
			return a.home(c, act, "شناسه استرداد نامعتبر است.")
		}
		return a.adminAction(c, act, path)
	case "pending", "pendingtopups":
		if !isAdmin(act) {
			return a.homeFor(c, "این بخش در دسترس نیست.")
		}
		return a.showPending(c, act, data[0], 0)
	case "pendingpage":
		if !canOpenAdmin(c.Chat(), act) || len(data) < 3 {
			return a.homeFor(c, "این بخش در دسترس نیست.")
		}
		page, err := parseAdminPage(data[2])
		if err != nil || (data[1] != "pending" && data[1] != "pendingtopups") {
			return a.home(c, act, "صفحه درخواست نامعتبر است.")
		}
		return a.showPending(c, act, data[1], page)
	case "resellerpage":
		if !canOpenAdmin(c.Chat(), act) || len(data) < 2 {
			return a.homeFor(c, "این بخش در دسترس نیست.")
		}
		page, err := parseAdminPage(data[1])
		if err != nil {
			return a.home(c, act, "صفحه درخواست نامعتبر است.")
		}
		return a.pendingResellersPage(c, act, page)
	case "resellers":
		if !isAdmin(act) {
			return a.homeFor(c, "این بخش در دسترس نیست.")
		}
		return a.pendingResellers(c, act)
	case "resapprove", "resreject":
		if !isAdmin(act) || len(data) < 2 {
			return a.homeFor(c, "این بخش در دسترس نیست.")
		}
		telegramID, e := strconv.ParseInt(data[1], 10, 64)
		if e != nil || telegramID <= 0 {
			return a.homeFor(c, "شناسه reseller نامعتبر است.")
		}
		return a.reviewReseller(c, act, telegramID, data[0])
	case "approve", "approvetopup":
		if !isAdmin(act) || len(data) < 2 {
			return a.homeFor(c, "این بخش در دسترس نیست.")
		}
		id, e := strconv.ParseInt(data[1], 10, 64)
		if e != nil {
			return a.homeFor(c, "شناسه نامعتبر است.")
		}
		path := fmt.Sprintf("/v1/payment-intents/%d/approve", id)
		if data[0] == "approvetopup" {
			path = fmt.Sprintf("/v1/wallet/topups/%d/approve", id)
		}
		return a.adminAction(c, act, path)
	case "review-payment", "review-topup":
		if !isAdmin(act) || len(data) < 2 {
			return a.homeFor(c, "این بخش در دسترس نیست.")
		}
		id, e := strconv.ParseInt(data[1], 10, 64)
		if e != nil || id <= 0 {
			return a.home(c, act, "شناسه درخواست نامعتبر است.")
		}
		kind := "payment"
		if data[0] == "review-topup" {
			kind = "topup"
		}
		return a.reviewPendingItem(c, act, kind, id)
	case "reject-payment", "reject-topup":
		if !isAdmin(act) || len(data) < 2 {
			return a.homeFor(c, "این بخش در دسترس نیست.")
		}
		id, e := strconv.ParseInt(data[1], 10, 64)
		if e != nil || id <= 0 {
			return a.home(c, act, "شناسه درخواست نامعتبر است.")
		}
		kind := "payment"
		if data[0] == "reject-topup" {
			kind = "topup"
		}
		return a.show(c, fmt.Sprintf("رد درخواست %s شماره %d را تأیید می‌کنید؟", kind, id), markup([]telebot.Btn{btn("بله، رد شود", fmt.Sprintf("confirm-review-reject|%s|%d", kind, id))}, []telebot.Btn{btn("بازگشت", fmt.Sprintf("review-%s|%d", kind, id))}))
	case "confirm-review-reject":
		if !isAdmin(act) || len(data) < 3 {
			return a.homeFor(c, "این بخش در دسترس نیست.")
		}
		id, e := strconv.ParseInt(data[2], 10, 64)
		if e != nil || id <= 0 {
			return a.home(c, act, "شناسه درخواست نامعتبر است.")
		}
		path, ok := adminRejectPath(data[1], id)
		if !ok {
			return a.home(c, act, "نوع درخواست نامعتبر است.")
		}
		return a.adminAction(c, act, path)
	case "config":
		if !isAdmin(act) {
			return a.homeFor(c, "این بخش در دسترس نیست.")
		}
		return a.showConfig(c, act)
	case "cfg":
		if !isAdmin(act) || len(data) < 2 {
			return a.homeFor(c, "این بخش در دسترس نیست.")
		}
		if data[1] == "panel" && !isPrivateChat(c.Chat()) {
			return a.show(c, "تنظیم پنل فقط در گفت‌وگوی خصوصی در دسترس است.", markup([]telebot.Btn{btn("بازگشت", "config")}))
		}
		return a.configSection(c, act, data[1])
	case "cfgset":
		if !isAdmin(act) || len(data) < 3 {
			return a.homeFor(c, "این بخش در دسترس نیست.")
		}
		if data[1] == "panel" && !isPrivateChat(c.Chat()) {
			return a.show(c, "تنظیم پنل فقط در گفت‌وگوی خصوصی در دسترس است.", markup([]telebot.Btn{btn("بازگشت", "config")}))
		}
		return a.startConfigEdit(c, act, data[1], data[2])
	case "planedit":
		if !isAdmin(act) || len(data) < 2 {
			return a.homeFor(c, "این بخش در دسترس نیست.")
		}
		return a.planEditor(c, act, data[1])
	case "planfield":
		if !isAdmin(act) || len(data) < 3 {
			return a.homeFor(c, "این بخش در دسترس نیست.")
		}
		return a.startPlanEdit(c, act, data[1], data[2])
	case "plannewkind":
		if !isAdmin(act) || len(data) != 2 || !isPrivateChat(c.Chat()) {
			return a.homeFor(c, "این بخش فقط برای مدیر و در گفت‌وگوی خصوصی در دسترس است.")
		}
		return a.choosePlanDraftKind(c, act, data[1])
	case "plandraft":
		if !isAdmin(act) || len(data) < 2 || !isPrivateChat(c.Chat()) {
			return a.homeFor(c, "این بخش فقط برای مدیر و در گفت‌وگوی خصوصی در دسترس است.")
		}
		return a.handlePlanDraftAction(c, act, data[1:])
	case "planinbound":
		if !isAdmin(act) || len(data) < 2 || !isPrivateChat(c.Chat()) {
			return a.homeFor(c, "این بخش فقط برای مدیر و در گفت‌وگوی خصوصی در دسترس است.")
		}
		return a.handlePlanInboundAction(c, act, data[1:])
	case "feature":
		if !isAdmin(act) || len(data) < 2 {
			return a.homeFor(c, "این بخش در دسترس نیست.")
		}
		return a.toggleFeature(c, act, data[1])
	default:
		return a.homeFor(c, "این دکمه منقضی یا نامعتبر است.")
	}
}
func (a *botApp) homeFor(c telebot.Context, msg string) error {
	act, err := a.resolve(c)
	if err != nil {
		return a.sendFailure(c, err)
	}
	return a.home(c, act, msg)
}
func (a *botApp) showPlans(c telebot.Context, act actor, kind string) error {
	if kind != "paid" && kind != "test" {
		return a.home(c, act, "انتخاب نامعتبر است.")
	}
	var items []plan
	if err := a.call(c, "GET", "/v1/plans?kind="+kind, act.TelegramID, nil, &items); err != nil {
		return a.sendFailure(c, err)
	}
	if len(items) == 0 {
		return a.show(c, "در حال حاضر طرحی در دسترس نیست.", markup([]telebot.Btn{btn("خانه", "home")}))
	}
	rows := make([][]telebot.Btn, 0, len(items)+1)
	var summary strings.Builder
	for _, p := range items {
		fmt.Fprintf(&summary, "%s\n", formatPlanDetails(p, kind))
		label := p.Name
		if kind == "test" {
			label = fmt.Sprintf("🧪 %s · تست روزانه", p.Name)
		} else if p.IsLimited {
			label = fmt.Sprintf("%s · هر GB %d تومان", p.Name, p.PriceGB)
		} else {
			label = fmt.Sprintf("%s · پایه %d تومان/ماه", p.Name, p.BasePrice)
		}
		action := "select"
		if kind == "test" {
			action = "trial"
		}
		rows = append(rows, []telebot.Btn{btn(label, fmt.Sprintf("%s|%d", action, p.ID))})
	}
	rows = append(rows, []telebot.Btn{btn("خانه", "home")})
	title := "طرح‌های خرید"
	if kind == "test" {
		title = "طرح‌های تست. هر درخواست یک کاربر است؛ تست گروهی غیرفعال است."
	} else {
		title = "طرح‌های خرید\n\n" + strings.TrimSpace(summary.String())
	}
	if kind == "test" {
		title += "\n\n" + strings.TrimSpace(summary.String())
	}
	return a.show(c, title, markup(rows...))
}

func (a *botApp) planByID(c telebot.Context, id int64) (plan, error) {
	if id <= 0 {
		return plan{}, fmt.Errorf("invalid plan id")
	}
	var items []plan
	actorID := int64(0)
	if c.Sender() != nil {
		actorID = c.Sender().ID
	}
	if err := a.call(c, "GET", "/v1/plans?kind=paid", actorID, nil, &items); err != nil {
		return plan{}, err
	}
	for _, item := range items {
		if item.ID == id {
			return item, nil
		}
	}
	return plan{}, fmt.Errorf("plan not found")
}
func (a *botApp) submitTrial(c telebot.Context, act actor, id int64) error {
	key := a.operationKey(c, fmt.Sprintf("trial-%d", id))
	var out purchase
	if err := a.call(c, "POST", "/v1/trials", act.TelegramID, map[string]any{"plan_id": id, "idempotency_key": key}, &out); err != nil {
		return a.sendFailure(c, err)
	}
	return a.show(c, "درخواست تست ثبت شد و برای بررسی سهمیه روزانه و وضعیت تأیید reseller پردازش می‌شود.", markup([]telebot.Btn{btn("طرح‌های تست", "plans|test"), btn("خانه", "home")}))
}
func (a *botApp) startPurchase(c telebot.Context, act actor, id int64) error {
	p, err := a.planByID(c, id)
	if err != nil {
		return a.sendFailure(c, err)
	}
	st := conversation{Step: "months", PlanID: id, Vals: map[string]string{}, Expires: time.Now().Add(20 * time.Minute)}
	a.setFlow(act.TelegramID, st)
	return a.showPurchaseDuration(c, p)
}
func (a *botApp) text(c telebot.Context) error {
	if c.Sender() == nil {
		return nil
	}
	raw := strings.TrimSpace(c.Text())
	if strings.HasPrefix(raw, "/") {
		return a.homeFor(c, "برای استفاده از منو دکمه‌ها را انتخاب کنید.")
	}
	act, err := a.resolve(c)
	if err != nil {
		return a.sendFailure(c, err)
	}
	a.mu.Lock()
	st, ok := a.flows[act.TelegramID]
	a.mu.Unlock()
	if !ok || time.Now().After(st.Expires) {
		a.clearFlow(act.TelegramID)
		return a.home(c, act, "از منو گزینه‌ای را انتخاب کنید.")
	}
	if isAdmin(act) && adminFlowStep(st.Step) && !isPrivateChat(c.Chat()) {
		a.clearFlow(act.TelegramID)
		return c.Send("تنظیمات مدیریت فقط در گفت‌وگوی خصوصی قابل تغییر هستند. از /admin در گفت‌وگوی خصوصی شروع کنید.")
	}
	if st.Step == "paneltoken" {
		err := submitPanelToken(c.Chat(), c.Delete, func() error {
			payload := map[string]string{"base_url": st.Vals["base_url"], "token": raw}
			return a.call(c, "PUT", "/v1/admin/config/panel", act.TelegramID, payload, nil)
		})
		a.clearFlow(act.TelegramID)
		switch {
		case errors.Is(err, errPanelPrivateChat):
			return c.Send("اطلاعات اتصال پنل فقط در گفت‌وگوی خصوصی پذیرفته می‌شود. تنظیم را از منوی خصوصی دوباره آغاز کنید.")
		case errors.Is(err, errPanelTokenDelete):
			return c.Send("پیام توکن حذف نشد و هیچ تغییری ذخیره نشد. لطفاً پیام را خودتان حذف کنید و دوباره در گفت‌وگوی خصوصی تلاش کنید.")
		case err != nil:
			return a.show(c, "ذخیره تنظیمات پنل انجام نشد. مقدار توکن را بررسی کنید و دوباره تلاش کنید.", markup([]telebot.Btn{btn("بازگشت", "config")}))
		default:
			return a.show(c, "تنظیمات پنل ذخیره شد.", markup([]telebot.Btn{btn("بازگشت", "config")}))
		}
	}
	if st.Step == "cfgvalue" && st.Vals["section"] == "panel" && !isPrivateChat(c.Chat()) {
		a.clearFlow(act.TelegramID)
		return c.Send("نشانی پنل فقط در گفت‌وگوی خصوصی پذیرفته می‌شود. تنظیم را از منوی خصوصی دوباره آغاز کنید.")
	}
	if raw == "لغو" {
		a.clearFlow(act.TelegramID)
		return a.home(c, act, "عملیات لغو شد.")
	}
	switch st.Step {
	case "months":
		months, e := strconv.Atoi(raw)
		if e != nil || months < 1 || months > 36 {
			return a.setFlowAndPrompt(c, act.TelegramID, st, "مدت را به‌صورت عددی بین ۱ تا ۳۶ ماه بفرستید.")
		}
		st.Vals["months"] = strconv.Itoa(months)
		return a.advancePurchase(c, act, st)
	case "gb":
		gb, e := strconv.Atoi(raw)
		if e != nil || gb < 0 || gb > 100000 {
			return a.setFlowAndPrompt(c, act.TelegramID, st, "حجم باید عددی بین صفر تا ۱۰۰۰۰۰ گیگابایت باشد.")
		}
		p, e := a.planByID(c, st.PlanID)
		if e != nil {
			return a.sendFailure(c, e)
		}
		if gb < p.MinGB {
			return a.setFlowAndPrompt(c, act.TelegramID, st, fmt.Sprintf("حداقل حجم این طرح %d گیگابایت است. مقدار دیگری بفرستید.", p.MinGB))
		}
		st.Vals["gb"] = strconv.Itoa(gb)
		return a.showPurchaseIP(c, act, st)
	case "name":
		name := strings.TrimSpace(raw)
		if name == "-" {
			name = ""
		} else if name == "" || utf8.RuneCountInString(name) > 64 {
			return a.setFlowAndPrompt(c, act.TelegramID, st, "نام سرویس باید بین ۱ تا ۶۴ نویسه باشد؛ برای نام پیش‌فرض «-» بفرستید.")
		}
		return a.createPurchaseQuote(c, act, st, name)
	case "ip-number":
		ip, e := strconv.Atoi(raw)
		if e != nil {
			return a.setFlowAndPrompt(c, act.TelegramID, st, "تعداد IP را به‌صورت عدد صحیح وارد کنید.")
		}
		p, e := a.planByID(c, st.PlanID)
		if e != nil {
			return a.sendFailure(c, e)
		}
		if !validPurchaseIPLimit(ip, p) {
			return a.setFlowAndPrompt(c, act.TelegramID, st, fmt.Sprintf("محدودیت IP باید بین %d و %d باشد.", p.BaseIP, p.MaxIP))
		}
		st.Vals["ip"] = strconv.Itoa(ip)
		return a.promptPurchaseName(c, act, st)
	case "topup":
		amount, e := strconv.ParseInt(raw, 10, 64)
		min, _ := strconv.ParseInt(st.Vals["min_topup_toman"], 10, 64)
		if e != nil || amount <= 0 {
			return a.setFlowAndPrompt(c, act.TelegramID, st, "مبلغ شارژ را به تومان و به شکل عدد مثبت وارد کنید.")
		}
		if err := validateTopupAmount(amount, min); err != nil {
			return a.setFlowAndPrompt(c, act.TelegramID, st, fmt.Sprintf("حداقل مبلغ شارژ %s تومان است. مبلغ دیگری وارد کنید.", formatToman(min)))
		}
		st.Step = "topup-submit"
		st.Vals["amount_toman"] = strconv.FormatInt(amount, 10)
		st.Vals["operation_key"] = a.operationKey(c, "topup")
		st.Expires = time.Now().Add(20 * time.Minute)
		a.setFlow(act.TelegramID, st)
		return a.createTopup(c, act, amount, st.Vals["operation_key"])
	case "cancelid":
		id, e := strconv.ParseInt(raw, 10, 64)
		if e != nil || id <= 0 {
			return a.show(c, "شناسه اشتراک معتبر وارد کنید.")
		}
		a.clearFlow(act.TelegramID)
		return a.cancelSubscription(c, act, id)
	case "cfgvalue":
		if !isAdmin(act) {
			return a.home(c, act, "این بخش در دسترس نیست.")
		}
		return a.saveConfigValue(c, act, st, raw)
	case "cfgtextkey":
		if !isAdmin(act) {
			return a.home(c, act, "این بخش در دسترس نیست.")
		}
		if !validConfigKey(raw) {
			return a.show(c, "کلید فقط می‌تواند شامل حروف کوچک انگلیسی، عدد و زیرخط باشد.", markup([]telebot.Btn{btn("لغو", "config")}))
		}
		st.Vals["field"] = raw
		st.Step = "cfgvalue"
		st.Expires = time.Now().Add(20 * time.Minute)
		a.setFlow(act.TelegramID, st)
		return a.show(c, "متن جدید را وارد کنید.", markup([]telebot.Btn{btn("لغو", "config")}))
	case "planvalue":
		if !isAdmin(act) {
			return a.home(c, act, "این بخش در دسترس نیست.")
		}
		return a.savePlanValue(c, act, st, raw)
	case "plan-draft-name":
		if !isAdmin(act) || !isPrivateChat(c.Chat()) || st.PlanDraft == nil {
			return a.home(c, act, "پیش‌نویس طرح پیدا نشد. از منوی مدیریت دوباره شروع کنید.")
		}
		name := strings.TrimSpace(raw)
		if name == "" || len([]byte(name)) > 120 {
			return a.setFlowAndPrompt(c, act.TelegramID, st, "نام طرح باید بین ۱ تا ۱۲۰ بایت باشد. نام را دوباره بفرستید.")
		}
		st.PlanDraft.Name = name
		st.Step = ""
		st.Expires = time.Now().Add(20 * time.Minute)
		a.setFlow(act.TelegramID, st)
		return a.showPlanDraftKind(c, act, st.PlanDraft)
	case "plan-draft-field":
		if !isAdmin(act) || !isPrivateChat(c.Chat()) || st.PlanDraft == nil {
			return a.home(c, act, "پیش‌نویس طرح پیدا نشد. از منوی مدیریت دوباره شروع کنید.")
		}
		return a.savePlanDraftField(c, act, st, raw)
	default:
		a.clearFlow(act.TelegramID)
		return a.home(c, act, "ورودی منقضی شد. از منو دوباره شروع کنید.")
	}
}

func isPrivateChat(chat *telebot.Chat) bool {
	return chat != nil && chat.Type == telebot.ChatPrivate
}

func adminFlowStep(step string) bool {
	switch step {
	case "cfgvalue", "cfgtextkey", "planvalue", "paneltoken", "plan-draft-name", "plan-draft-field":
		return true
	default:
		return false
	}
}

// submitPanelToken removes the message containing the credential before the
// credential is sent to the backend. A failed delete stops the update.
func submitPanelToken(chat *telebot.Chat, deleteMessage func() error, submit func() error) error {
	if deleteMessage == nil || deleteMessage() != nil {
		return errPanelTokenDelete
	}
	if !isPrivateChat(chat) {
		return errPanelPrivateChat
	}
	return submit()
}
func (a *botApp) setFlowAndPrompt(c telebot.Context, id int64, st conversation, prompt string) error {
	st.Expires = time.Now().Add(20 * time.Minute)
	a.setFlow(id, st)
	return a.show(c, prompt, markup([]telebot.Btn{btn("لغو", "home")}))
}
func (a *botApp) createTopup(c telebot.Context, act actor, amount int64, key string) error {
	var instructions paymentInstructions
	if err := a.call(c, "GET", "/v1/payment-instructions", act.TelegramID, nil, &instructions); err != nil {
		return a.show(c, "اطلاعات پرداخت دریافت نشد؛ درخواست شارژ ثبت نشده است. پس از بررسی دوباره تلاش کنید.", markup([]telebot.Btn{btn("تلاش دوباره", "retry-topup")}, []telebot.Btn{btn("لغو", "home")}))
	}
	if !validPaymentDestination(instructions) {
		return a.show(c, "اطلاعات کارت پرداخت کامل نیست؛ درخواست شارژ ثبت نشده است. مدیر باید شماره کارت و نام صاحب کارت را تنظیم کند.", markup([]telebot.Btn{btn("تلاش دوباره", "retry-topup")}, []telebot.Btn{btn("لغو", "home")}))
	}
	var out struct {
		TopupID int64  `json:"topup_id"`
		Status  string `json:"status"`
	}
	if err := a.call(c, "POST", "/v1/wallet/topups", act.TelegramID, map[string]any{"amount_toman": amount, "idempotency_key": key}, &out); err != nil {
		return a.show(c, "ثبت درخواست شارژ تأیید نشد. برای بررسی نتیجه، همین درخواست را با همان کلید امن دوباره امتحان کنید.", markup([]telebot.Btn{btn("تلاش دوباره", "retry-topup")}, []telebot.Btn{btn("لغو", "home")}))
	}
	if out.TopupID <= 0 || out.Status != "awaiting_receipt" {
		return a.show(c, "نتیجه ثبت شارژ تأیید نشد. برای بررسی نتیجه، همین درخواست را با همان کلید امن دوباره امتحان کنید.", markup([]telebot.Btn{btn("تلاش دوباره", "retry-topup")}, []telebot.Btn{btn("لغو", "home")}))
	}
	a.clearFlow(act.TelegramID)
	text := fmt.Sprintf("درخواست شارژ شماره %d ثبت شد.\nمبلغ: %s تومان\n%s", out.TopupID, formatToman(amount), paymentInstructionDetails(instructions))
	return a.show(c, text, markup([]telebot.Btn{btn("📷 ارسال عکس رسید", fmt.Sprintf("receipt|topup|%d", out.TopupID))}, []telebot.Btn{btn("خانه", "home")}))
}
func (a *botApp) photo(c telebot.Context) error {
	if c.Sender() == nil {
		return nil
	}
	act, err := a.resolve(c)
	if err != nil {
		return a.sendFailure(c, err)
	}
	a.mu.Lock()
	st, ok := a.receipts[c.Sender().ID]
	a.mu.Unlock()
	if !ok {
		active, complete, activeErr := a.awaitingPhotoReceipts(c, act.TelegramID)
		if activeErr != nil {
			return a.sendFailure(c, activeErr)
		}
		candidates := receiptCandidatesForPhoto(active, complete)
		if !complete {
			return a.show(c, "درخواست‌های رسید زیاد هستند؛ برای جلوگیری از اتصال عکس به فاکتور اشتباه، ابتدا از فهرست ادامه پرداخت یا شارژ فاکتور را انتخاب کنید.", markup([]telebot.Btn{btn("ادامه پرداخت یا شارژ", "resume")}, []telebot.Btn{btn("خانه", "home")}))
		}
		switch len(candidates) {
		case 0:
			return a.show(c, "فاکتور منتظر رسید پیدا نشد. اگر فاکتور فعال دارید از گزینه ادامه پرداخت یا شارژ استفاده کنید.", markup([]telebot.Btn{btn("ادامه پرداخت یا شارژ", "resume")}, []telebot.Btn{btn("خانه", "home")}))
		case 1:
			st, ok = candidates[0], true
			a.mu.Lock()
			a.receipts[act.TelegramID] = st
			a.mu.Unlock()
		default:
			return a.show(c, "چند فاکتور منتظر رسید دارید. ابتدا از منوی ادامه پرداخت یا شارژ فاکتور مربوطه را انتخاب کنید، سپس عکس را دوباره ارسال کنید.", markup([]telebot.Btn{btn("ادامه پرداخت یا شارژ", "resume")}))
		}
	}
	msg := c.Message()
	if msg == nil || msg.Photo == nil {
		return a.show(c, "عکس رسید دریافت نشد.")
	}
	path := fmt.Sprintf("/v1/payment-intents/%d/receipt", st.ID)
	if st.Kind == "topup" {
		path = fmt.Sprintf("/v1/wallet/topups/%d/receipt", st.ID)
	}
	if err = a.call(c, "POST", path, act.TelegramID, map[string]any{"telegram_file_id": msg.Photo.FileID}, nil); err != nil {
		return a.sendFailure(c, err)
	}
	a.mu.Lock()
	delete(a.receipts, act.TelegramID)
	a.mu.Unlock()
	return a.home(c, act, "رسید ثبت شد و برای بررسی قرار گرفت.")
}
func (a *botApp) cancelSubscription(c telebot.Context, act actor, id int64) error {
	if err := a.call(c, "POST", fmt.Sprintf("/v1/subscriptions/%d/cancel", id), act.TelegramID, map[string]any{"idempotency_key": a.operationKey(c, fmt.Sprintf("cancel-%d", id))}, nil); err != nil {
		return a.sendFailure(c, err)
	}
	return a.show(c, "درخواست لغو ثبت شد و وضعیت پنل در حال تطبیق است.", markup([]telebot.Btn{btn("سرویس‌های من", "services"), btn("خانه", "home")}))
}
func (a *botApp) wallet(c telebot.Context, act actor) error {
	features := a.runtime(c, act.TelegramID)
	if !featureEnabled(features.Features, "wallet_enabled") {
		return a.home(c, act, "کیف پول در حال حاضر غیرفعال است.")
	}
	var out map[string]any
	if err := a.call(c, "GET", "/v1/wallet", act.TelegramID, nil, &out); err != nil {
		return a.sendFailure(c, err)
	}
	return a.show(c, fmt.Sprintf("موجودی کیف پول: %v تومان", out["balance_toman"]), markup(walletMenuRows(features)...))
}
func (a *botApp) ledger(c telebot.Context, act actor) error {
	var out []map[string]any
	if err := a.call(c, "GET", "/v1/wallet/ledger", act.TelegramID, nil, &out); err != nil {
		return a.sendFailure(c, err)
	}
	if len(out) == 0 {
		return a.show(c, "تراکنشی ثبت نشده است.", markup([]telebot.Btn{btn("خانه", "home")}))
	}
	var b strings.Builder
	for _, r := range out {
		fmt.Fprintf(&b, "%v تومان · %v · %v\n", r["amount_toman"], r["type"], r["description"])
	}
	return a.show(c, strings.TrimSpace(b.String()), markup([]telebot.Btn{btn("کیف پول", "wallet"), btn("خانه", "home")}))
}
func (a *botApp) services(c telebot.Context, act actor) error {
	var out []subscription
	if err := a.call(c, "GET", "/v1/subscriptions", act.TelegramID, nil, &out); err != nil {
		return a.sendFailure(c, err)
	}
	if len(out) == 0 {
		return a.show(c, "اشتراکی ثبت نشده است.", markup([]telebot.Btn{btn("خرید سرویس", "plans|paid"), btn("خانه", "home")}))
	}
	rows := make([][]telebot.Btn, 0, len(out)+1)
	for _, s := range out {
		label := fmt.Sprintf("%s · %s", short(s.DisplayName, 28), s.Status)
		rows = append(rows, []telebot.Btn{btn(label, fmt.Sprintf("service|%d", s.ID))})
	}
	rows = append(rows, []telebot.Btn{btn("خانه", "home")})
	return a.show(c, "سرویس‌هایتان را برای مشاهده لینک و جزئیات انتخاب کنید:", markup(rows...))
}

func (a *botApp) subscriptionDetails(c telebot.Context, act actor, id int64, page int) error {
	var subscriptions []subscription
	if err := a.call(c, "GET", "/v1/subscriptions", act.TelegramID, nil, &subscriptions); err != nil {
		return a.sendFailure(c, err)
	}
	for _, item := range subscriptions {
		if item.ID != id {
			continue
		}
		message, nextPage, previousPage := renderSubscriptionDetails(item, page)
		rows := [][]telebot.Btn{}
		if previousPage >= 0 {
			rows = append(rows, []telebot.Btn{btn("لینک‌های قبلی", fmt.Sprintf("servicepage|%d|%d", id, previousPage))})
		}
		if nextPage >= 0 {
			rows = append(rows, []telebot.Btn{btn("لینک‌های بعدی", fmt.Sprintf("servicepage|%d|%d", id, nextPage))})
		}
		if item.Status == "active" {
			rows = append(rows, []telebot.Btn{btn("درخواست لغو سرویس", fmt.Sprintf("cancel|%d", id))})
		}
		rows = append(rows, []telebot.Btn{btn("بازگشت به سرویس‌ها", "services"), btn("خانه", "home")})
		return a.show(c, message, markup(rows...))
	}
	return a.show(c, "این سرویس در حساب شما پیدا نشد.", markup([]telebot.Btn{btn("سرویس‌های من", "services"), btn("خانه", "home")}))
}

type subscriptionLinkPart struct {
	link, part, total int
	text              string
}

func subscriptionLinkParts(links []string) []subscriptionLinkPart {
	parts := []subscriptionLinkPart{}
	for linkIndex, link := range links {
		runes := []rune(link)
		count := (len(runes) + maxSubscriptionLinkChunk - 1) / maxSubscriptionLinkChunk
		if count == 0 {
			continue
		}
		for partIndex, offset := 0, 0; offset < len(runes); partIndex, offset = partIndex+1, offset+maxSubscriptionLinkChunk {
			end := offset + maxSubscriptionLinkChunk
			if end > len(runes) {
				end = len(runes)
			}
			parts = append(parts, subscriptionLinkPart{link: linkIndex + 1, part: partIndex + 1, total: count, text: string(runes[offset:end])})
		}
	}
	return parts
}

func (p subscriptionLinkPart) display() string {
	if p.total > 1 {
		return fmt.Sprintf("\n🔗 لینک %d · بخش %d/%d (بخش‌ها را به‌ترتیب بدون فاصله بچسبانید):\n%s", p.link, p.part, p.total, p.text)
	}
	return fmt.Sprintf("\n🔗 لینک %d:\n%s", p.link, p.text)
}

func subscriptionPageEnd(base string, parts []subscriptionLinkPart, start int) int {
	if start < 0 {
		start = 0
	}
	if start > len(parts) {
		return len(parts)
	}
	end := start
	text := base
	for end < len(parts) && end-start < maxSubscriptionLinksPerPage {
		line := parts[end].display()
		if utf8.RuneCountInString(text+line) > maxSubscriptionPageRunes {
			break
		}
		text += line
		end++
	}
	return end
}

func renderSubscriptionDetails(s subscription, start int) (string, int, int) {
	if start < 0 {
		start = 0
	}
	expiry := "بدون تاریخ انقضا"
	if s.ExpiryTimeMS > 0 {
		expiry = time.UnixMilli(s.ExpiryTimeMS).UTC().Format("2006-01-02 15:04 UTC")
	}
	traffic := "نامحدود"
	if s.TrafficLimitBytes > 0 {
		traffic = fmt.Sprintf("%.2f GB", float64(s.TrafficLimitBytes)/(1000*1000*1000))
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s\nوضعیت: %s · نوع: %s\nایمیل/شناسه: %s\nIP مجاز: %d · حجم: %s\nانقضا: %s", short(s.DisplayName, 80), short(s.Status, 40), short(s.Kind, 40), short(s.Email, 100), s.IPLimit, traffic, expiry)
	base := b.String()
	parts := subscriptionLinkParts(s.Links)
	if len(parts) == 0 {
		b.WriteString("\nلینک اشتراک هنوز در دسترس نیست.")
		return b.String(), -1, -1
	}
	if start > len(parts) {
		start = len(parts)
	}
	end := subscriptionPageEnd(base, parts, start)
	for index := start; index < end; index++ {
		b.WriteString(parts[index].display())
	}
	previous := -1
	if start > 0 {
		cursor := 0
		for cursor < start {
			next := subscriptionPageEnd(base, parts, cursor)
			if next <= cursor {
				break
			}
			if next >= start {
				previous = cursor
				break
			}
			cursor = next
		}
	}
	next := -1
	if end < len(parts) {
		next = end
	}
	return b.String(), next, previous
}

const maxSubscriptionLinksPerPage = 5
const maxSubscriptionLinkChunk = 2500
const maxSubscriptionPageRunes = 3600

func (a *botApp) adminHome(c telebot.Context, act actor) error {
	return a.show(c, "مدیریت فقط برای مدیر پیکربندی‌شده در این deployment در دسترس است.", markup([]telebot.Btn{btn("درخواست‌های reseller", "resellers")}, []telebot.Btn{btn("پرداخت‌های در انتظار", "pending"), btn("شارژهای در انتظار", "pendingtopups")}, []telebot.Btn{btn("کارهای عملیاتی", "work-items"), btn("استردادهای در انتظار", "refunds")}, []telebot.Btn{btn("تنظیمات ربات و طرح‌ها", "config")}, []telebot.Btn{btn("خانه", "home")}))
}
func (a *botApp) pendingResellers(c telebot.Context, act actor) error {
	return a.pendingResellersPage(c, act, 0)
}
func (a *botApp) pendingResellersPage(c telebot.Context, act actor, page int) error {
	var items []map[string]any
	if err := a.call(c, "GET", "/v1/admin/resellers/pending", act.TelegramID, nil, &items); err != nil {
		return a.sendFailure(c, err)
	}
	if len(items) == 0 {
		return a.show(c, "درخواست تأیید reseller در انتظار نیست.", markup([]telebot.Btn{btn("مدیریت", "admin")}))
	}
	start, end, ok := adminPageBounds(len(items), page)
	if !ok {
		return a.show(c, "این صفحه از فهرست درخواست‌ها وجود ندارد.", markup([]telebot.Btn{btn("بازگشت", "resellers")}))
	}
	rows := make([][]telebot.Btn, 0, end-start+2)
	for _, item := range items[start:end] {
		id := fmt.Sprint(item["telegram_id"])
		label := fmt.Sprintf("%s · %v", id, item["approval_status"])
		rows = append(rows, []telebot.Btn{btn(label, "resapprove|"+id), btn("رد", "resreject|"+id)})
	}
	if nav := adminPageNavigation(page, len(items), "resellerpage"); len(nav) > 0 {
		rows = append(rows, nav)
	}
	rows = append(rows, []telebot.Btn{btn("مدیریت", "admin")})
	return a.show(c, "درخواست‌های reseller در همین deployment:", markup(rows...))
}
func (a *botApp) reviewReseller(c telebot.Context, act actor, telegramID int64, action string) error {
	verb := "approve"
	if action == "resreject" {
		verb = "reject"
	}
	var out map[string]any
	path := fmt.Sprintf("/v1/admin/resellers/%d/%s", telegramID, verb)
	if err := a.call(c, "POST", path, act.TelegramID, map[string]any{}, &out); err != nil {
		return a.sendFailure(c, err)
	}
	return a.show(c, fmt.Sprintf("وضعیت reseller %d ثبت شد: %v", telegramID, out["approval_status"]), markup([]telebot.Btn{btn("بازگشت به درخواست‌ها", "resellers"), btn("مدیریت", "admin")}))
}
func (a *botApp) showPending(c telebot.Context, act actor, kind string, page int) error {
	path, label := "/v1/admin/payments", "پرداخت‌های منتظر"
	action := "review-payment"
	if kind == "pendingtopups" {
		path, label, action = "/v1/admin/topups", "شارژهای منتظر", "review-topup"
	}
	var items []map[string]any
	if err := a.call(c, "GET", path, act.TelegramID, nil, &items); err != nil {
		return a.sendFailure(c, err)
	}
	if len(items) == 0 {
		return a.show(c, "درخواست معوقی وجود ندارد.", markup([]telebot.Btn{btn("مدیریت", "admin")}))
	}
	start, end, ok := adminPageBounds(len(items), page)
	if !ok {
		return a.show(c, "این صفحه از فهرست درخواست‌ها وجود ندارد.", markup([]telebot.Btn{btn("بازگشت", kind)}))
	}
	rows := make([][]telebot.Btn, 0, end-start+2)
	for _, item := range items[start:end] {
		id := fmt.Sprint(item["id"])
		rows = append(rows, []telebot.Btn{btn(fmt.Sprintf("بررسی رسید #%s · %v تومان", id, item["amount_toman"]), action+"|"+id)})
	}
	if nav := adminPageNavigation(page, len(items), "pendingpage|"+kind); len(nav) > 0 {
		rows = append(rows, nav)
	}
	rows = append(rows, []telebot.Btn{btn("مدیریت", "admin")})
	return a.show(c, label, markup(rows...))
}
func (a *botApp) adminAction(c telebot.Context, act actor, path string) error {
	var out map[string]any
	if err := a.call(c, "POST", path, act.TelegramID, map[string]any{}, &out); err != nil {
		return a.sendFailure(c, err)
	}
	return a.show(c, "عملیات ثبت شد: "+fmt.Sprint(out["status"]), markup([]telebot.Btn{btn("مدیریت", "admin")}))
}

func (a *botApp) showConfig(c telebot.Context, act actor) error {
	var cfg adminConfig
	if err := a.call(c, "GET", "/v1/admin/config", act.TelegramID, nil, &cfg); err != nil {
		return a.sendFailure(c, err)
	}
	rows := [][]telebot.Btn{{btn("طرح‌ها و قیمت‌ها", "cfg|plans"), btn("پرداخت واریزی", "cfg|payment")}, {btn("قواعد تست و قابلیت‌ها", "cfg|settings"), btn("اتصال پنل", "cfg|panel")}, {btn("متن‌های ربات", "cfg|text")}, {btn("مدیریت", "admin")}}
	return a.show(c, fmt.Sprintf("پیکربندی این deployment (%s)\nپنل: %s · رمز تنظیم شده: %t", cfg.Channel, cfg.Panel.BaseURL, cfg.Panel.TokenConfigured), markup(rows...))
}
func (a *botApp) configSection(c telebot.Context, act actor, section string) error {
	switch section {
	case "plans":
		return a.configPlans(c, act)
	case "payment":
		var cfg adminConfig
		if err := a.call(c, "GET", "/v1/admin/config", act.TelegramID, nil, &cfg); err != nil {
			return a.sendFailure(c, err)
		}
		return a.show(c, fmt.Sprintf("شماره کارت: %s\nصاحب کارت: %s\nراهنما: %s", cfg.Payment.CardNumber, cfg.Payment.CardOwner, cfg.Payment.Instructions), markup([]telebot.Btn{btn("تغییر شماره کارت", "cfgset|payment|card_number"), btn("تغییر صاحب کارت", "cfgset|payment|card_owner")}, []telebot.Btn{btn("تغییر راهنمای پرداخت", "cfgset|payment|instructions")}, []telebot.Btn{btn("بازگشت", "config")}))
	case "settings":
		return a.configSettings(c, act)
	case "text":
		return a.configTexts(c, act)
	case "panel":
		var cfg adminConfig
		if err := a.call(c, "GET", "/v1/admin/config", act.TelegramID, nil, &cfg); err != nil {
			return a.sendFailure(c, err)
		}
		return a.show(c, fmt.Sprintf("نشانی پنل: %s\nتوکن ذخیره شده: %t\nبرای تغییر، ابتدا نشانی را وارد کنید و سپس توکن را وارد کنید.", cfg.Panel.BaseURL, cfg.Panel.TokenConfigured), markup([]telebot.Btn{btn("تغییر اطلاعات پنل", "cfgset|panel|base_url")}, []telebot.Btn{btn("بازگشت", "config")}))
	default:
		return a.show(c, "بخش نامعتبر است.", markup([]telebot.Btn{btn("مدیریت", "admin")}))
	}
}
func (a *botApp) configPlans(c telebot.Context, act actor) error {
	var cfg adminConfig
	if err := a.call(c, "GET", "/v1/admin/config", act.TelegramID, nil, &cfg); err != nil {
		return a.sendFailure(c, err)
	}
	rows := make([][]telebot.Btn, 0, len(cfg.Plans)+2)
	for _, p := range cfg.Plans {
		rows = append(rows, []telebot.Btn{btn(fmt.Sprintf("%s · %s · %t", p.Name, p.Kind, p.Enabled), fmt.Sprintf("planedit|%d", p.ID))})
	}
	rows = append(rows, []telebot.Btn{btn("ایجاد طرح", "cfgset|plan|new")}, []telebot.Btn{btn("بازگشت", "config")})
	return a.show(c, "طرح را برای ویرایش قیمت‌ها و مشخصات انتخاب کنید.", markup(rows...))
}
func (a *botApp) planEditor(c telebot.Context, act actor, idText string) error {
	return a.planEditorWithNotice(c, act, idText, "")
}

func (a *botApp) planEditorWithNotice(c telebot.Context, act actor, idText, notice string) error {
	id, e := strconv.ParseInt(idText, 10, 64)
	if e != nil {
		return a.show(c, "شناسه طرح نامعتبر است.")
	}
	var cfg adminConfig
	if err := a.call(c, "GET", "/v1/admin/config", act.TelegramID, nil, &cfg); err != nil {
		return a.sendFailure(c, err)
	}
	for _, p := range cfg.Plans {
		if p.ID == id {
			message := formatAdminPlan(p)
			if notice != "" {
				message = notice + "\n\n" + message
			}
			return a.show(c, message, planEditorMarkup(p))
		}
	}
	return a.show(c, "طرح پیدا نشد.", markup([]telebot.Btn{btn("بازگشت", "cfg|plans")}))
}
func (a *botApp) startPlanEdit(c telebot.Context, act actor, id, field string) error {
	var cfg adminConfig
	if err := a.call(c, "GET", "/v1/admin/config", act.TelegramID, nil, &cfg); err != nil {
		return a.sendFailure(c, err)
	}
	var selected *plan
	for i := range cfg.Plans {
		if strconv.FormatInt(cfg.Plans[i].ID, 10) == id {
			selected = &cfg.Plans[i]
			break
		}
	}
	if selected == nil {
		return a.show(c, "طرح پیدا نشد.", markup([]telebot.Btn{btn("بازگشت", "cfg|plans")}))
	}
	if field == "inbound_ids" {
		return a.startExistingPlanInboundEdit(c, act, *selected)
	}
	current := planSummaryText(planFieldValue(*selected, field), 1000)
	a.setFlow(act.TelegramID, conversation{Step: "planvalue", Vals: map[string]string{"id": id, "field": field}, Expires: time.Now().Add(20 * time.Minute)})
	return a.show(c, fmt.Sprintf("%s\nمقدار فعلی: %s", planFieldPrompt(field), current), markup([]telebot.Btn{btn("لغو", "cfg|plans")}))
}

func planFieldPrompt(field string) string {
	prompts := map[string]string{
		"name":                        "نام طرح را وارد کنید.",
		"kind":                        "نوع طرح را وارد کنید: paid یا test.",
		"enabled":                     "وضعیت فعال را وارد کنید: true یا false.",
		"is_limited":                  "نوع محدودیت را وارد کنید: true یا false.",
		"description":                 "توضیحات طرح را وارد کنید؛ برای پاک‌کردن - بفرستید.",
		"base_price_toman":            "قیمت پایه را به تومان و به‌صورت عدد صحیح نامنفی وارد کنید.",
		"price_per_extra_ip_toman":    "قیمت هر IP اضافه را به تومان وارد کنید.",
		"price_per_gb_toman":          "قیمت هر GB را به تومان وارد کنید.",
		"price_per_extra_month_toman": "قیمت هر ماه اضافه را به تومان وارد کنید.",
		"base_ip_limit":               "تعداد IP پایه را وارد کنید؛ صفر یعنی نامحدود.",
		"max_ip_limit":                "حداکثر تعداد IP را وارد کنید؛ صفر یعنی نامحدود.",
		"min_data_gb":                 "حداقل حجم را به GB وارد کنید.",
		"max_data_gb":                 "حداکثر حجم را به GB وارد کنید؛ اعشار مجاز است و صفر یعنی نامحدود.",
		"expire_seconds":              "مدت را به ثانیه وارد کنید.",
		"test_ip_limit":               "تعداد IP نمایشی تست را وارد کنید.",
		"max_per_day":                 "سهمیه روزانه را وارد کنید؛ صفر یعنی بدون سقف.",
		"flow":                        "Flow را وارد کنید؛ برای پاک‌کردن - بفرستید.",
		"discount_tiers":              "تخفیف‌ها را با قالب ماه:درصد وارد کنید؛ نمونه 3:10,6:20. برای پاک‌کردن - بفرستید.",
		"allowed_telegram_ids":        "شناسه‌های مجاز تلگرام را با کاما جدا کنید؛ - یعنی همه resellerهای این deployment.",
		"usage_description":           "راهنمای مصرف را وارد کنید؛ برای پاک‌کردن - بفرستید.",
	}
	if prompt := prompts[field]; prompt != "" {
		return prompt
	}
	return "مقدار جدید را وارد کنید."
}

func planFieldValue(p plan, field string) string {
	switch field {
	case "discount_tiers":
		return planDiscountLabel(p.DiscountTiers)
	case "allowed_telegram_ids":
		return planAccessLabel(p.AllowedTelegramIDs)
	case "max_data_gb":
		return formatPlanData(p.MaxBytes)
	case "expire_seconds":
		return humanDuration(p.ExpireSeconds)
	case "inbound_ids":
		return planInboundNames(p.InboundIDs)
	case "base_price_toman":
		return formatToman(p.BasePrice)
	case "price_per_gb_toman":
		return formatToman(p.PriceGB)
	case "price_per_extra_ip_toman":
		return formatToman(p.PriceExtraIP)
	case "price_per_extra_month_toman":
		return formatToman(p.PriceExtraMonth)
	case "min_data_gb":
		return strconv.Itoa(p.MinGB)
	case "base_ip_limit":
		return strconv.Itoa(p.BaseIP)
	case "max_ip_limit":
		return strconv.Itoa(p.MaxIP)
	case "test_ip_limit":
		return strconv.Itoa(p.TestIPLimit)
	case "max_per_day":
		return strconv.Itoa(p.MaxPerDay)
	case "enabled":
		return strconv.FormatBool(p.Enabled)
	case "is_limited":
		return strconv.FormatBool(p.IsLimited)
	case "is_global":
		return strconv.FormatBool(p.IsGlobal)
	default:
		b, err := json.Marshal(p)
		if err != nil {
			return ""
		}
		values := map[string]json.RawMessage{}
		if err := json.Unmarshal(b, &values); err != nil {
			return ""
		}
		var value string
		if err := json.Unmarshal(values[field], &value); err == nil {
			return value
		}
		return string(values[field])
	}
}
func (a *botApp) configSettings(c telebot.Context, act actor) error {
	var cfg adminConfig
	if err := a.call(c, "GET", "/v1/admin/config", act.TelegramID, nil, &cfg); err != nil {
		return a.sendFailure(c, err)
	}
	rows := [][]telebot.Btn{{btn(fmt.Sprintf("سهمیه روزانه نامؤید: %d", cfg.Settings.UnapprovedTrialDailyLimit), "cfgset|settings|unapproved_trial_daily_limit")}, {btn(resellerApprovalButtonLabel(cfg.Settings.ResellerApprovedRequired), "cfgset|settings|reseller_approved_required")}, {btn(fmt.Sprintf("حداقل شارژ: %s تومان", formatToman(cfg.Settings.MinTopupToman)), "cfgset|settings|min_topup_toman")}}
	keys := []string{"purchases_enabled", "trials_enabled", "wallet_enabled", "topups_enabled", "direct_payments_enabled"}
	for _, key := range keys {
		rows = append(rows, []telebot.Btn{btn(fmt.Sprintf("%s: %t (تغییر)", key, featureEnabled(cfg.Settings.Features, key)), "feature|"+key)})
	}
	rows = append(rows, []telebot.Btn{btn("بازگشت", "config")})
	return a.show(c, fmt.Sprintf("سهمیه تست روزانه: %d. حداقل شارژ: %s تومان. این محدودیت‌ها و قابلیت‌ها در backend هم اعمال می‌شوند.", cfg.Settings.UnapprovedTrialDailyLimit, formatToman(cfg.Settings.MinTopupToman)), markup(rows...))
}
func resellerApprovalButtonLabel(required bool) string {
	if required {
		return "فقط reseller تأییدشده: بله"
	}
	return "فقط reseller تأییدشده: خیر"
}
func (a *botApp) configTexts(c telebot.Context, act actor) error {
	var cfg adminConfig
	if err := a.call(c, "GET", "/v1/admin/config", act.TelegramID, nil, &cfg); err != nil {
		return a.sendFailure(c, err)
	}
	rows := make([][]telebot.Btn, 0, len(cfg.Settings.Text)+1)
	for key, value := range cfg.Settings.Text {
		rows = append(rows, []telebot.Btn{btn(key+": "+short(value, 24), "cfgset|text|"+key)})
	}
	rows = append(rows, []telebot.Btn{btn("افزودن متن", "cfgset|text|new")})
	rows = append(rows, []telebot.Btn{btn("بازگشت", "config")})
	return a.show(c, "متن مورد نظر را انتخاب کنید.", markup(rows...))
}
func (a *botApp) startConfigEdit(c telebot.Context, act actor, section, field string) error {
	if section == "text" && field == "new" {
		a.setFlow(act.TelegramID, conversation{Step: "cfgtextkey", Vals: map[string]string{"section": "text"}, Expires: time.Now().Add(20 * time.Minute)})
		return a.show(c, "یک کلید کوتاه انگلیسی وارد کنید؛ نمونه: home_title", markup([]telebot.Btn{btn("لغو", "config")}))
	}
	prompt := "مقدار جدید را وارد کنید."
	if section == "payment" {
		prompt = "مقدار جدید را وارد کنید."
	}
	if section == "panel" && field == "base_url" {
		prompt = "نشانی پایه پنل را وارد کنید؛ سپس ربات از شما توکن را می‌خواهد."
	}
	if section == "plan" {
		return a.startPlanDraft(c, act)
	}
	a.setFlow(act.TelegramID, conversation{Step: "cfgvalue", Vals: map[string]string{"section": section, "field": field}, Expires: time.Now().Add(20 * time.Minute)})
	return a.show(c, prompt, markup([]telebot.Btn{btn("لغو", "config")}))
}

func (a *botApp) saveConfigValue(c telebot.Context, act actor, st conversation, value string) error {
	section, field := st.Vals["section"], st.Vals["field"]
	payload := map[string]any{}
	path := ""
	switch section {
	case "payment":
		return a.patchPaymentInstruction(c, act, field, value)
	case "settings":
		path = "/v1/admin/config/settings"
		if field == "unapproved_trial_daily_limit" {
			v, e := strconv.Atoi(value)
			if e != nil || v < 0 {
				return a.show(c, "عدد نامعتبر است.")
			}
			payload[field] = v
		} else if field == "reseller_approved_required" {
			v, e := strconv.ParseBool(value)
			if e != nil {
				return a.show(c, "فقط true یا false وارد کنید.")
			}
			payload[field] = v
		} else if field == "min_topup_toman" {
			v, e := strconv.ParseInt(value, 10, 64)
			if e != nil || v < 0 {
				return a.show(c, "حداقل شارژ باید عدد صحیح نامنفی و به تومان باشد.")
			}
			payload[field] = v
		}
	case "text":
		path = "/v1/admin/config/settings"
		var cfg adminConfig
		if err := a.call(c, "GET", "/v1/admin/config", act.TelegramID, nil, &cfg); err != nil {
			return a.sendFailure(c, err)
		}
		if cfg.Settings.Text == nil {
			cfg.Settings.Text = map[string]string{}
		}
		cfg.Settings.Text[field] = value
		payload["text"] = cfg.Settings.Text
	case "panel":
		if field == "base_url" {
			st.Vals["base_url"] = value
			st.Step = "paneltoken"
			a.setFlow(act.TelegramID, st)
			return a.show(c, "توکن پنل را وارد کنید. این مقدار فقط به backend ارسال و ذخیره می‌شود.", markup([]telebot.Btn{btn("لغو", "config")}))
		}
		return a.show(c, "بخش نامعتبر است.")
	default:
		return a.show(c, "بخش نامعتبر است.")
	}
	if err := a.call(c, "PATCH", path, act.TelegramID, payload, nil); err != nil {
		return a.sendFailure(c, err)
	}
	a.clearFlow(act.TelegramID)
	return a.show(c, "تنظیم ذخیره شد.", markup([]telebot.Btn{btn("بازگشت", "config")}))
}

func (a *botApp) patchPaymentInstruction(c telebot.Context, act actor, field, value string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := a.updatePaymentInstruction(ctx, act.TelegramID, field, value); err != nil {
		return a.sendFailure(c, err)
	}
	a.clearFlow(act.TelegramID)
	return a.show(c, "تنظیم پرداخت ذخیره شد و سایر مقادیر حفظ شدند.", markup([]telebot.Btn{btn("بازگشت", "config")}))
}

func (a *botApp) updatePaymentInstruction(ctx context.Context, actorID int64, field, value string) error {
	var cfg adminConfig
	if err := a.api.Call(ctx, "GET", "/v1/admin/config", actorID, nil, &cfg); err != nil {
		return err
	}
	payload, err := buildPaymentInstructionPatch(cfg.Payment, field, value)
	if err != nil {
		return err
	}
	return a.api.Call(ctx, "PATCH", "/v1/admin/config/payment-instructions", actorID, payload, nil)
}

func buildPaymentInstructionPatch(current paymentInstructions, field, value string) (map[string]string, error) {
	switch field {
	case "card_number":
		current.CardNumber = value
	case "card_owner":
		current.CardOwner = value
	case "instructions":
		current.Instructions = value
	default:
		return nil, fmt.Errorf("unsupported payment field")
	}
	return map[string]string{"card_number": current.CardNumber, "card_owner": current.CardOwner, "instructions": current.Instructions}, nil
}

func (a *botApp) toggleFeature(c telebot.Context, act actor, key string) error {
	allowed := map[string]bool{"purchases_enabled": true, "trials_enabled": true, "wallet_enabled": true, "topups_enabled": true, "direct_payments_enabled": true}
	if !allowed[key] {
		return a.home(c, act, "قابلیت نامعتبر است.")
	}
	var cfg adminConfig
	if err := a.call(c, "GET", "/v1/admin/config", act.TelegramID, nil, &cfg); err != nil {
		return a.sendFailure(c, err)
	}
	if cfg.Settings.Features == nil {
		cfg.Settings.Features = map[string]bool{}
	}
	cfg.Settings.Features[key] = !featureEnabled(cfg.Settings.Features, key)
	if err := a.call(c, "PATCH", "/v1/admin/config/settings", act.TelegramID, map[string]any{"features": cfg.Settings.Features}, nil); err != nil {
		return a.sendFailure(c, err)
	}
	return a.configSettings(c, act)
}

func featureEnabled(features map[string]bool, key string) bool {
	value, exists := features[key]
	return !exists || value
}
func validConfigKey(key string) bool {
	if key == "" || len(key) > 30 {
		return false
	}
	for _, r := range key {
		if !(r >= 'a' && r <= 'z') && !(r >= '0' && r <= '9') && r != '_' {
			return false
		}
	}
	return true
}
func (a *botApp) savePlanValue(c telebot.Context, act actor, st conversation, value string) error {
	id, e := strconv.ParseInt(st.Vals["id"], 10, 64)
	if e != nil {
		return a.show(c, "شناسه طرح نامعتبر است.")
	}
	var cfg adminConfig
	if err := a.call(c, "GET", "/v1/admin/config", act.TelegramID, nil, &cfg); err != nil {
		return a.sendFailure(c, err)
	}
	var selected *plan
	for i := range cfg.Plans {
		if cfg.Plans[i].ID == id {
			selected = &cfg.Plans[i]
			break
		}
	}
	if selected == nil {
		return a.show(c, "طرح پیدا نشد.")
	}
	field := st.Vals["field"]
	if err := setPlanField(selected, field, value); err != nil {
		return a.show(c, "مقدار نامعتبر: "+err.Error())
	}
	if field == "enabled" && selected.Enabled {
		inbounds, err := a.planInbounds(c, act.TelegramID)
		if err != nil {
			return a.sendFailure(c, err)
		}
		if err := validatePlanInboundSelection(*selected, inbounds); err != nil {
			return a.show(c, "طرح تا زمانی که inboundهای فعال انتخاب نشوند فعال نمی‌شود: "+err.Error(), markup([]telebot.Btn{btn("انتخاب inboundها", fmt.Sprintf("planfield|%d|inbound_ids", selected.ID))}, []telebot.Btn{btn("بازگشت", fmt.Sprintf("planedit|%d", selected.ID))}))
		}
	}
	payload, _ := json.Marshal(selected)
	if err := a.call(c, "PUT", fmt.Sprintf("/v1/admin/config/plans/%d", id), act.TelegramID, json.RawMessage(payload), nil); err != nil {
		return a.sendFailure(c, err)
	}
	a.clearFlow(act.TelegramID)
	return a.planEditorWithNotice(c, act, st.Vals["id"], "✅ تغییر طرح ذخیره شد.")
}

func setPlanField(p *plan, field, raw string) error {
	parseInt := func(bits int) (int64, error) {
		v, err := strconv.ParseInt(strings.TrimSpace(raw), 10, bits)
		if err == nil && v < 0 {
			err = fmt.Errorf("باید غیرمنفی باشد")
		}
		return v, err
	}
	parseBool := func() (bool, error) { return strconv.ParseBool(strings.TrimSpace(raw)) }
	switch field {
	case "name":
		value := strings.TrimSpace(raw)
		if value == "" || len([]byte(value)) > 120 {
			return fmt.Errorf("نام باید بین ۱ تا ۱۲۰ بایت باشد")
		}
		p.Name = value
	case "kind":
		if raw != "paid" && raw != "test" {
			return fmt.Errorf("نوع باید paid یا test باشد")
		}
		p.Kind = raw
	case "enabled":
		v, e := parseBool()
		if e != nil {
			return e
		}
		p.Enabled = v
	case "is_limited":
		v, e := parseBool()
		if e != nil {
			return e
		}
		p.IsLimited = v
	case "description":
		if raw == "-" {
			raw = ""
		}
		if len([]byte(raw)) > 2048 {
			return fmt.Errorf("توضیحات حداکثر ۲۰۴۸ بایت باشد")
		}
		p.Description = raw
	case "base_price_toman":
		v, e := parseInt(64)
		if e != nil {
			return e
		}
		p.BasePrice = v
	case "price_per_extra_ip_toman":
		v, e := parseInt(64)
		if e != nil {
			return e
		}
		p.PriceExtraIP = v
	case "price_per_gb_toman":
		v, e := parseInt(64)
		if e != nil {
			return e
		}
		p.PriceGB = v
	case "price_per_extra_month_toman":
		v, e := parseInt(64)
		if e != nil {
			return e
		}
		p.PriceExtraMonth = v
	case "base_ip_limit":
		v, e := parseInt(32)
		if e != nil || v > 10000 {
			if e == nil {
				e = fmt.Errorf("حداکثر تعداد IP برابر ۱۰۰۰۰ است")
			}
			return e
		}
		p.BaseIP = int(v)
	case "max_ip_limit":
		v, e := parseInt(32)
		if e != nil || v > 10000 {
			if e == nil {
				e = fmt.Errorf("حداکثر تعداد IP برابر ۱۰۰۰۰ است")
			}
			return e
		}
		p.MaxIP = int(v)
	case "min_data_gb":
		v, e := parseInt(32)
		if e != nil || v > 100000 {
			if e == nil {
				e = fmt.Errorf("حداقل حجم حداکثر ۱۰۰۰۰۰ GB است")
			}
			return e
		}
		p.MinGB = int(v)
	case "max_data_bytes":
		v, e := parseInt(64)
		if e != nil {
			return e
		}
		p.MaxBytes = v
	case "max_data_gb":
		gb, e := strconv.ParseFloat(strings.TrimSpace(raw), 64)
		bytes := gb * float64(uint64(1)<<30)
		if e != nil || math.IsNaN(gb) || math.IsInf(gb, 0) || gb < 0 || bytes >= float64(math.MaxInt64) {
			return fmt.Errorf("حجم باید صفر یا عددی مثبت و محدود باشد")
		}
		p.MaxBytes = int64(bytes)
	case "expire_seconds":
		v, e := parseInt(64)
		if e != nil || v > maxResellerPlanLifetimeSeconds {
			if e == nil {
				e = fmt.Errorf("مدت حداکثر ده سال است")
			}
			return e
		}
		p.ExpireSeconds = v
	case "test_ip_limit":
		v, e := parseInt(32)
		if e != nil || v > 10000 {
			if e == nil {
				e = fmt.Errorf("حداکثر IP تست برابر ۱۰۰۰۰ است")
			}
			return e
		}
		p.TestIPLimit = int(v)
	case "max_per_day":
		v, e := parseInt(32)
		if e != nil || v > 10000 {
			if e == nil {
				e = fmt.Errorf("سهمیه روزانه حداکثر ۱۰۰۰۰ است")
			}
			return e
		}
		p.MaxPerDay = int(v)
	case "flow":
		if raw == "-" {
			raw = ""
		}
		if len([]byte(raw)) > 120 {
			return fmt.Errorf("Flow حداکثر ۱۲۰ بایت باشد")
		}
		p.Flow = raw
	case "inbound_ids":
		p.InboundIDs = []int{}
		seen := make(map[int]struct{})
		for _, part := range strings.Split(raw, ",") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			v, e := strconv.Atoi(part)
			if e != nil || v <= 0 {
				return fmt.Errorf("شناسه inbound نامعتبر است")
			}
			if _, exists := seen[v]; exists {
				return fmt.Errorf("شناسه inbound تکراری است")
			}
			seen[v] = struct{}{}
			p.InboundIDs = append(p.InboundIDs, v)
		}
	case "discount_tiers":
		tiers, e := parsePlanDiscounts(raw)
		if e != nil {
			return e
		}
		p.DiscountTiers = tiers
	case "allowed_telegram_ids":
		ids, e := parsePlanAccess(raw)
		if e != nil {
			return e
		}
		p.AllowedTelegramIDs = ids
		p.IsGlobal = len(ids) == 0
	case "usage_description":
		if raw == "-" {
			raw = ""
		}
		if len([]byte(raw)) > 4096 {
			return fmt.Errorf("راهنما حداکثر ۴۰۹۶ بایت باشد")
		}
		p.UsageDescription = raw
	default:
		return fmt.Errorf("فیلد پشتیبانی نمی‌شود")
	}
	return nil
}

func (a *botApp) call(c telebot.Context, method, path string, actorID int64, in, out any) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return a.api.Call(ctx, method, path, actorID, in, out)
}
func (a *botApp) operationKey(c telebot.Context, op string) string {
	chatID := int64(0)
	if c.Chat() != nil {
		chatID = c.Chat().ID
	}
	return attemptKey(c.Sender().ID, chatID, int64(c.Update().ID), op)
}
func (a *botApp) setFlow(id int64, st conversation) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.flows[id] = st
}
func (a *botApp) clearFlow(id int64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.flows, id)
	delete(a.receipts, id)
}
func stableKey(senderID, chatID, eventID int64, operation string) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%d:%d:%d:%s", senderID, chatID, eventID, operation)))
	return hex.EncodeToString(sum[:])
}
func attemptKey(senderID, chatID, updateID int64, operation string) string {
	return stableKey(senderID, chatID, updateID, operation)
}
func short(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}
func (a *botApp) sendFailure(c telebot.Context, err error) error {
	status, category := resellerFailureDiagnostic(err)
	if status > 0 {
		log.Printf("reseller action failed: category=%s status=%d", category, status)
	} else {
		log.Printf("reseller action failed: category=%s", category)
	}
	return a.show(c, "درخواست انجام نشد. "+resellerFailureHint(err), markup([]telebot.Btn{btn("بازگشت به خانه", "home")}))
}

func resellerFailureDiagnostic(err error) (int, string) {
	var apiErr *backend.APIError
	if !errors.As(err, &apiErr) {
		return 0, "transport_or_internal"
	}
	code := apiErr.Code
	if code == "" || len(code) > 48 {
		return apiErr.Status, "backend_error"
	}
	for _, r := range code {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-') {
			return apiErr.Status, "backend_error"
		}
	}
	return apiErr.Status, code
}

func resellerFailureHint(err error) string {
	status, category := resellerFailureDiagnostic(err)
	if status == 400 {
		var apiErr *backend.APIError
		if errors.As(err, &apiErr) && apiErr.Code == "invalid_request" {
			message := strings.Join(strings.Fields(apiErr.Message), " ")
			if message != "" && len([]rune(message)) <= 300 {
				return "داده‌های ارسالی پذیرفته نشدند (HTTP 400): " + message
			}
		}
		return "داده‌های ارسالی پذیرفته نشدند (HTTP 400). فیلدها را بررسی کنید."
	}
	if status > 0 {
		return fmt.Sprintf("خطای backend (HTTP %d، %s).", status, category)
	}
	return "ارتباط با backend کامل نشد."
}
