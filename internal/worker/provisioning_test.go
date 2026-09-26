package worker

import (
	"encoding/json"
	"testing"

	"example.com/xui-commerce/backend/internal/store"
	"example.com/xui-commerce/backend/internal/xui"
)

func TestMatchesProvisionFieldsAcceptsCurrentAndLegacyPanelState(t *testing.T) {
	w := &store.WorkItem{}
	w.Desired.Email = "customer_123@example"
	w.Desired.ClientUUID = "0f15c4ab-394d-4c84-a01b-e3d66b7d2a11"
	w.Desired.SubID = "sub123"
	w.Desired.ExpiryTimeMS = -3600000
	w.Desired.IPLimit = 3
	w.Desired.TrafficLimitBytes = 1024
	w.Desired.PlanName = "monthly"
	w.Desired.TelegramID = 456

	base := &xui.RemoteClient{
		Email: w.Desired.Email, UUID: w.Desired.ClientUUID, SubID: w.Desired.SubID,
		Enable: true, ExpiryTime: w.Desired.ExpiryTimeMS, TotalGB: w.Desired.TrafficLimitBytes,
	}

	current := *base
	current.LimitIP = 0
	current.Comment = xui.CustomerClientComment(w.Desired.PlanName, w.Desired.TelegramID, w.Desired.IPLimit)
	if !matchesProvisionFields(w, &current) {
		t.Fatal("current comment-based device limit state was rejected")
	}

	legacy := *base
	legacy.LimitIP = w.Desired.IPLimit
	legacy.Comment = w.Desired.PlanName
	if !matchesProvisionFields(w, &legacy) {
		t.Fatal("pre-change panel state for an existing work item was rejected")
	}

	legacy.LimitIP++
	if matchesProvisionFields(w, &legacy) {
		t.Fatal("panel state with an unexpected device limit was accepted")
	}
}

func TestPreservedClientFingerprintIgnoresMutationFieldsAndCanonicalizesSecrets(t *testing.T) {
	first := &xui.RemoteClient{
		UUID: "12345678-1234-4234-a234-123456789abc", Email: "owned@example.invalid", SubID: "sub-1",
		Enable: true, ExpiryTime: 100, LimitIP: 0, LimitHWID: 1, LegacyTotal: 4096,
		Flow: "xtls-rprx-vision", Group: "gold", Comment: "devices: 2", TgID: 42,
		Extra: map[string]json.RawMessage{
			"password":  json.RawMessage(`"panel-secret"`),
			"addresses": json.RawMessage(`{"b":2,"a":1}`),
		},
	}
	second := *first
	second.ExpiryTime = 999
	second.LimitIP = 6
	second.Comment = "devices: 5"
	second.TotalGB = 4096
	second.LegacyTotal = 0
	second.Extra = map[string]json.RawMessage{
		"password":  json.RawMessage(`"panel-secret"`),
		"addresses": json.RawMessage(`{"a":1,"b":2}`),
	}
	got, err := preservedClientFingerprint(first)
	if err != nil {
		t.Fatal(err)
	}
	gotSecond, err := preservedClientFingerprint(&second)
	if err != nil {
		t.Fatal(err)
	}
	if got != gotSecond {
		t.Fatal("mutation-owned fields or JSON map ordering changed the preserved-field fingerprint")
	}
	second.Extra["password"] = json.RawMessage(`"changed-secret"`)
	changed, err := preservedClientFingerprint(&second)
	if err != nil {
		t.Fatal(err)
	}
	if changed == got {
		t.Fatal("a panel-owned secret change was not detected")
	}
}
