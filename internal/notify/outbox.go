package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"example.com/xui-commerce/backend/internal/config"
	"example.com/xui-commerce/backend/internal/store"
)

type Dispatcher struct {
	Store  *store.Store
	Config config.Config
	HTTP   *http.Client
	Logger *slog.Logger
}

func (d *Dispatcher) Run(ctx context.Context) {
	ticker := time.NewTicker(d.Config.WorkerInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := d.DispatchOnce(ctx); err != nil && d.Logger != nil {
				d.Logger.Error("notification dispatcher iteration failed", "error", err)
			}
		}
	}
}
func (d *Dispatcher) DispatchOnce(ctx context.Context) error {
	items, err := d.Store.OutboxPending(ctx, 50)
	if err != nil {
		return err
	}
	for _, item := range items {
		id := item["id"].(int64)
		deployment := item["deployment_id"].(string)
		chatID := item["chat_id"].(int64)
		text := formatMessage(item["topic"].(string), item["payload"])
		token := d.Config.TelegramTokens[deployment]
		sent := false
		if token != "" && chatID > 0 {
			sent = d.send(ctx, token, chatID, text)
		}
		if err = d.Store.MarkOutbox(ctx, id, sent); err != nil {
			return err
		}
		if !sent && d.Logger != nil {
			d.Logger.Warn("durable notification delivery failed; it will retry", "outbox_id", id, "deployment_id", deployment)
		}
	}
	return nil
}
func (d *Dispatcher) send(ctx context.Context, token string, chatID int64, text string) bool {
	httpClient := d.HTTP
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 8 * time.Second}
	}
	body, _ := json.Marshal(map[string]any{"chat_id": chatID, "text": text, "disable_web_page_preview": true})
	// Telegram's Bot API places the token in the URL path. Never log this URL or the request error.
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.telegram.org/bot"+token+"/sendMessage", bytes.NewReader(body))
	if err != nil {
		return false
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return false
	}
	var result struct {
		OK bool `json:"ok"`
	}
	if json.NewDecoder(resp.Body).Decode(&result) != nil {
		return false
	}
	return result.OK
}
func formatMessage(topic string, payload any) string {
	m, ok := payload.(map[string]any)
	if !ok {
		return "درخواست شما به‌روزرسانی شد."
	}
	name, _ := m["email"].(string)
	status, _ := m["status"].(string)
	switch topic {
	case "subscription.ready":
		links, _ := m["links"].([]any)
		out := fmt.Sprintf("اشتراک شما فعال شد: %s", name)
		for _, link := range links {
			if s, ok := link.(string); ok && strings.HasPrefix(s, "https://") || ok && strings.HasPrefix(s, "http://") {
				out += "\n" + s
			}
		}
		return out
	case "subscription.cancelled":
		return fmt.Sprintf("اشتراک %s لغو شد.", name)
	case "refund.approved":
		return fmt.Sprintf("بازپرداخت به کیف پول شما اضافه شد: %v تومان.", m["amount_toman"])
	case "reseller.access_requested":
		telegramID, _ := m["telegram_id"].(float64)
		requestID, _ := m["request_id"].(float64)
		return fmt.Sprintf("درخواست دسترسی نمایندگی جدید:\nآیدی تلگرام: %.0f\nشناسه درخواست: %.0f\nبرای بررسی، منوی درخواست‌های reseller را باز کنید.", telegramID, requestID)
	default:
		if status != "" {
			return fmt.Sprintf("وضعیت درخواست %s: %s", name, status)
		}
		return "درخواست شما به‌روزرسانی شد."
	}
}
