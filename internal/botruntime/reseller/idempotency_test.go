package reseller

import "testing"

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
