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
	"example.com/xui-commerce/backend/internal/secrets"
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
		if err = d.dispatchOne(ctx, item); err != nil {
			return err
		}
	}
	return nil
}

func (d *Dispatcher) dispatchOne(ctx context.Context, item map[string]any) error {
	id := item["id"].(int64)
	deployment := item["deployment_id"].(string)
	leaseCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	lease, err := d.Store.AcquireDeploymentRequestLease(leaseCtx, deployment)
	cancel()
	if err != nil {
		return err
	}
	defer func() {
		releaseCtx, releaseCancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer releaseCancel()
		if releaseErr := lease.Release(releaseCtx); releaseErr != nil && d.Logger != nil {
			d.Logger.Error("deployment notification lease release failed", "outbox_id", id, "error", releaseErr)
		}
	}()
	active, err := lease.IsEnabled(ctx, false, deployment)
	if err != nil || !active {
		return err
	}
	chatID := item["chat_id"].(int64)
	text := formatMessage(item["topic"].(string), item["payload"])
	token := d.Config.TelegramTokens[deployment]
	if token == "" && len(d.Config.PanelSecretsKey) == 32 {
		if encrypted, secretErr := d.Store.EncryptedTelegramToken(ctx, deployment); secretErr == nil && len(encrypted) > 0 {
			if plain, openErr := secrets.Open(d.Config.PanelSecretsKey, encrypted); openErr == nil {
				token = string(plain)
			}
		}
	}
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
	case "subscription.updated":
		switch m["action"] {
		case "extend":
			return fmt.Sprintf("تمدید اشتراک %s با پنل همگام شد.", name)
		case "upgrade_ip":
			return fmt.Sprintf("سقف دستگاه‌های اشتراک %s به %v تغییر کرد.", name, m["ip_limit"])
		default:
			return fmt.Sprintf("تغییرات اشتراک %s با پنل همگام شد.", name)
		}
	case "refund.approved":
		return fmt.Sprintf("بازپرداخت به کیف پول شما اضافه شد: %v تومان.", m["amount_toman"])
	case "payment.rejected":
		return fmt.Sprintf("رسید پرداخت مستقیم شما به مبلغ %v تومان رد شد. لطفاً رسید را بررسی کرده و در صورت نیاز دوباره ارسال کنید.", m["amount_toman"])
	case "topup.rejected":
		return "درخواست افزایش موجودی کیف پول شما رد شد. لطفاً رسید را بررسی کرده و در صورت نیاز درخواست جدید ثبت کنید."
	case "refund.rejected":
		return "درخواست استرداد وجه شما توسط مدیریت رد شد."
	case "reseller.access_requested":
		telegramID, _ := m["telegram_id"].(float64)
		requestID, _ := m["request_id"].(float64)
		return fmt.Sprintf("درخواست دسترسی نمایندگی جدید:\nآیدی تلگرام: %.0f\nشناسه درخواست: %.0f\nبرای بررسی، منوی درخواست‌های reseller را باز کنید.", telegramID, requestID)
	case "reseller.access_approved":
		return "درخواست دسترسی نمایندگی شما تأیید شد. برای باز کردن یا تازه‌سازی منوی نمایندگی، /start را ارسال کنید."
	case "reseller.access_rejected":
		return "درخواست دسترسی نمایندگی شما رد شد. برای دیدن گزینه‌های موجود یا پیگیری، /start را ارسال کنید و با پشتیبانی تماس بگیرید."
	default:
		if status != "" {
			return fmt.Sprintf("وضعیت درخواست %s: %s", name, status)
		}
		return "درخواست شما به‌روزرسانی شد."
	}
}
