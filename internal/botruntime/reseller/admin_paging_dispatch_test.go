package reseller

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"example.com/xui-commerce/backend/internal/botruntime/backend"
	"gopkg.in/telebot.v3"
)

type adminPagingContext struct {
	telebot.Context
	sender *telebot.User
	chat   *telebot.Chat
	data   string
	shown  interface{}
	opts   []interface{}
}

func (c *adminPagingContext) Sender() *telebot.User { return c.sender }
func (c *adminPagingContext) Chat() *telebot.Chat   { return c.chat }
func (c *adminPagingContext) Data() string          { return c.data }
func (c *adminPagingContext) Respond(...*telebot.CallbackResponse) error {
	return nil
}
func (c *adminPagingContext) EditOrSend(what interface{}, opts ...interface{}) error {
	c.shown, c.opts = what, opts
	return nil
}

func TestAdminPagingCallbacksRenderSecondPageAndGateAccess(t *testing.T) {
	var adminListCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.Method + " " + r.URL.Path {
		case "POST /v1/actors/resolve":
			id, _ := strconv.ParseInt(r.Header.Get("X-Actor-Telegram-ID"), 10, 64)
			_ = json.NewEncoder(w).Encode(actor{TelegramID: id, Role: "admin"})
		case "GET /v1/admin/resellers/pending":
			adminListCalls++
			items := make([]map[string]any, adminReviewPageSize+2)
			for i := range items {
				items[i] = map[string]any{"telegram_id": int64(500 + i), "approval_status": "pending"}
			}
			_ = json.NewEncoder(w).Encode(items)
		case "GET /v1/admin/payments", "GET /v1/admin/topups":
			adminListCalls++
			items := make([]map[string]any, adminReviewPageSize+2)
			for i := range items {
				items[i] = map[string]any{"id": int64(700 + i), "amount_toman": int64(1000 + i)}
			}
			_ = json.NewEncoder(w).Encode(items)
		case "GET /v1/features":
			_, _ = w.Write([]byte(`{"features":{}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	api, err := backend.New(server.URL, "test-secret", time.Second)
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name, route, wantID string
	}{
		{name: "resellers", route: "resellerpage|1", wantID: "509"},
		{name: "payments", route: "pendingpage|pending|1", wantID: "709"},
		{name: "topups", route: "pendingpage|pendingtopups|1", wantID: "709"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app := &botApp{api: api, menus: map[int64]string{adminTelegramID: "page-screen"}}
			ctx := &adminPagingContext{
				sender: &telebot.User{ID: adminTelegramID},
				chat:   &telebot.Chat{ID: adminTelegramID, Type: telebot.ChatPrivate},
				data:   "\f" + callbackUnique + "|" + tc.route + "|vpage-screen",
			}
			if err := app.callback(ctx); err != nil {
				t.Fatalf("dispatch callback: %v", err)
			}
			keyboard := testMarkup(t, ctx.opts)
			found := false
			for _, row := range keyboard.InlineKeyboard {
				for _, button := range row {
					if strings.Contains(button.Data, tc.wantID) {
						found = true
					}
					if strings.Contains(button.Data, "507") || strings.Contains(button.Data, "707") {
						t.Errorf("page two unexpectedly includes prior-page record: %q", button.Data)
					}
				}
			}
			if !found {
				t.Fatalf("page two does not expose expected stable record ID %s: %#v", tc.wantID, keyboard.InlineKeyboard)
			}
		})
	}

	before := adminListCalls
	for _, tc := range []struct {
		name, route string
		id          int64
		chatType    telebot.ChatType
	}{
		{name: "group chat", route: "resellerpage|1", id: adminTelegramID, chatType: telebot.ChatGroup},
		{name: "non-admin actor", route: "pendingpage|pending|1", id: 500, chatType: telebot.ChatPrivate},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app := &botApp{api: api, menus: map[int64]string{tc.id: "page-screen"}}
			ctx := &adminPagingContext{
				sender: &telebot.User{ID: tc.id},
				chat:   &telebot.Chat{ID: tc.id, Type: tc.chatType},
				data:   fmt.Sprintf("\f%s|%s|vpage-screen", callbackUnique, tc.route),
			}
			if err := app.callback(ctx); err != nil {
				t.Fatalf("dispatch denied callback: %v", err)
			}
		})
	}
	if adminListCalls != before {
		t.Fatalf("denied page callbacks made %d admin list calls", adminListCalls-before)
	}
}

func testMarkup(t *testing.T, opts []interface{}) *telebot.ReplyMarkup {
	t.Helper()
	for _, opt := range opts {
		if markup, ok := opt.(*telebot.ReplyMarkup); ok {
			return markup
		}
	}
	t.Fatal("callback did not render a keyboard")
	return nil
}
