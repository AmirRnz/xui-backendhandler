package notify

import (
	"strings"
	"testing"
)

func TestResellerAccessRequestedMessage(t *testing.T) {
	message := formatMessage("reseller.access_requested", map[string]any{"telegram_id": float64(12345), "request_id": float64(9)})
	for _, expected := range []string{"12345", "9", "درخواست‌های reseller"} {
		if !strings.Contains(message, expected) {
			t.Fatalf("notification message %q missing %q", message, expected)
		}
	}
}

func TestResellerAccessDecisionMessagesGuideApplicant(t *testing.T) {
	for _, tc := range []struct {
		topic string
		want  string
	}{
		{"reseller.access_approved", "تأیید شد"},
		{"reseller.access_rejected", "رد شد"},
	} {
		message := formatMessage(tc.topic, map[string]any{})
		if !strings.Contains(message, tc.want) || !strings.Contains(message, "/start") {
			t.Errorf("%s message lacks decision or menu guidance: %q", tc.topic, message)
		}
	}
}
