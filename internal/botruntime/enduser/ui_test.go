package enduser

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"example.com/xui-commerce/backend/internal/botruntime/backend"
	"gopkg.in/telebot.v3"
)

func fakeTelegramServer(t *testing.T, capture func(string, map[string]any)) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		method := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		if capture != nil {
			capture(method, body)
		}
		if method == "sendPhoto" {
			_, _ = fmt.Fprint(w, `{"ok":true,"result":{"message_id":99,"date":1,"chat":{"id":42,"type":"private"},"photo":[{"file_id":"returned-file","file_unique_id":"returned-unique","width":1,"height":1}],"caption":"ok"}}`)
			return
		}
		if method == "getMe" {
			_, _ = fmt.Fprint(w, `{"ok":true,"result":{"id":1,"is_bot":true,"first_name":"test","username":"test"}}`)
			return
		}
		_, _ = fmt.Fprint(w, `{"ok":true,"result":{"message_id":99,"date":1,"chat":{"id":42,"type":"private"},"text":"ok"}}`)
	}))
}

func requestMarkup(t *testing.T, body map[string]any) map[string]any {
	t.Helper()
	switch value := body["reply_markup"].(type) {
	case string:
		var markup map[string]any
		if err := json.Unmarshal([]byte(value), &markup); err != nil {
			t.Fatalf("decode Telegram reply markup: %v", err)
		}
		return markup
	case map[string]any:
		return value
	default:
		return nil
	}
}

func testBackend(t *testing.T, handler http.HandlerFunc) (*backend.Client, *httptest.Server) {
	t.Helper()
	s := httptest.NewServer(handler)
	api, err := backend.New(s.URL, "test-token", time.Second)
	if err != nil {
		s.Close()
		t.Fatal(err)
	}
	api.HTTP = s.Client()
	return api, s
}

func TestRetailHomeMatchesLegacyMenuSnapshot(t *testing.T) {
	api, backendServer := testBackend(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/actors/resolve":
			_, _ = w.Write([]byte(`{"telegram_id":42,"role":"customer","approval_status":"approved"}`))
		case "/v1/features":
			_, _ = w.Write([]byte(`{"features":{},"text":{}}`))
		default:
			t.Errorf("unexpected backend request: %s %s", r.Method, r.URL.String())
			http.NotFound(w, r)
		}
	})
	defer backendServer.Close()
	app := &botApp{api: api, states: make(map[int64]conversation)}
	bot, err := telebot.NewBot(telebot.Settings{Offline: true, Synchronous: true})
	if err != nil {
		t.Fatal(err)
	}
	ctx := bot.NewContext(telebot.Update{Message: &telebot.Message{
		Sender: &telebot.User{ID: 42}, Chat: &telebot.Chat{ID: 42, Type: telebot.ChatPrivate}, Text: "/start",
	}})
	text, markup, err := app.homeView(ctx, "👋 به پنل کاربری خوش آمدید\nسرویس وی‌پی‌ان خود را مدیریت کنید یا سرویس جدید خریداری نمایید.")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(text, "👋 به پنل کاربری خوش آمدید") {
		t.Fatalf("unexpected legacy welcome text: %q", text)
	}
	want := [][]string{
		{"🧪 تست رایگان", "💼 خرید سرویس"},
		{"📋 سرویس‌های من", "👛 کیف پول"},
		{"🆘 پشتیبانی"},
	}
	if len(markup.InlineKeyboard) != len(want) {
		t.Fatalf("home rows = %d, want %d", len(markup.InlineKeyboard), len(want))
	}
	for row, labels := range want {
		if len(markup.InlineKeyboard[row]) != len(labels) {
			t.Fatalf("row %d button count = %d, want %d", row, len(markup.InlineKeyboard[row]), len(labels))
		}
		for col, label := range labels {
			if got := markup.InlineKeyboard[row][col].Text; got != label {
				t.Fatalf("button [%d,%d] = %q, want %q", row, col, got, label)
			}
		}
	}
	for _, row := range markup.InlineKeyboard {
		for _, button := range row {
			if strings.Contains(strings.ToLower(button.Text), "admin") || strings.Contains(button.Text, "مدیریت") {
				t.Fatalf("admin control leaked onto customer home: %q", button.Text)
			}
		}
	}
}

func TestTelebotDispatchesLegacyPurchaseMenuCallback(t *testing.T) {
	var planRequest bool
	api, backendServer := testBackend(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/actors/resolve":
			_, _ = w.Write([]byte(`{"telegram_id":42,"role":"customer","approval_status":"approved"}`))
		case "/v1/features":
			_, _ = w.Write([]byte(`{"features":{},"text":{}}`))
		case "/v1/plans":
			planRequest = r.URL.Query().Get("kind") == "paid"
			_, _ = w.Write([]byte(`[{"id":7,"name":"ماهیانه","kind":"paid","is_limited":false,"base_price_toman":120000,"base_ip_limit":1,"max_ip_limit":3}]`))
		default:
			t.Errorf("unexpected backend request: %s %s", r.Method, r.URL.String())
			http.NotFound(w, r)
		}
	})
	defer backendServer.Close()

	var editSeen bool
	telegramServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/editMessageText") {
			editSeen = true
		}
		_, _ = fmt.Fprint(w, `{"ok":true,"result":{"message_id":99,"date":1,"chat":{"id":42,"type":"private"},"text":"ok"}}`)
	}))
	defer telegramServer.Close()
	bot, err := telebot.NewBot(telebot.Settings{Token: "test-token", URL: telegramServer.URL, Synchronous: true})
	if err != nil {
		t.Fatal(err)
	}
	app := &botApp{api: api, states: make(map[int64]conversation)}
	app.register(bot)
	app.setState(42, conversation{})
	nonce := app.state(42).Nonce
	bot.ProcessUpdate(telebot.Update{Callback: &telebot.Callback{
		ID: "callback-1", Sender: &telebot.User{ID: 42}, Data: "\fnav|" + nonce + "|plans-paid",
		Message: &telebot.Message{ID: 99, Sender: &telebot.User{ID: 0}, Chat: &telebot.Chat{ID: 42, Type: telebot.ChatPrivate}},
	}})
	if !planRequest {
		t.Fatal("real Telebot callback dispatch did not reach the paid plans backend route")
	}
	if !editSeen {
		t.Fatal("paid plans callback did not edit the current inline-menu message")
	}
	if _, ok := app.consumeCallbackState(42, nonce); ok {
		t.Fatal("callback nonce was not consumed by the real registered handler")
	}
}

func TestRetailHomeHidesBackendDisabledLegacyActions(t *testing.T) {
	api, backendServer := testBackend(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/actors/resolve" {
			_, _ = w.Write([]byte(`{"telegram_id":42,"role":"customer"}`))
			return
		}
		_, _ = w.Write([]byte(`{"features":{"purchases_enabled":false,"trials_enabled":false,"wallet_enabled":false},"text":{}}`))
	})
	defer backendServer.Close()
	app := &botApp{api: api, states: make(map[int64]conversation)}
	bot, err := telebot.NewBot(telebot.Settings{Offline: true, Synchronous: true})
	if err != nil {
		t.Fatal(err)
	}
	ctx := bot.NewContext(telebot.Update{Message: &telebot.Message{Sender: &telebot.User{ID: 42}, Chat: &telebot.Chat{ID: 42, Type: telebot.ChatPrivate}}})
	_, markup, err := app.homeView(ctx, "welcome")
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(markup.InlineKeyboard)
	if strings.Contains(string(data), "plans-paid") || strings.Contains(string(data), "plans-test") || strings.Contains(string(data), "wallet") {
		t.Fatalf("disabled retail features appeared on home: %s", data)
	}
}

func TestPurchaseRequiresVisibleQuoteAndExplicitConfirmation(t *testing.T) {
	var mu sync.Mutex
	quoteCalls, purchaseCalls := 0, 0
	var purchaseBody map[string]any
	api, backendServer := testBackend(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/actors/resolve":
			_, _ = w.Write([]byte(`{"telegram_id":42,"role":"customer"}`))
		case "/v1/features":
			_, _ = w.Write([]byte(`{"features":{},"text":{}}`))
		case "/v1/quotes":
			mu.Lock()
			quoteCalls++
			mu.Unlock()
			_, _ = w.Write([]byte(`{"id":77,"plan_name":"ماهیانه","months":3,"duration_days":90,"ip_limit":2,"data_gb":20,"base_price_toman":90000,"extra_ip_price_toman":10000,"extra_month_price_toman":0,"traffic_price_toman":20000,"discount_toman":5000,"final_price_toman":115000,"currency":"تومان"}`))
		case "/v1/wallet":
			_, _ = w.Write([]byte(`{"balance_toman":150000,"currency":"تومان"}`))
		case "/v1/purchases":
			mu.Lock()
			purchaseCalls++
			_ = json.NewDecoder(r.Body).Decode(&purchaseBody)
			mu.Unlock()
			_, _ = w.Write([]byte(`{"order_id":11,"status":"provisioning","amount_toman":115000}`))
		default:
			t.Errorf("unexpected backend request: %s %s", r.Method, r.URL.String())
			http.NotFound(w, r)
		}
	})
	defer backendServer.Close()
	var sentInvoice map[string]any
	telegramServer := fakeTelegramServer(t, func(method string, body map[string]any) {
		if method == "sendMessage" {
			sentInvoice = body
		}
	})
	defer telegramServer.Close()
	bot, err := telebot.NewBot(telebot.Settings{Token: "test-token", URL: telegramServer.URL, Synchronous: true})
	if err != nil {
		t.Fatal(err)
	}
	app := &botApp{api: api, states: make(map[int64]conversation)}
	app.register(bot)
	app.setState(42, conversation{PlanID: 7, Months: 3, IPLimit: 2, DataGB: 20, Name: "customer", Method: "wallet", OperationKey: "purchase-test-key"})
	ctx := bot.NewContext(telebot.Update{Message: &telebot.Message{ID: 7, Sender: &telebot.User{ID: 42}, Chat: &telebot.Chat{ID: 42, Type: telebot.ChatPrivate}}})
	if err = app.showInvoice(ctx, app.state(42), false); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	gotQuotes, gotPurchases := quoteCalls, purchaseCalls
	mu.Unlock()
	if gotQuotes != 1 || gotPurchases != 0 {
		t.Fatalf("before explicit confirmation, quotes/purchases = %d/%d, want 1/0", gotQuotes, gotPurchases)
	}
	text, _ := sentInvoice["text"].(string)
	for _, want := range []string{"طرح:", "customer", "3 ماه", "90 روز", "IP هم‌زمان: 2", "20 گیگابایت", "115,000 تومان", "تخفیف: 5,000 تومان", "موجودی کیف پول: 150,000 تومان"} {
		if !strings.Contains(text, want) {
			t.Fatalf("invoice missing %q: %s", want, text)
		}
	}
	markup := requestMarkup(t, sentInvoice)
	if markup == nil {
		t.Fatal("invoice has no payment buttons")
	}
	rows, _ := markup["inline_keyboard"].([]any)
	if len(rows) == 0 {
		t.Fatal("invoice has no inline keyboard rows")
	}
	var walletCallback string
	for _, rawRow := range rows {
		row, _ := rawRow.([]any)
		for _, rawButton := range row {
			button, _ := rawButton.(map[string]any)
			if button["text"] == "👛 پرداخت از کیف پول" {
				walletCallback, _ = button["callback_data"].(string)
			}
		}
	}
	if walletCallback == "" {
		t.Fatal("invoice lacks the wallet confirmation button")
	}
	bot.ProcessUpdate(telebot.Update{Callback: &telebot.Callback{ID: "confirm", Sender: &telebot.User{ID: 42}, Data: walletCallback, Message: &telebot.Message{ID: 99, Sender: &telebot.User{}, Chat: &telebot.Chat{ID: 42, Type: telebot.ChatPrivate}}}})
	mu.Lock()
	defer mu.Unlock()
	if purchaseCalls != 1 {
		t.Fatalf("confirmed invoice caused %d purchases, want one", purchaseCalls)
	}
	if purchaseBody["quote_id"] != float64(77) || purchaseBody["payment_method"] != "wallet" {
		t.Fatalf("purchase did not consume displayed immutable quote: %#v", purchaseBody)
	}
}

func TestTopupShowsBackendPaymentInstructionsAndMinimum(t *testing.T) {
	var created int
	api, backendServer := testBackend(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/actors/resolve":
			_, _ = w.Write([]byte(`{"telegram_id":42,"role":"customer"}`))
		case "/v1/payment-instructions":
			_, _ = w.Write([]byte(`{"card_number":"6037-0000","card_owner":"Test Owner","instructions":"واریز دقیق مبلغ","min_topup_toman":50000}`))
		case "/v1/wallet/topups":
			created++
			_, _ = w.Write([]byte(`{"topup_id":12,"amount_toman":75000,"status":"awaiting_receipt"}`))
		default:
			t.Errorf("unexpected backend request: %s %s", r.Method, r.URL.String())
			http.NotFound(w, r)
		}
	})
	defer backendServer.Close()
	var sentText string
	telegramServer := fakeTelegramServer(t, func(method string, body map[string]any) {
		if method == "sendMessage" {
			sentText, _ = body["text"].(string)
		}
	})
	defer telegramServer.Close()
	bot, err := telebot.NewBot(telebot.Settings{Token: "test-token", URL: telegramServer.URL, Synchronous: true})
	if err != nil {
		t.Fatal(err)
	}
	app := &botApp{api: api, states: make(map[int64]conversation)}
	app.setState(42, conversation{OperationKey: "topup-test-key"})
	ctx := bot.NewContext(telebot.Update{Message: &telebot.Message{ID: 8, Sender: &telebot.User{ID: 42}, Chat: &telebot.Chat{ID: 42, Type: telebot.ChatPrivate}}})
	if err = app.createTopup(ctx, 75000); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"6037-0000", "Test Owner", "واریز دقیق مبلغ", "حداقل شارژ: 50,000 تومان", "75,000 تومان", "رسید"} {
		if !strings.Contains(sentText, want) {
			t.Fatalf("top-up screen missing %q: %s", want, sentText)
		}
	}
	if created != 1 {
		t.Fatalf("created top-up count=%d, want 1", created)
	}
	sentText = ""
	if err = app.createTopup(ctx, 25000); err != nil {
		t.Fatal(err)
	}
	if created != 1 {
		t.Fatalf("below-minimum amount created a top-up: %d", created)
	}
	if !strings.Contains(sentText, "حداقل مبلغ شارژ") {
		t.Fatalf("below-minimum prompt missing: %s", sentText)
	}
}

func TestStartResumesActivePaymentAndTopup(t *testing.T) {
	api, backendServer := testBackend(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/actors/resolve":
			_, _ = w.Write([]byte(`{"telegram_id":42,"role":"customer"}`))
		case "/v1/payment-intents/active":
			_, _ = w.Write([]byte(`{"payment_intents":[{"id":19,"status":"receipt_submitted","amount_toman":90000},{"id":17,"status":"awaiting_receipt","amount_toman":120000}],"next_cursor":null,"payment_intent":{"id":19,"status":"receipt_submitted","amount_toman":90000}}`))
		case "/v1/wallet/topups/active":
			_, _ = w.Write([]byte(`{"topups":[{"id":23,"status":"awaiting_receipt","amount_toman":75000}],"next_cursor":null,"topup":{"id":23,"status":"awaiting_receipt","amount_toman":75000}}`))
		default:
			t.Errorf("unexpected backend request: %s %s", r.Method, r.URL.String())
			http.NotFound(w, r)
		}
	})
	defer backendServer.Close()
	var sent map[string]any
	tg := fakeTelegramServer(t, func(method string, body map[string]any) {
		if method == "sendMessage" {
			sent = body
		}
	})
	defer tg.Close()
	bot, err := telebot.NewBot(telebot.Settings{Token: "test-token", URL: tg.URL, Synchronous: true})
	if err != nil {
		t.Fatal(err)
	}
	app := &botApp{api: api, states: make(map[int64]conversation)}
	app.register(bot)
	bot.ProcessUpdate(telebot.Update{Message: &telebot.Message{
		ID: 7, Sender: &telebot.User{ID: 42}, Chat: &telebot.Chat{ID: 42, Type: telebot.ChatPrivate}, Text: "/start",
	}})
	if sent == nil || !strings.Contains(fmt.Sprint(sent["text"]), "درخواست‌های پرداخت") || !strings.Contains(fmt.Sprint(sent["text"]), "#19") {
		t.Fatalf("start did not offer receipt recovery: %#v", sent)
	}
	markup := requestMarkup(t, sent)
	encoded, _ := json.Marshal(markup)
	for _, want := range []string{"resume-payment", "resume-topup", "17", "23", "home"} {
		if !strings.Contains(string(encoded), want) {
			t.Fatalf("resume menu missing %q: %s", want, encoded)
		}
	}
}

func TestReceiptResumeMenuPaginatesOlderOutstandingRequests(t *testing.T) {
	var olderCursor string
	api, backendServer := testBackend(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/actors/resolve":
			_, _ = w.Write([]byte(`{"telegram_id":42,"role":"customer"}`))
		case "/v1/payment-intents/active":
			if r.URL.Query().Get("before_id") == "5" {
				olderCursor = "5"
				_, _ = w.Write([]byte(`{"payment_intents":[{"id":4,"status":"awaiting_receipt","amount_toman":40000},{"id":3,"status":"receipt_submitted","amount_toman":30000},{"id":2,"status":"awaiting_receipt","amount_toman":20000},{"id":1,"status":"awaiting_receipt","amount_toman":10000}],"next_cursor":null,"payment_intent":{"id":4,"status":"awaiting_receipt","amount_toman":40000}}`))
				return
			}
			_, _ = w.Write([]byte(`{"payment_intents":[{"id":10,"status":"receipt_submitted","amount_toman":100000},{"id":9,"status":"receipt_submitted","amount_toman":90000},{"id":8,"status":"receipt_submitted","amount_toman":80000},{"id":7,"status":"receipt_submitted","amount_toman":70000},{"id":6,"status":"receipt_submitted","amount_toman":60000},{"id":5,"status":"receipt_submitted","amount_toman":50000},{"id":4,"status":"awaiting_receipt","amount_toman":40000},{"id":3,"status":"receipt_submitted","amount_toman":30000},{"id":2,"status":"awaiting_receipt","amount_toman":20000},{"id":1,"status":"awaiting_receipt","amount_toman":10000}],"next_cursor":null,"payment_intent":{"id":10,"status":"receipt_submitted","amount_toman":100000}}`))
		case "/v1/wallet/topups/active":
			_, _ = w.Write([]byte(`{"topups":[],"next_cursor":null,"topup":null}`))
		default:
			t.Errorf("unexpected backend request: %s %s", r.Method, r.URL.String())
			http.NotFound(w, r)
		}
	})
	defer backendServer.Close()
	var sent, edited map[string]any
	tg := fakeTelegramServer(t, func(method string, body map[string]any) {
		if method == "sendMessage" {
			sent = body
		}
		if method == "editMessageText" {
			edited = body
		}
	})
	defer tg.Close()
	bot, err := telebot.NewBot(telebot.Settings{Token: "test-token", URL: tg.URL, Synchronous: true})
	if err != nil {
		t.Fatal(err)
	}
	app := &botApp{api: api, states: make(map[int64]conversation)}
	app.register(bot)
	bot.ProcessUpdate(telebot.Update{Message: &telebot.Message{
		ID: 7, Sender: &telebot.User{ID: 42}, Chat: &telebot.Chat{ID: 42, Type: telebot.ChatPrivate}, Text: "/start",
	}})
	if sent == nil || !strings.Contains(fmt.Sprint(sent["text"]), "#10") {
		t.Fatalf("newest recovery page missing: %#v", sent)
	}
	markupBytes, _ := json.Marshal(requestMarkup(t, sent))
	var markup struct {
		InlineKeyboard [][]struct {
			Data string `json:"callback_data"`
		} `json:"inline_keyboard"`
	}
	if err = json.Unmarshal(markupBytes, &markup); err != nil {
		t.Fatal(err)
	}
	var next string
	for _, row := range markup.InlineKeyboard {
		for _, button := range row {
			if strings.Contains(button.Data, "receipt-page-next") {
				next = button.Data
			}
		}
	}
	if next == "" {
		t.Fatalf("newest page has no older-page control: %s", markupBytes)
	}
	bot.ProcessUpdate(telebot.Update{Callback: &telebot.Callback{
		ID: "older-page", Sender: &telebot.User{ID: 42}, Data: next,
		Message: &telebot.Message{ID: 99, Sender: &telebot.User{}, Chat: &telebot.Chat{ID: 42, Type: telebot.ChatPrivate}},
	}})
	if olderCursor != "5" {
		t.Fatalf("next screen before_id=%q, want exclusive cursor 5", olderCursor)
	}
	if edited == nil || !strings.Contains(fmt.Sprint(edited["text"]), "#4") || strings.Contains(fmt.Sprint(edited["text"]), "#10") {
		t.Fatalf("older recovery page incorrect: %#v", edited)
	}
}

func TestPhotoWithoutLocalStateChoosesOlderAwaitingBehindSubmittedRequest(t *testing.T) {
	var receiptPosts int
	var receivedFileID string
	api, backendServer := testBackend(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/actors/resolve":
			_, _ = w.Write([]byte(`{"telegram_id":42,"role":"customer"}`))
		case "/v1/payment-intents/active":
			_, _ = w.Write([]byte(`{"payment_intents":[{"id":19,"status":"receipt_submitted","amount_toman":90000},{"id":17,"status":"awaiting_receipt","amount_toman":120000}],"next_cursor":null,"payment_intent":{"id":19,"status":"receipt_submitted","amount_toman":90000}}`))
		case "/v1/wallet/topups/active":
			_, _ = w.Write([]byte(`{"topups":[],"next_cursor":null,"topup":null}`))
		case "/v1/payment-intents/17/receipt":
			receiptPosts++
			var body map[string]string
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode receipt body: %v", err)
			}
			receivedFileID = body["telegram_file_id"]
			_, _ = w.Write([]byte(`{"payment_intent_id":17,"status":"receipt_submitted"}`))
		case "/v1/features":
			_, _ = w.Write([]byte(`{"features":{},"text":{}}`))
		default:
			t.Errorf("unexpected backend request: %s %s", r.Method, r.URL.String())
			http.NotFound(w, r)
		}
	})
	defer backendServer.Close()
	tg := fakeTelegramServer(t, nil)
	defer tg.Close()
	bot, err := telebot.NewBot(telebot.Settings{Token: "test-token", URL: tg.URL, Synchronous: true})
	if err != nil {
		t.Fatal(err)
	}
	app := &botApp{api: api, states: make(map[int64]conversation)}
	ctx := bot.NewContext(telebot.Update{Message: &telebot.Message{
		ID: 8, Sender: &telebot.User{ID: 42}, Chat: &telebot.Chat{ID: 42, Type: telebot.ChatPrivate},
		Photo: &telebot.Photo{File: telebot.File{FileID: "receipt-photo"}},
	}})
	if err = app.photo(ctx); err != nil {
		t.Fatal(err)
	}
	if receiptPosts != 1 || receivedFileID != "receipt-photo" {
		t.Fatalf("receipt recovery post count/file = %d/%q, want 1/receipt-photo", receiptPosts, receivedFileID)
	}
}

func TestAdminReceiptViewAndConfirmedRejectUsePrivateBackendRoutes(t *testing.T) {
	var rejected, approved int
	api, backendServer := testBackend(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/actors/resolve":
			_, _ = w.Write([]byte(`{"telegram_id":96937669,"role":"admin"}`))
		case "/v1/admin/config":
			_, _ = w.Write([]byte(`{"channel":"retail"}`))
		case "/v1/admin/payments":
			_, _ = w.Write([]byte(`[{"id":12,"telegram_id":42,"amount_toman":120000,"status":"receipt_submitted","telegram_file_id":"evidence-file"}]`))
		case "/v1/admin/topups":
			_, _ = w.Write([]byte(`[{"id":13,"telegram_id":42,"amount_toman":75000,"status":"receipt_submitted","telegram_file_id":"topup-evidence"}]`))
		case "/v1/payment-intents/12/reject":
			if r.Method != http.MethodPost {
				t.Errorf("reject method = %s, want POST", r.Method)
			}
			rejected++
			_, _ = w.Write([]byte(`{"payment_intent_id":12,"status":"rejected","already_rejected":false}`))
		case "/v1/payment-intents/12/approve":
			approved++
			_, _ = w.Write([]byte(`{"payment_intent_id":12,"status":"approved"}`))
		default:
			t.Errorf("unexpected backend request: %s %s", r.Method, r.URL.String())
			http.NotFound(w, r)
		}
	})
	defer backendServer.Close()
	var photoFileID string
	var photoCaption string
	var lastMarkup map[string]any
	tg := fakeTelegramServer(t, func(method string, body map[string]any) {
		if method == "sendPhoto" {
			photoFileID, _ = body["photo"].(string)
			photoCaption, _ = body["caption"].(string)
		}
		if method == "editMessageText" || method == "sendMessage" {
			lastMarkup = requestMarkup(t, body)
		}
	})
	defer tg.Close()
	bot, err := telebot.NewBot(telebot.Settings{Token: "test-token", URL: tg.URL, Synchronous: true})
	if err != nil {
		t.Fatal(err)
	}
	app := &botApp{api: api, states: make(map[int64]conversation)}
	app.register(bot)
	app.setState(adminTelegramID, conversation{})
	adminMessage := &telebot.Message{ID: 99, Sender: &telebot.User{}, Chat: &telebot.Chat{ID: adminTelegramID, Type: telebot.ChatPrivate}}
	bot.ProcessUpdate(telebot.Update{Callback: &telebot.Callback{
		ID: "pending-list", Sender: &telebot.User{ID: adminTelegramID},
		Data: "\fnav|" + app.state(adminTelegramID).Nonce + "|pending-payments", Message: adminMessage,
	}})
	listMarkup, _ := json.Marshal(lastMarkup)
	if !strings.Contains(string(listMarkup), "view-payment-receipt") || strings.Contains(string(listMarkup), "approve-payment") || strings.Contains(string(listMarkup), "reject-payment") {
		t.Fatalf("payment list must require opening evidence before decisions: %s", listMarkup)
	}
	bot.ProcessUpdate(telebot.Update{Callback: &telebot.Callback{
		ID: "forged-approve-before-review", Sender: &telebot.User{ID: adminTelegramID},
		Data: "\fnav|" + app.state(adminTelegramID).Nonce + "|approve-payment|12", Message: adminMessage,
	}})
	if approved != 0 {
		t.Fatal("payment was approved before its receipt was privately reviewed")
	}
	bot.ProcessUpdate(telebot.Update{Callback: &telebot.Callback{
		ID: "pending-topup-list", Sender: &telebot.User{ID: adminTelegramID},
		Data: "\fnav|" + app.state(adminTelegramID).Nonce + "|pending-topups", Message: adminMessage,
	}})
	listMarkup, _ = json.Marshal(lastMarkup)
	if !strings.Contains(string(listMarkup), "view-topup-receipt") || strings.Contains(string(listMarkup), "approve-topup") || strings.Contains(string(listMarkup), "reject-topup") {
		t.Fatalf("top-up list must require opening evidence before decisions: %s", listMarkup)
	}
	bot.ProcessUpdate(telebot.Update{Callback: &telebot.Callback{
		ID: "pending-payment-list-again", Sender: &telebot.User{ID: adminTelegramID},
		Data: "\fnav|" + app.state(adminTelegramID).Nonce + "|pending-payments", Message: adminMessage,
	}})
	bot.ProcessUpdate(telebot.Update{Callback: &telebot.Callback{
		ID: "view-receipt", Sender: &telebot.User{ID: adminTelegramID},
		Data: "\fnav|" + app.state(adminTelegramID).Nonce + "|view-payment-receipt|12", Message: adminMessage,
	}})
	if photoFileID != "evidence-file" {
		t.Fatalf("admin receipt photo file_id = %q, want evidence-file", photoFileID)
	}
	if !strings.Contains(photoCaption, "42") {
		t.Fatalf("admin receipt caption lacks customer Telegram ID: %q", photoCaption)
	}
	if lastMarkup == nil {
		t.Fatal("receipt view did not include approve/reject controls")
	}
	encoded, _ := json.Marshal(lastMarkup)
	if !strings.Contains(string(encoded), "approve-payment") || !strings.Contains(string(encoded), "reject-payment") {
		t.Fatalf("receipt detail lacks review controls: %s", encoded)
	}
	bot.ProcessUpdate(telebot.Update{Callback: &telebot.Callback{
		ID: "reject-confirmation", Sender: &telebot.User{ID: adminTelegramID},
		Data: "\fnav|" + app.state(adminTelegramID).Nonce + "|reject-payment|12", Message: adminMessage,
	}})
	encoded, _ = json.Marshal(lastMarkup)
	if !strings.Contains(string(encoded), "confirm-reject-payment") {
		t.Fatalf("reject action did not require confirmation: %s", encoded)
	}
	bot.ProcessUpdate(telebot.Update{Callback: &telebot.Callback{
		ID: "reject-confirmed", Sender: &telebot.User{ID: adminTelegramID},
		Data: "\fnav|" + app.state(adminTelegramID).Nonce + "|confirm-reject-payment|12", Message: adminMessage,
	}})
	if rejected != 1 {
		t.Fatalf("payment reject calls=%d, want one", rejected)
	}
	if approved != 0 {
		t.Fatalf("unexpected payment approvals=%d", approved)
	}
	group := bot.NewContext(telebot.Update{Message: &telebot.Message{Sender: &telebot.User{ID: adminTelegramID}, Chat: &telebot.Chat{ID: -9, Type: telebot.ChatGroup}}})
	if _, err = app.requireRetailAdmin(group); err == nil {
		t.Fatal("admin access was allowed from a group chat")
	}
}

func TestAdminPendingReceiptListsPaginateBeyondTenWithoutDecisionButtons(t *testing.T) {
	var listCalls, topupCalls int
	api, backendServer := testBackend(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/actors/resolve":
			_, _ = w.Write([]byte(`{"telegram_id":96937669,"role":"admin"}`))
		case "/v1/admin/config":
			_, _ = w.Write([]byte(`{"channel":"retail"}`))
		case "/v1/admin/payments":
			listCalls++
			items := make([]pendingReceiptView, 12)
			for i := range items {
				items[i] = pendingReceiptView{ID: int64(i + 1), Amount: int64(1000 + i), Status: "receipt_submitted", TelegramFileID: fmt.Sprintf("file-%d", i+1)}
			}
			_ = json.NewEncoder(w).Encode(items)
		case "/v1/admin/topups":
			topupCalls++
			items := make([]pendingReceiptView, 12)
			for i := range items {
				items[i] = pendingReceiptView{ID: int64(i + 101), Amount: int64(2000 + i), Status: "receipt_submitted", TelegramFileID: fmt.Sprintf("topup-file-%d", i+101)}
			}
			_ = json.NewEncoder(w).Encode(items)
		default:
			t.Errorf("unexpected backend request: %s %s", r.Method, r.URL.String())
			http.NotFound(w, r)
		}
	})
	defer backendServer.Close()
	var lastMessage map[string]any
	tg := fakeTelegramServer(t, func(method string, body map[string]any) {
		if method == "editMessageText" || method == "sendMessage" {
			lastMessage = body
		}
	})
	defer tg.Close()
	bot, err := telebot.NewBot(telebot.Settings{Token: "test-token", URL: tg.URL, Synchronous: true})
	if err != nil {
		t.Fatal(err)
	}
	app := &botApp{api: api, states: make(map[int64]conversation)}
	app.register(bot)
	app.setState(adminTelegramID, conversation{})
	adminMessage := &telebot.Message{ID: 99, Sender: &telebot.User{}, Chat: &telebot.Chat{ID: adminTelegramID, Type: telebot.ChatPrivate}}
	bot.ProcessUpdate(telebot.Update{Callback: &telebot.Callback{
		ID: "pending-page-one", Sender: &telebot.User{ID: adminTelegramID},
		Data: "\fnav|" + app.state(adminTelegramID).Nonce + "|pending-payments", Message: adminMessage,
	}})
	if lastMessage == nil || !strings.Contains(fmt.Sprint(lastMessage["text"]), "1–8 از 12") {
		t.Fatalf("first pending page missing range/count: %#v", lastMessage)
	}
	markupBytes, _ := json.Marshal(requestMarkup(t, lastMessage))
	var markup struct {
		InlineKeyboard [][]struct {
			Data string `json:"callback_data"`
		} `json:"inline_keyboard"`
	}
	if err = json.Unmarshal(markupBytes, &markup); err != nil {
		t.Fatal(err)
	}
	var next string
	viewCount := 0
	for _, row := range markup.InlineKeyboard {
		for _, button := range row {
			if strings.Contains(button.Data, "view-payment-receipt") {
				viewCount++
			}
			if strings.Contains(button.Data, "pending-payments-next") {
				next = button.Data
			}
			if strings.Contains(button.Data, "approve-payment") || strings.Contains(button.Data, "reject-payment") {
				t.Fatalf("pending list exposed a decision before evidence review: %s", button.Data)
			}
		}
	}
	if viewCount != adminReceiptMenuPageSize || next == "" {
		t.Fatalf("first page controls: receipt buttons=%d next=%q markup=%s", viewCount, next, markupBytes)
	}
	bot.ProcessUpdate(telebot.Update{Callback: &telebot.Callback{
		ID: "pending-page-two", Sender: &telebot.User{ID: adminTelegramID}, Data: next, Message: adminMessage,
	}})
	if listCalls != 2 || !strings.Contains(fmt.Sprint(lastMessage["text"]), "9–12 از 12") || !strings.Contains(fmt.Sprint(lastMessage["text"]), "12") {
		t.Fatalf("second pending page failed to expose later items: calls=%d message=%#v", listCalls, lastMessage)
	}
	bot.ProcessUpdate(telebot.Update{Callback: &telebot.Callback{
		ID: "topup-page-one", Sender: &telebot.User{ID: adminTelegramID},
		Data: "\fnav|" + app.state(adminTelegramID).Nonce + "|pending-topups", Message: adminMessage,
	}})
	if !strings.Contains(fmt.Sprint(lastMessage["text"]), "1–8 از 12") {
		t.Fatalf("first pending top-up page missing range/count: %#v", lastMessage)
	}
	markupBytes, _ = json.Marshal(requestMarkup(t, lastMessage))
	if err = json.Unmarshal(markupBytes, &markup); err != nil {
		t.Fatal(err)
	}
	next = ""
	viewCount = 0
	for _, row := range markup.InlineKeyboard {
		for _, button := range row {
			if strings.Contains(button.Data, "view-topup-receipt") {
				viewCount++
			}
			if strings.Contains(button.Data, "pending-topups-next") {
				next = button.Data
			}
			if strings.Contains(button.Data, "approve-topup") || strings.Contains(button.Data, "reject-topup") {
				t.Fatalf("pending top-up list exposed a decision before evidence review: %s", button.Data)
			}
		}
	}
	if viewCount != adminReceiptMenuPageSize || next == "" {
		t.Fatalf("first top-up page controls: receipt buttons=%d next=%q markup=%s", viewCount, next, markupBytes)
	}
	bot.ProcessUpdate(telebot.Update{Callback: &telebot.Callback{
		ID: "topup-page-two", Sender: &telebot.User{ID: adminTelegramID}, Data: next, Message: adminMessage,
	}})
	if topupCalls != 2 || !strings.Contains(fmt.Sprint(lastMessage["text"]), "9–12 از 12") || !strings.Contains(fmt.Sprint(lastMessage["text"]), "112") {
		t.Fatalf("second pending top-up page failed to expose later items: calls=%d message=%#v", topupCalls, lastMessage)
	}
}

func TestCallbackBackendFailureReplacesStaleKeyboard(t *testing.T) {
	api, backendServer := testBackend(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/actors/resolve" {
			_, _ = w.Write([]byte(`{"telegram_id":42,"role":"customer"}`))
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":{"code":"unavailable","message":"retry"}}`))
	})
	defer backendServer.Close()
	var edited map[string]any
	telegramServer := fakeTelegramServer(t, func(method string, body map[string]any) {
		if method == "editMessageText" {
			edited = body
		}
	})
	defer telegramServer.Close()
	bot, err := telebot.NewBot(telebot.Settings{Token: "test-token", URL: telegramServer.URL, Synchronous: true})
	if err != nil {
		t.Fatal(err)
	}
	app := &botApp{api: api, states: make(map[int64]conversation)}
	app.register(bot)
	app.setState(42, conversation{})
	nonce := app.state(42).Nonce
	bot.ProcessUpdate(telebot.Update{Callback: &telebot.Callback{ID: "failure", Sender: &telebot.User{ID: 42}, Data: "\fnav|" + nonce + "|plans-paid", Message: &telebot.Message{ID: 99, Sender: &telebot.User{}, Chat: &telebot.Chat{ID: 42, Type: telebot.ChatPrivate}}}})
	if edited == nil {
		t.Fatal("backend failure did not replace callback message with a fresh menu")
	}
	markup := requestMarkup(t, edited)
	rows, _ := markup["inline_keyboard"].([]any)
	if len(rows) == 0 {
		encoded, _ := json.Marshal(edited)
		t.Fatalf("fresh error screen has no retry action: %s", encoded)
	}
	if _, ok := app.consumeCallbackState(42, nonce); ok {
		t.Fatal("error screen retained consumed callback nonce")
	}
}

func TestFailedPurchaseKeepsQuoteAndIdempotencyForRetry(t *testing.T) {
	var purchaseCalls int
	var idempotencyKeys []string
	api, backendServer := testBackend(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/actors/resolve":
			_, _ = w.Write([]byte(`{"telegram_id":42,"role":"customer"}`))
		case "/v1/features":
			_, _ = w.Write([]byte(`{"features":{"purchases_enabled":true,"wallet_enabled":true},"text":{}}`))
		case "/v1/purchases":
			purchaseCalls++
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			idempotencyKeys = append(idempotencyKeys, body["idempotency_key"].(string))
			if purchaseCalls == 1 {
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = w.Write([]byte(`{"error":{"code":"unavailable","message":"retry"}}`))
				return
			}
			_, _ = w.Write([]byte(`{"order_id":4,"amount_toman":12000,"status":"paid"}`))
		default:
			http.NotFound(w, r)
		}
	})
	defer backendServer.Close()
	var edited map[string]any
	telegramServer := fakeTelegramServer(t, func(method string, body map[string]any) {
		if method == "editMessageText" {
			edited = body
		}
	})
	defer telegramServer.Close()
	bot, err := telebot.NewBot(telebot.Settings{Token: "test-token", URL: telegramServer.URL, Synchronous: true})
	if err != nil {
		t.Fatal(err)
	}
	app := &botApp{api: api, states: make(map[int64]conversation)}
	app.register(bot)
	app.setState(42, conversation{QuoteID: 101, QuotePrice: 12000, OperationKey: "stable-purchase-op"})
	nonce := app.state(42).Nonce
	message := &telebot.Message{ID: 99, Sender: &telebot.User{}, Chat: &telebot.Chat{ID: 42, Type: telebot.ChatPrivate}}
	bot.ProcessUpdate(telebot.Update{Callback: &telebot.Callback{ID: "purchase-first", Sender: &telebot.User{ID: 42}, Data: "\fnav|" + nonce + "|method-wallet", Message: message}})
	if purchaseCalls != 1 || edited == nil {
		t.Fatalf("first callback did not produce retry screen: calls=%d edited=%v", purchaseCalls, edited != nil)
	}
	markup := requestMarkup(t, edited)
	rows, _ := markup["inline_keyboard"].([]any)
	if len(rows) == 0 {
		t.Fatal("retry screen has no usable buttons")
	}
	newNonce := app.state(42).Nonce
	bot.ProcessUpdate(telebot.Update{Callback: &telebot.Callback{ID: "purchase-retry", Sender: &telebot.User{ID: 42}, Data: "\fnav|" + newNonce + "|retry-purchase", Message: message}})
	if purchaseCalls != 2 {
		t.Fatalf("retry did not reach backend: calls=%d", purchaseCalls)
	}
	if len(idempotencyKeys) != 2 || idempotencyKeys[0] != idempotencyKeys[1] || idempotencyKeys[0] != "purchase-stable-purchase-op" {
		t.Fatalf("retry changed idempotency key: %#v", idempotencyKeys)
	}
}
