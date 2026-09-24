package reseller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"example.com/xui-commerce/backend/internal/botruntime/backend"
	"gopkg.in/telebot.v3"
)

func TestPaymentInstructionPatchPreservesOtherFields(t *testing.T) {
	current := paymentInstructions{CardNumber: "1111", CardOwner: "Existing owner", Instructions: "Existing instructions"}
	tests := []struct {
		field, value string
		want         paymentInstructions
	}{
		{"card_number", "2222", paymentInstructions{CardNumber: "2222", CardOwner: "Existing owner", Instructions: "Existing instructions"}},
		{"card_owner", "New owner", paymentInstructions{CardNumber: "1111", CardOwner: "New owner", Instructions: "Existing instructions"}},
		{"instructions", "New instructions", paymentInstructions{CardNumber: "1111", CardOwner: "Existing owner", Instructions: "New instructions"}},
	}
	for _, tt := range tests {
		t.Run(tt.field, func(t *testing.T) {
			got, err := buildPaymentInstructionPatch(current, tt.field, tt.value)
			if err != nil {
				t.Fatal(err)
			}
			encoded, err := json.Marshal(got)
			if err != nil {
				t.Fatal(err)
			}
			var actual paymentInstructions
			if err := json.Unmarshal(encoded, &actual); err != nil {
				t.Fatal(err)
			}
			if actual != tt.want {
				t.Fatalf("patch = %#v, want %#v", actual, tt.want)
			}
		})
	}
	if _, err := buildPaymentInstructionPatch(current, "unknown", "value"); err == nil {
		t.Fatal("unknown payment fields must be rejected")
	}
}

func TestPaymentInstructionEditReadsAndReplacesFullObject(t *testing.T) {
	var patch paymentInstructions
	var methods []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-secret" {
			t.Errorf("authorization header missing")
		}
		if r.Header.Get("X-Actor-Telegram-ID") != "96937669" {
			t.Errorf("unexpected actor header: %q", r.Header.Get("X-Actor-Telegram-ID"))
		}
		methods = append(methods, r.Method+" "+r.URL.Path)
		switch r.Method + " " + r.URL.Path {
		case "GET /v1/admin/config":
			_, _ = io.WriteString(w, `{"payment_instructions":{"card_number":"1111","card_owner":"Existing owner","instructions":"Existing instructions"}}`)
		case "PATCH /v1/admin/config/payment-instructions":
			defer r.Body.Close()
			body, _ := io.ReadAll(r.Body)
			if err := json.Unmarshal(body, &patch); err != nil {
				t.Errorf("decode patch: %v", err)
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	api, err := backend.New(server.URL, "test-secret", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	app := &botApp{api: api}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := app.updatePaymentInstruction(ctx, adminTelegramID, "card_number", "2222"); err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(methods, ","), "GET /v1/admin/config,PATCH /v1/admin/config/payment-instructions"; got != want {
		t.Fatalf("requests = %q, want %q", got, want)
	}
	want := paymentInstructions{CardNumber: "2222", CardOwner: "Existing owner", Instructions: "Existing instructions"}
	if patch != want {
		t.Fatalf("patch = %#v, want %#v", patch, want)
	}
}

func TestResellerApprovalSettingIsDisplayedAsCurrentValue(t *testing.T) {
	var cfg adminConfig
	if err := json.Unmarshal([]byte(`{"settings":{"reseller_approved_required":true}}`), &cfg); err != nil {
		t.Fatal(err)
	}
	if !cfg.Settings.ResellerApprovedRequired {
		t.Fatal("config did not decode the enabled approval requirement")
	}
	if got := resellerApprovalButtonLabel(cfg.Settings.ResellerApprovedRequired); got != "فقط reseller تأییدشده: بله" {
		t.Fatalf("label = %q", got)
	}
	if got := resellerApprovalButtonLabel(false); got != "فقط reseller تأییدشده: خیر" {
		t.Fatalf("disabled label = %q", got)
	}
}

func TestAdminConfigDecodesBackendStringIdentifiers(t *testing.T) {
	var cfg adminConfig
	data := `{"deployment_id":"reseller-turk1","panel":{"id":"panel-17","base_url":"https://panel.example","token_configured":true}}`
	if err := json.Unmarshal([]byte(data), &cfg); err != nil {
		t.Fatalf("decode admin config: %v", err)
	}
	if cfg.DeploymentID != "reseller-turk1" || cfg.Panel.ID != "panel-17" {
		t.Fatalf("unexpected identifiers: deployment=%q panel=%q", cfg.DeploymentID, cfg.Panel.ID)
	}
}

func TestAdminGateRequiresConfiguredIDAndBackendRole(t *testing.T) {
	if !isAdmin(actor{TelegramID: adminTelegramID, Role: "admin"}) {
		t.Fatal("configured backend admin should pass the UI gate")
	}
	if isAdmin(actor{TelegramID: adminTelegramID, Role: "reseller"}) {
		t.Fatal("matching Telegram ID without backend admin role must not pass")
	}
	if isAdmin(actor{TelegramID: adminTelegramID + 1, Role: "admin"}) {
		t.Fatal("other Telegram IDs must not pass")
	}
}

func TestAdminMenuRequiresPrivateChat(t *testing.T) {
	admin := actor{TelegramID: adminTelegramID, Role: "admin"}
	for name, chat := range map[string]*telebot.Chat{
		"private":    {Type: telebot.ChatPrivate},
		"group":      {Type: telebot.ChatGroup},
		"supergroup": {Type: telebot.ChatSuperGroup},
		"missing":    nil,
	} {
		t.Run(name, func(t *testing.T) {
			if got := canOpenAdmin(chat, admin); got != (name == "private") {
				t.Fatalf("canOpenAdmin = %t for %s chat", got, name)
			}
		})
	}
	if canOpenAdmin(&telebot.Chat{Type: telebot.ChatPrivate}, actor{TelegramID: adminTelegramID, Role: "reseller"}) {
		t.Fatal("private-chat access must still require the backend admin role")
	}
}

func TestPanelConfigRequiresPrivateChat(t *testing.T) {
	for name, chat := range map[string]*telebot.Chat{
		"private":    {Type: telebot.ChatPrivate},
		"group":      {Type: telebot.ChatGroup},
		"supergroup": {Type: telebot.ChatSuperGroup},
		"missing":    nil,
	} {
		t.Run(name, func(t *testing.T) {
			if got := isPrivateChat(chat); got != (name == "private") {
				t.Fatalf("isPrivateChat = %t", got)
			}
		})
	}
}

func TestPanelTokenFromGroupIsDeletedButNeverSubmitted(t *testing.T) {
	deleted, submitted := false, false
	err := submitPanelToken(&telebot.Chat{Type: telebot.ChatGroup}, func() error {
		deleted = true
		return nil
	}, func() error {
		submitted = true
		return nil
	})
	if !deleted || !errors.Is(err, errPanelPrivateChat) || submitted {
		t.Fatalf("group token handling: deleted=%t submitted=%t err=%v", deleted, submitted, err)
	}
}

func TestPrivatePanelTokenIsDeletedBeforeSubmission(t *testing.T) {
	var events []string
	err := submitPanelToken(&telebot.Chat{Type: telebot.ChatPrivate}, func() error {
		events = append(events, "delete")
		return nil
	}, func() error {
		events = append(events, "submit")
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(events, ","), "delete,submit"; got != want {
		t.Fatalf("operation order = %q, want %q", got, want)
	}
}

func TestPanelTokenDeleteFailureStopsSubmission(t *testing.T) {
	submitted := false
	err := submitPanelToken(&telebot.Chat{Type: telebot.ChatPrivate}, func() error {
		return fmt.Errorf("telegram delete failed")
	}, func() error {
		submitted = true
		return nil
	})
	if !errors.Is(err, errPanelTokenDelete) || submitted {
		t.Fatalf("delete failure: submitted=%t err=%v", submitted, err)
	}
}

func TestAttemptKeyIsStableForRetriesAndFreshForNewUpdate(t *testing.T) {
	firstDelivery := attemptKey(41, 9, 5001, "trial-7")
	if retry := attemptKey(41, 9, 5001, "trial-7"); retry != firstDelivery {
		t.Fatalf("retry key = %q, want %q", retry, firstDelivery)
	}
	if nextDay := attemptKey(41, 9, 6001, "trial-7"); nextDay == firstDelivery {
		t.Fatal("a later button press must get a fresh attempt key")
	}
}

func TestCallbackDataRejectsStaleMenuVersions(t *testing.T) {
	for _, raw := range []string{"trial|7|vcurrent-token", "\fgo|trial|7|vcurrent-token"} {
		app := &botApp{menus: map[int64]string{41: "current-token"}}
		parts, valid := app.callbackData(41, raw)
		if !valid || len(parts) != 2 || parts[0] != "trial" || parts[1] != "7" {
			t.Fatalf("current callback %q = %#v, valid=%t", raw, parts, valid)
		}
		if _, valid := app.callbackData(41, raw); valid {
			t.Fatalf("callback token was reusable after accepting %q", raw)
		}
	}
	app := &botApp{menus: map[int64]string{41: "current-token"}}
	if _, valid := app.callbackData(41, "trial|7|vprevious-token"); valid {
		t.Fatal("callback from a previous screen must be rejected")
	}
	if _, valid := app.callbackData(41, "\fother|trial|7|vcurrent-token"); valid {
		t.Fatal("callback with a different telebot unique name must be rejected")
	}
	if _, valid := app.callbackData(41, "trial|7"); valid {
		t.Fatal("callback without a screen token must be rejected")
	}
	keyboard := &telebot.ReplyMarkup{InlineKeyboard: [][]telebot.InlineButton{{{Data: "trial|7"}}}}
	bindMenuToken(keyboard, "current-token")
	if got := keyboard.InlineKeyboard[0][0].Data; got != "trial|7|vcurrent-token" {
		t.Fatalf("bound callback = %q", got)
	}
}

func TestConcurrentDuplicateTrialAndQuoteCallbacksConsumeMenuOnce(t *testing.T) {
	for _, action := range []string{"trial|7", "purchase-default-name"} {
		t.Run(action, func(t *testing.T) {
			app := &botApp{menus: map[int64]string{41: "current-token"}}
			bot, err := telebot.NewBot(telebot.Settings{Offline: true, Synchronous: true})
			if err != nil {
				t.Fatal(err)
			}
			var accepted atomic.Int32
			registerCallbackHandlers(bot, func(c telebot.Context) error {
				if _, ok := app.callbackData(c.Sender().ID, c.Data()); ok {
					accepted.Add(1)
				}
				return nil
			})
			var wg sync.WaitGroup
			for i := 0; i < 32; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					bot.ProcessUpdate(telebot.Update{Callback: &telebot.Callback{
						ID: action, Sender: &telebot.User{ID: 41}, Data: "\fgo|" + action + "|vcurrent-token",
					}})
				}()
			}
			wg.Wait()
			if got := accepted.Load(); got != 1 {
				t.Fatalf("accepted callback dispatches = %d, want one", got)
			}
			app.mu.Lock()
			defer app.mu.Unlock()
			if _, exists := app.menus[41]; exists {
				t.Fatal("consumed menu token should be invalid until the handler renders the next menu")
			}
		})
	}
}

func TestTelebotCallbackUniqueDispatchStripsWirePrefix(t *testing.T) {
	bot, err := telebot.NewBot(telebot.Settings{Offline: true, Synchronous: true})
	if err != nil {
		t.Fatal(err)
	}
	var got string
	registerCallbackHandlers(bot, func(c telebot.Context) error {
		got = c.Data()
		return nil
	})
	bot.ProcessUpdate(telebot.Update{Callback: &telebot.Callback{Data: "\fgo|trial|7|vcurrent-token"}})
	if got != "trial|7|vcurrent-token" {
		t.Fatalf("callback payload = %q; want the unique prefix stripped", got)
	}
}

func TestSubscriptionLinkPagesRespectTelegramTextLimitWithoutSkipping(t *testing.T) {
	links := []string{}
	for i := 0; i < 8; i++ {
		links = append(links, fmt.Sprintf("vless://%d-%s", i, strings.Repeat("x", 1100)))
	}
	s := subscription{DisplayName: "VPN", Links: links}
	start := 0
	seen := 0
	for {
		text, next, _ := renderSubscriptionDetails(s, start)
		if utf8.RuneCountInString(text) > 4096 {
			t.Fatalf("page exceeds Telegram text limit: %d", utf8.RuneCountInString(text))
		}
		for i := start; i < len(links) && i < start+maxSubscriptionLinksPerPage; i++ {
			if strings.Contains(text, links[i]) {
				seen++
			}
		}
		if next < 0 {
			break
		}
		if next <= start {
			t.Fatalf("pagination did not advance from %d: next %d", start, next)
		}
		start = next
	}
	if seen != len(links) {
		t.Fatalf("shown links = %d, want %d", seen, len(links))
	}
}

func TestSubscriptionLinkPaginationUsesHistoryAndSplitsLongLinks(t *testing.T) {
	long := "vless://" + strings.Repeat("L", 7000)
	s := subscription{DisplayName: "Service", Links: []string{long, "https://short.example/a", "vless://another"}}
	parts := subscriptionLinkParts(s.Links)
	if len(parts) < 4 {
		t.Fatalf("long links were not split into text-safe parts: %d", len(parts))
	}
	text, next, previous := renderSubscriptionDetails(s, 0)
	if next != 1 || previous != -1 {
		t.Fatalf("first cursor next=%d previous=%d, want 1/-1", next, previous)
	}
	if utf8.RuneCountInString(text) > 4096 || !strings.Contains(text, parts[0].text) {
		t.Fatal("first page exceeds Telegram limit or omits first link part")
	}
	text, next, previous = renderSubscriptionDetails(s, 1)
	if previous != 0 {
		t.Fatalf("uneven page previous cursor=%d, want actual prior page start 0", previous)
	}
	if utf8.RuneCountInString(text) > 4096 || !strings.Contains(text, parts[1].text) {
		t.Fatal("second page exceeds Telegram limit or omits next long-link part")
	}
	start := 0
	seen := make([]bool, len(parts))
	for {
		page, nextCursor, _ := renderSubscriptionDetails(s, start)
		if utf8.RuneCountInString(page) > 4096 {
			t.Fatalf("page exceeds Telegram limit: %d", utf8.RuneCountInString(page))
		}
		if nextCursor < 0 {
			nextCursor = len(parts)
		}
		if nextCursor <= start && start < len(parts) {
			t.Fatalf("pagination stuck at cursor %d", start)
		}
		for i := start; i < nextCursor; i++ {
			seen[i] = strings.Contains(page, parts[i].text)
		}
		if nextCursor == len(parts) {
			break
		}
		start = nextCursor
	}
	for i, ok := range seen {
		if !ok {
			t.Fatalf("link part %d was skipped", i)
		}
	}
}

func TestSubscriptionDetailsRenderOwnerLinksWithPagination(t *testing.T) {
	s := subscription{ID: 9, DisplayName: "My VPN", Email: "owner@example.test", Status: "active", Kind: "paid", IPLimit: 2, TrafficLimitBytes: 2_000_000_000, ExpiryTimeMS: 1_800_000_000_000, Links: []string{"vless://one", "vless://two", "vless://three", "vless://four", "vless://five", "vless://six"}}
	first, next, previous := renderSubscriptionDetails(s, 0)
	if next != 5 || previous != -1 || !strings.Contains(first, "owner@example.test") || !strings.Contains(first, "vless://one") || !strings.Contains(first, "vless://five") || strings.Contains(first, "vless://six") {
		t.Fatalf("first page or metadata is wrong: next=%d previous=%d, text=%q", next, previous, first)
	}
	second, next, previous := renderSubscriptionDetails(s, 5)
	if next != -1 || previous != 0 || !strings.Contains(second, "vless://six") {
		t.Fatalf("second page is wrong: next=%d previous=%d, text=%q", next, previous, second)
	}
}
