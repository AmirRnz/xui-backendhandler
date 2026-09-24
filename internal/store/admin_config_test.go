package store

import (
	"errors"
	"testing"
)

func TestResellerApprovalDecisionOnlyTransitionsPendingActors(t *testing.T) {
	for _, status := range []string{"approved", "rejected"} {
		if err := validateResellerDecision("pending", status); err != nil {
			t.Fatalf("pending -> %s should be allowed: %v", status, err)
		}
	}
	for _, current := range []string{"approved", "rejected"} {
		for _, target := range []string{"approved", "rejected"} {
			if err := validateResellerDecision(current, target); !errors.Is(err, ErrConflict) {
				t.Fatalf("%s -> %s should conflict, got %v", current, target, err)
			}
		}
	}
	if err := validateResellerDecision("pending", "pending"); !errors.Is(err, ErrInvalidAdminConfig) {
		t.Fatalf("invalid target status should be rejected, got %v", err)
	}
}
