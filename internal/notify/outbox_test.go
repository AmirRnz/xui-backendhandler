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
