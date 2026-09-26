package worker

import (
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
