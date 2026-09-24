package enduser

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"example.com/xui-commerce/backend/internal/botruntime/backend"
	"gopkg.in/telebot.v3"
)

func TestAdminConfigDecodesBackendStringIdentifiers(t *testing.T) {
	const response = `{"deployment_id":"retail-finland","channel":"retail-finland","plans":[],"payment_instructions":{},"settings":{"retail_trial_reset_days":0,"features":{},"text":{}},"panel":{"id":"panel-retail-finland","base_url":"https://panel.example.test","token_configured":false}}`
	var cfg adminConfig
	if err := json.Unmarshal([]byte(response), &cfg); err != nil {
		t.Fatalf("decode backend admin config: %v", err)
	}
	if cfg.DeploymentID != "retail-finland" || cfg.Panel.ID != "panel-retail-finland" {
		t.Fatalf("backend string identifiers were not preserved: deployment=%q panel=%q", cfg.DeploymentID, cfg.Panel.ID)
	}
}

func TestStableKeyTracksTelegramMessageAndOperation(t *testing.T) {
	first := stableKey(41, 9, 120, "purchase")
	if first != stableKey(41, 9, 120, "purchase") {
		t.Fatal("same Telegram update must use the same idempotency key")
	}
	if first == stableKey(41, 9, 121, "purchase") {
		t.Fatal("different Telegram messages must not share an idempotency key")
	}
	if first == stableKey(41, 9, 120, "trial") {
		t.Fatal("different operations must not share an idempotency key")
	}
}

func TestFeatureGateDefaultsEnabledAndHonorsDisabledFlags(t *testing.T) {
	if !featureEnabled(publicFeatures{}, "purchases_enabled") {
		t.Fatal("an unset feature must retain the backend's enabled-by-default behavior")
	}
	if featureEnabled(publicFeatures{Features: map[string]bool{"purchases_enabled": false}}, "purchases_enabled") {
		t.Fatal("an explicitly disabled feature must be hidden")
	}
}

func TestPlanFieldCallbackPayloadFitsTelegramLimit(t *testing.T) {
	fields := []string{"name", "kind", "description", "base_price_toman", "price_per_extra_ip_toman", "price_per_gb_toman", "price_per_extra_month_toman", "ip_limits", "min_data_gb", "max_data_bytes", "expire_seconds", "test_ip_limit", "flow", "inbound_ids", "usage_description"}
	for _, field := range fields {
		code := planFieldCode(field)
		decoded, ok := planFieldFromCode(code)
		if !ok || decoded != field {
			t.Fatalf("field %q did not round trip through callback code %q", field, code)
		}
		payload := strings.Join([]string{"0123456789", "pf", "9223372036854775807", code}, "|")
		if len(payload) > 64 {
			t.Fatalf("callback payload for %q exceeds Telegram's 64-byte limit: %d", field, len(payload))
		}
	}
}

func TestPaymentInstructionPatchPreservesOtherFields(t *testing.T) {
	current := map[string]string{"card_number": "1111", "card_owner": "Existing owner", "instructions": "Pay exact amount"}
	updated := paymentInstructionPatch(current, "card_number", "2222")
	if updated["card_number"] != "2222" || updated["card_owner"] != "Existing owner" || updated["instructions"] != "Pay exact amount" {
		t.Fatalf("updating one payment field must preserve the other configured fields: %#v", updated)
	}
}

func TestSessionOperationKeyDeduplicatesDeliveryButSeparatesActions(t *testing.T) {
	same := sessionKey(41, 9, 120, "menu-a", "trial", "7")
	if same != sessionKey(41, 9, 120, "menu-a", "trial", "7") {
		t.Fatal("redelivery of the same action must reuse its idempotency key")
	}
	if same == sessionKey(41, 9, 120, "menu-b", "trial", "7") {
		t.Fatal("a new menu session must get a distinct idempotency key")
	}
	if same == sessionKey(41, 9, 120, "menu-a", "trial", "8") {
		t.Fatal("a different target must get a distinct idempotency key")
	}
	if same == sessionKey(41, 9, 120, "menu-a", "cancel", "7") {
		t.Fatal("a different operation must get a distinct idempotency key")
	}
}

func TestTelebotNavCallbackUniqueUsesDecodedUnique(t *testing.T) {
	if !isNavCallback(&telebot.Callback{Unique: "nav", Data: "nonce|home"}) {
		t.Fatal("telebot callback handler receives the decoded unique without the leading form-feed")
	}
	if isNavCallback(&telebot.Callback{Unique: "\fnav", Data: "nonce|home"}) {
		t.Fatal("raw callback encoding must not be treated as the decoded unique")
	}
}

func TestRegisteredNavRouteDecodesCallbackWithTelebot(t *testing.T) {
	bot, err := telebot.NewBot(telebot.Settings{Offline: true, Synchronous: true})
	if err != nil {
		t.Fatalf("create offline telebot: %v", err)
	}
	var gotUnique, gotData string
	registerCallbackRoutes(bot, func(c telebot.Context) error {
		gotUnique = c.Callback().Unique
		gotData = c.Data()
		return nil
	})

	bot.ProcessUpdate(telebot.Update{Callback: &telebot.Callback{
		Sender: &telebot.User{ID: 41},
		Data:   "\fnav|nonce|home",
	}})
	if gotUnique != "nav" || gotData != "nonce|home" {
		t.Fatalf("registered nav callback should be decoded, got unique=%q data=%q", gotUnique, gotData)
	}

	gotUnique, gotData = "", ""
	bot.ProcessUpdate(telebot.Update{Callback: &telebot.Callback{
		Sender: &telebot.User{ID: 41},
		Data:   "\fother|opaque",
	}})
	if gotUnique != "" || gotData != "\fother|opaque" {
		t.Fatalf("unknown callback should reach fallback without nav decoding, got unique=%q data=%q", gotUnique, gotData)
	}
}

func TestCallbackStateCanBeConsumedOnlyOnce(t *testing.T) {
	app := &botApp{states: make(map[int64]conversation)}
	app.setState(41, conversation{Step: "purchase-confirm"})
	initial := app.state(41)

	consumed, ok := app.consumeCallbackState(41, initial.Nonce)
	if !ok || consumed.Nonce != initial.Nonce || consumed.Step != "purchase-confirm" {
		t.Fatalf("first callback should consume its current state: %#v, %t", consumed, ok)
	}
	rotated := app.state(41)
	if rotated.Nonce == initial.Nonce {
		t.Fatal("consuming a callback must rotate the stored nonce")
	}
	if _, ok := app.consumeCallbackState(41, initial.Nonce); ok {
		t.Fatal("redelivered callback must not consume the old nonce twice")
	}
}

func TestAdminCommandRequiresConfiguredAdminInPrivateChat(t *testing.T) {
	admin := &telebot.User{ID: adminTelegramID}
	if !adminCommandSender(admin, &telebot.Chat{Type: telebot.ChatPrivate}) {
		t.Fatal("configured administrator should be able to enter /admin privately")
	}
	if adminCommandSender(&telebot.User{ID: 41}, &telebot.Chat{Type: telebot.ChatPrivate}) {
		t.Fatal("ordinary users must not enter the admin command")
	}
	if adminCommandSender(admin, &telebot.Chat{Type: telebot.ChatGroup}) {
		t.Fatal("admin controls must not open in group chats")
	}
	if adminCommandSender(nil, &telebot.Chat{Type: telebot.ChatPrivate}) || adminCommandSender(admin, nil) {
		t.Fatal("missing Telegram identity or chat must deny admin entry")
	}
}

func TestSubscriptionPagesExposeEveryLinkAndEmptyLinkSubscription(t *testing.T) {
	subscriptions := []subscriptionView{
		{ID: 10, Links: []string{"vless://one", "https://example.test/sub"}},
		{ID: 11},
	}
	pages := subscriptionLinkPages(subscriptions)
	if len(pages) != 3 {
		t.Fatalf("expected one page per connection link and one empty subscription page, got %d", len(pages))
	}
	if pages[0].Subscription.ID != 10 || pages[0].LinkIndex != 0 || pages[1].Subscription.ID != 10 || pages[1].LinkIndex != 1 || pages[2].Subscription.ID != 11 || pages[2].LinkIndex != -1 {
		t.Fatalf("subscription links were dropped or misordered: %#v", pages)
	}
}

func TestFailureDiagnosticUsesOnlySanitizedBackendCategory(t *testing.T) {
	status, category := failureDiagnostic(&backend.APIError{Status: 503, Code: "service_unavailable", Message: "sensitive backend detail"})
	if status != 503 || category != "service_unavailable" {
		t.Fatalf("expected safe status/category, got %d/%q", status, category)
	}
	status, category = failureDiagnostic(&backend.APIError{Status: 500, Code: "bad\nsecret", Message: "sensitive backend detail"})
	if status != 500 || category != "backend_error" {
		t.Fatalf("unsafe backend error code must be reduced to generic category, got %d/%q", status, category)
	}
}

func TestPanelURLPromptRequiresPrivateChat(t *testing.T) {
	stateSet, promptSent := false, false
	err := beginPanelURLPrompt(&telebot.Chat{Type: telebot.ChatGroup}, func() {
		stateSet = true
	}, func() error {
		promptSent = true
		return nil
	})
	if !errors.Is(err, errPanelPrivateChat) {
		t.Fatalf("group chat error = %v, want private-chat error", err)
	}
	if stateSet || promptSent {
		t.Fatalf("group callback started panel setup: stateSet=%t promptSent=%t", stateSet, promptSent)
	}
}

func TestPanelTokenDeleteFailureDoesNotSubmit(t *testing.T) {
	submitted := false
	err := submitPanelToken(&telebot.Chat{Type: telebot.ChatPrivate}, func() error {
		return errors.New("delete failed")
	}, func() error {
		submitted = true
		return nil
	})
	if !errors.Is(err, errPanelTokenDelete) {
		t.Fatalf("delete failure error = %v, want token-delete error", err)
	}
	if submitted {
		t.Fatal("token was submitted after Telegram message deletion failed")
	}
}
