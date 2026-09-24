package reseller

import (
	"strings"
	"testing"

	"gopkg.in/telebot.v3"
)

func TestApprovedMainMenuMatchesResellerGridAndFeatureGates(t *testing.T) {
	rows := mainMenuRows(runtimeConfig{}, actor{Role: "reseller", ApprovalStatus: "approved"})
	if len(rows) != 3 {
		t.Fatalf("approved menu has %d rows, want 3: %#v", len(rows), rows)
	}
	assertButton(t, rows[0], 0, "🧪 تست رایگان", "plans|test")
	assertButton(t, rows[0], 1, "💼 خرید سرویس", "plans|paid")
	assertButton(t, rows[1], 0, "📋 سرویس‌های من", "services")
	assertButton(t, rows[1], 1, "👛 کیف پول", "wallet")
	assertButton(t, rows[2], 0, "🆘 پشتیبانی", "support")

	limited := mainMenuRows(runtimeConfig{Features: map[string]bool{
		"trials_enabled": false, "purchases_enabled": false, "wallet_enabled": false,
	}}, actor{Role: "reseller", ApprovalStatus: "approved"})
	if len(limited) != 2 {
		t.Fatalf("feature-gated approved menu has %d rows, want services and support: %#v", len(limited), limited)
	}
	assertButton(t, limited[0], 0, "📋 سرویس‌های من", "services")
	assertButton(t, limited[1], 0, "🆘 پشتیبانی", "support")
}

func TestPendingMainMenuHidesCommerceAndKeepsTrialAccessRequestAndSupport(t *testing.T) {
	rows := mainMenuRows(runtimeConfig{}, actor{Role: "reseller", ApprovalStatus: "pending"})
	if len(rows) != 3 {
		t.Fatalf("pending menu has %d rows, want 3: %#v", len(rows), rows)
	}
	assertButton(t, rows[0], 0, "🧪 دریافت تست", "plans|test")
	assertButton(t, rows[1], 0, "📝 درخواست دسترسی نمایندگی", "request-access")
	assertButton(t, rows[2], 0, "🆘 پشتیبانی", "support")

	noTrials := mainMenuRows(runtimeConfig{Features: map[string]bool{"trials_enabled": false}}, actor{Role: "reseller", ApprovalStatus: "pending"})
	if len(noTrials) != 2 {
		t.Fatalf("pending menu with trials disabled has %d rows, want request and support: %#v", len(noTrials), noTrials)
	}
	assertButton(t, noTrials[0], 0, "📝 درخواست دسترسی نمایندگی", "request-access")
	assertButton(t, noTrials[1], 0, "🆘 پشتیبانی", "support")

	status := accessRequestMessage(actor{ApprovalStatus: "pending"})
	if !strings.Contains(status, "در انتظار تایید مدیر") || strings.Contains(status, "ارسال شد") {
		t.Fatalf("pending access message must report current status without claiming a request was sent: %q", status)
	}
}

func TestRejectedResellerCannotSubmitAccessRequestAgain(t *testing.T) {
	rows := mainMenuRows(runtimeConfig{}, actor{Role: "reseller", ApprovalStatus: "rejected"})
	if len(rows) != 2 {
		t.Fatalf("rejected menu has %d rows, want trial and support only: %#v", len(rows), rows)
	}
	assertButton(t, rows[0], 0, "🧪 دریافت تست", "plans|test")
	assertButton(t, rows[1], 0, "🆘 پشتیبانی", "support")
	if got := accessRequestMessage(actor{ApprovalStatus: "rejected"}); got != "درخواست دسترسی شما تایید نشده است. برای پیگیری با پشتیبانی ارتباط بگیرید." {
		t.Fatalf("rejected access message = %q", got)
	}
}

func TestWalletMenuRespectsTopupFeature(t *testing.T) {
	rows := walletMenuRows(runtimeConfig{})
	if len(rows) != 2 {
		t.Fatalf("wallet menu has %d rows, want actions and home: %#v", len(rows), rows)
	}
	assertButton(t, rows[0], 0, "گردش کیف پول", "ledger")
	assertButton(t, rows[0], 1, "شارژ کیف پول", "topup")
	assertButton(t, rows[1], 0, "خانه", "home")

	withoutTopup := walletMenuRows(runtimeConfig{Features: map[string]bool{"topups_enabled": false}})
	if len(withoutTopup) != 2 || len(withoutTopup[0]) != 1 {
		t.Fatalf("wallet menu with topups disabled exposes unexpected actions: %#v", withoutTopup)
	}
	assertButton(t, withoutTopup[0], 0, "گردش کیف پول", "ledger")
}

func TestAccessRequestResponsesRenderDurableSubmissionAndCooldown(t *testing.T) {
	message, err := accessRequestResultMessage(accessRequestResponse{Status: "submitted"})
	if err != nil || message != "✅ درخواست دسترسی شما ثبت شد و برای بررسی مدیر ارسال خواهد شد." {
		t.Fatalf("submitted response = %q, err=%v", message, err)
	}

	message, err = accessRequestResultMessage(accessRequestResponse{
		Status:        "already_pending",
		NextRequestAt: "2026-09-25T10:30:00Z",
	})
	if err != nil || !strings.Contains(message, "در انتظار بررسی مدیر") || !strings.Contains(message, "2026-09-25 10:30 UTC") {
		t.Fatalf("already pending response = %q, err=%v", message, err)
	}

	if _, err = accessRequestResultMessage(accessRequestResponse{Status: "unexpected"}); err == nil {
		t.Fatal("unknown backend status should be reported")
	}
}

func TestSupportMessageUsesConfiguredUsername(t *testing.T) {
	for _, tc := range []struct {
		name string
		text map[string]string
		want string
	}{
		{name: "missing setting", want: "برای پشتیبانی لطفا با ادمین در ارتباط باشید."},
		{name: "bare username", text: map[string]string{"support_username": "helpdesk"}, want: "برای پشتیبانی لطفا با آی‌دی زیر در ارتباط باشید:\n@helpdesk"},
		{name: "already prefixed", text: map[string]string{"support_username": " @helpdesk "}, want: "برای پشتیبانی لطفا با آی‌دی زیر در ارتباط باشید:\n@helpdesk"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := supportMessage(runtimeConfig{Text: tc.text}); got != tc.want {
				t.Fatalf("supportMessage() = %q, want %q", got, tc.want)
			}
		})
	}
}

func assertButton(t *testing.T, row []telebot.Btn, index int, text, data string) {
	t.Helper()
	if len(row) <= index {
		t.Fatalf("row has %d buttons, missing index %d", len(row), index)
	}
	if got := row[index]; got.Text != text || got.Data != data {
		t.Fatalf("button[%d] = (%q, %q), want (%q, %q)", index, got.Text, got.Data, text, data)
	}
}
