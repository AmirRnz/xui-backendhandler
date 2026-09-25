package instancecli

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
)

type fakeActivationState struct {
	frozen              bool
	botActive           bool
	botEnabled          bool
	leaseHeld           bool
	validations         int
	startErr            error
	healthErr           error
	releaseErr          error
	unfreezeErr         error
	unfreezeAfterCommit bool
	unfreezeUnknown     bool
	events              []string
	refreezeSeen        int
	warnings            []string
}

type fakeActivationLease struct{ state *fakeActivationState }

func (l *fakeActivationLease) ValidateRestoredAdmin(context.Context, string, string, int64) error {
	l.state.validations++
	l.state.events = append(l.state.events, "validate-"+strconv.Itoa(l.state.validations))
	if !l.state.leaseHeld || !l.state.frozen {
		return errors.New("identity validation ran outside the frozen transfer lease")
	}
	return nil
}

func (l *fakeActivationLease) Release(context.Context) error {
	l.state.events = append(l.state.events, "release")
	l.state.leaseHeld = false
	return l.state.releaseErr
}

func newFakeActivationHooks(state *fakeActivationState) restoredActivationHooks {
	return restoredActivationHooks{
		AcquireTransferLease: func(context.Context, string) (restoredActivationLease, error) {
			state.events = append(state.events, "acquire")
			state.leaseHeld = true
			return &fakeActivationLease{state: state}, nil
		},
		Refreeze: func(context.Context, string, string) error {
			state.events = append(state.events, "refreeze")
			state.refreezeSeen++
			state.frozen = true
			return nil
		},
		Unfreeze: func(context.Context, string, string) (restoredUnfreezeState, error) {
			state.events = append(state.events, "unfreeze")
			if state.leaseHeld || !state.frozen || state.validations != 2 || !state.botActive {
				return restoredUnfreezeUnknown, errors.New("unfreeze happened before the transfer lease was released")
			}
			if state.unfreezeUnknown {
				return restoredUnfreezeUnknown, state.unfreezeErr
			}
			if state.unfreezeErr != nil {
				if state.unfreezeAfterCommit {
					state.frozen = false
					return restoredUnfreezeUnfrozen, state.unfreezeErr
				}
				return restoredUnfreezeFrozen, state.unfreezeErr
			}
			state.frozen = false
			return restoredUnfreezeUnfrozen, nil
		},
		ReleaseSafe: func(context.Context, string) error {
			state.events = append(state.events, "release-safe")
			if !state.leaseHeld || !state.frozen {
				return errors.New("safe work was released after the deployment was unfrozen")
			}
			return nil
		},
		ValidateHealth: func(context.Context, string) error {
			state.events = append(state.events, "health")
			if !state.leaseHeld || !state.frozen || !state.botActive {
				return errors.New("health was checked after activation was released")
			}
			return state.healthErr
		},
		StartBot: func(string) error {
			state.events = append(state.events, "start")
			if !state.leaseHeld || !state.frozen {
				return errors.New("bot started after the deployment was unfrozen")
			}
			state.botEnabled = true
			state.botActive = true
			return state.startErr
		},
		StopBot: func(string) {
			state.events = append(state.events, "stop-disable")
			state.botActive = false
			state.botEnabled = false
		},
		BotActive: func(string) bool {
			state.events = append(state.events, "active")
			return state.botActive
		},
		ReportWarning: func(message string) {
			state.warnings = append(state.warnings, message)
		},
	}
}

func restoredActivationInputForTest() restoredActivationInput {
	return restoredActivationInput{
		DeploymentID: "retail-activation-test",
		Fingerprint:  "archive-fingerprint",
		Slug:         "retail-copy",
		BackendToken: "restored-backend-token",
		AdminID:      99124,
		BackendURL:   "http://127.0.0.1:8088",
	}
}

func eventIndex(events []string, want string) int {
	for i, event := range events {
		if event == want {
			return i
		}
	}
	return -1
}

func TestActivateRestoredInstanceKeepsFreezeUntilAllChecksPass(t *testing.T) {
	state := &fakeActivationState{frozen: true}
	if err := activateRestoredInstance(context.Background(), restoredActivationInputForTest(), newFakeActivationHooks(state)); err != nil {
		t.Fatal(err)
	}
	if state.frozen || !state.botActive || !state.botEnabled || state.leaseHeld {
		t.Fatalf("successful activation ended in the wrong state: %+v", state)
	}
	if state.validations != 2 {
		t.Fatalf("validation count=%d, want pre-start and post-start validation", state.validations)
	}
	if state.refreezeSeen != 0 {
		t.Fatalf("successful activation unexpectedly refroze deployment: %d", state.refreezeSeen)
	}
	for _, event := range []string{"validate-1", "start", "validate-2", "health", "release-safe", "unfreeze", "release"} {
		if eventIndex(state.events, event) < 0 {
			t.Fatalf("activation event %q missing: %v", event, state.events)
		}
	}
	if eventIndex(state.events, "unfreeze") < eventIndex(state.events, "health") {
		t.Fatalf("deployment unfroze before health check: %v", state.events)
	}
	if eventIndex(state.events, "unfreeze") < eventIndex(state.events, "validate-2") {
		t.Fatalf("deployment unfroze before post-start identity validation: %v", state.events)
	}
	if eventIndex(state.events, "unfreeze") < eventIndex(state.events, "release") {
		t.Fatalf("deployment unfroze before the transfer lease was released: %v", state.events)
	}
}

func TestActivateRestoredInstanceStartFailureStopsBotAndKeepsFreeze(t *testing.T) {
	state := &fakeActivationState{frozen: true, startErr: errors.New("systemd start failed")}
	err := activateRestoredInstance(context.Background(), restoredActivationInputForTest(), newFakeActivationHooks(state))
	if err == nil || !strings.Contains(err.Error(), "bot service did not start") {
		t.Fatalf("start failure was not reported: %v", err)
	}
	if !state.frozen || state.botActive || state.botEnabled || state.leaseHeld {
		t.Fatalf("start failure did not stop and freeze the deployment: %+v", state)
	}
	if state.refreezeSeen != 1 || eventIndex(state.events, "unfreeze") >= 0 || eventIndex(state.events, "release-safe") >= 0 {
		t.Fatalf("start failure passed an activation boundary: %v", state.events)
	}
}

func TestActivateRestoredInstanceHealthFailureStopsBotAndKeepsFreeze(t *testing.T) {
	state := &fakeActivationState{frozen: true, healthErr: errors.New("health failed")}
	err := activateRestoredInstance(context.Background(), restoredActivationInputForTest(), newFakeActivationHooks(state))
	if err == nil || !strings.Contains(err.Error(), "backend health check failed") {
		t.Fatalf("health failure was not reported: %v", err)
	}
	if !state.frozen || state.botActive || state.botEnabled || state.leaseHeld {
		t.Fatalf("health failure did not stop and freeze the deployment: %+v", state)
	}
	if state.validations != 2 || state.refreezeSeen != 1 || eventIndex(state.events, "unfreeze") >= 0 || eventIndex(state.events, "release-safe") >= 0 {
		t.Fatalf("health failure passed an activation boundary: %v", state.events)
	}
}

func TestActivateRestoredInstanceLeaseReleaseFailureFailsClosed(t *testing.T) {
	state := &fakeActivationState{frozen: true, releaseErr: errors.New("lease release failed after lock relinquish")}
	err := activateRestoredInstance(context.Background(), restoredActivationInputForTest(), newFakeActivationHooks(state))
	if err == nil || !strings.Contains(err.Error(), "activation transfer lock could not be released") {
		t.Fatalf("lease release failure was not reported: %v", err)
	}
	if !state.frozen || state.botActive || state.botEnabled || state.leaseHeld {
		t.Fatalf("lease release failure did not fail closed: %+v", state)
	}
	if eventIndex(state.events, "unfreeze") >= 0 || eventIndex(state.events, "release") < 0 || state.refreezeSeen != 1 {
		t.Fatalf("lease release failure allowed activation to continue: %v", state.events)
	}
}

func TestActivateRestoredInstanceUnfreezeFailureKeepsDeploymentFrozen(t *testing.T) {
	state := &fakeActivationState{frozen: true, unfreezeErr: errors.New("freeze update failed")}
	err := activateRestoredInstance(context.Background(), restoredActivationInputForTest(), newFakeActivationHooks(state))
	if err == nil || !strings.Contains(err.Error(), "could not enable backend authentication") {
		t.Fatalf("unfreeze failure was not reported: %v", err)
	}
	if !state.frozen || state.botActive || state.botEnabled || state.leaseHeld {
		t.Fatalf("unfreeze failure did not leave the deployment frozen and stopped: %+v", state)
	}
	if eventIndex(state.events, "release") < 0 || state.refreezeSeen != 1 {
		t.Fatalf("unfreeze failure did not clean up after lease release: %v", state.events)
	}
}

func TestActivateRestoredInstanceVerifiedUnfreezeAfterErrorIsSuccessful(t *testing.T) {
	state := &fakeActivationState{
		frozen:              true,
		unfreezeErr:         errors.New("database reported an ambiguous commit result"),
		unfreezeAfterCommit: true,
	}
	if err := activateRestoredInstance(context.Background(), restoredActivationInputForTest(), newFakeActivationHooks(state)); err != nil {
		t.Fatalf("verified persisted activation should succeed: %v", err)
	}
	if state.frozen || !state.botActive || !state.botEnabled || state.leaseHeld || state.refreezeSeen != 0 {
		t.Fatalf("verified persisted activation was rolled back: %+v", state)
	}
	if eventIndex(state.events, "stop-disable") >= 0 {
		t.Fatalf("verified persisted activation stopped the active bot: %v", state.events)
	}
	if len(state.warnings) != 1 || !strings.Contains(state.warnings[0], "activation may already be visible") {
		t.Fatalf("ambiguous visible activation was not reported: %v", state.warnings)
	}
}

func TestActivateRestoredInstanceUnknownUnfreezeOutcomeLeavesBotForInspection(t *testing.T) {
	state := &fakeActivationState{
		frozen:          true,
		unfreezeErr:     errors.New("database write and readback both failed"),
		unfreezeUnknown: true,
	}
	err := activateRestoredInstance(context.Background(), restoredActivationInputForTest(), newFakeActivationHooks(state))
	if err == nil || !strings.Contains(err.Error(), "activation outcome is uncertain") {
		t.Fatalf("unknown unfreeze outcome was not reported: %v", err)
	}
	if state.botActive == false || state.botEnabled == false || state.leaseHeld || state.refreezeSeen != 0 {
		t.Fatalf("unknown unfreeze outcome triggered rollback: %+v", state)
	}
	if eventIndex(state.events, "stop-disable") >= 0 {
		t.Fatalf("unknown unfreeze outcome stopped the bot: %v", state.events)
	}
	if len(state.warnings) != 1 || !strings.Contains(state.warnings[0], "activation outcome is uncertain") {
		t.Fatalf("unknown unfreeze outcome was not handed to the operator: %v", state.warnings)
	}
}
