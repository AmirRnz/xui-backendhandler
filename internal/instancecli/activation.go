package instancecli

import (
	"context"
	"errors"
	"fmt"
	"time"
)

type restoredActivationLease interface {
	ValidateRestoredAdmin(context.Context, string, string, int64) error
	Release(context.Context) error
}

type restoredUnfreezeState uint8

const (
	restoredUnfreezeUnknown restoredUnfreezeState = iota
	restoredUnfreezeFrozen
	restoredUnfreezeUnfrozen
)

type restoredActivationInput struct {
	DeploymentID string
	Fingerprint  string
	Slug         string
	BackendToken string
	AdminID      int64
	BackendURL   string
}

type restoredActivationHooks struct {
	AcquireTransferLease func(context.Context, string) (restoredActivationLease, error)
	Refreeze             func(context.Context, string, string) error
	Unfreeze             func(context.Context, string, string) (restoredUnfreezeState, error)
	ReleaseSafe          func(context.Context, string) error
	ValidateHealth       func(context.Context, string) error
	StartBot             func(string) error
	StopBot              func(string)
	BotActive            func(string) bool
	ReportWarning        func(string)
}

// activateRestoredInstance is the ordered, side-effecting part of an
// instance restore. The deployment stays transfer_frozen until every
// activation check has succeeded. The hooks keep systemd and PostgreSQL
// boundaries injectable so this ordering is tested without a live host.
func activateRestoredInstance(ctx context.Context, input restoredActivationInput, hooks restoredActivationHooks) error {
	if input.DeploymentID == "" || input.Fingerprint == "" || input.Slug == "" || input.BackendToken == "" || input.AdminID <= 0 || input.BackendURL == "" {
		return errors.New("restored activation identity is incomplete")
	}
	if hooks.AcquireTransferLease == nil || hooks.Refreeze == nil || hooks.Unfreeze == nil || hooks.ReleaseSafe == nil || hooks.ValidateHealth == nil || hooks.StartBot == nil || hooks.StopBot == nil || hooks.BotActive == nil {
		return errors.New("restored activation hooks are incomplete")
	}
	transferCtx, transferCancel := context.WithTimeout(ctx, 5*time.Minute)
	lease, err := hooks.AcquireTransferLease(transferCtx, input.DeploymentID)
	transferCancel()
	if err != nil {
		return fmt.Errorf("database imported but frozen; could not establish activation transfer lock: %w", err)
	}
	leaseReleased := false
	defer func() {
		if !leaseReleased {
			releaseCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_ = lease.Release(releaseCtx)
			cancel()
		}
	}()
	releaseLease := func() error {
		if leaseReleased {
			return nil
		}
		releaseCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		err := lease.Release(releaseCtx)
		leaseReleased = true
		return err
	}
	cleanupFreeze := func() error {
		freezeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return hooks.Refreeze(freezeCtx, input.DeploymentID, input.Fingerprint)
	}
	failFrozen := func(cause error) error {
		hooks.StopBot(input.Slug)
		if freezeErr := cleanupFreeze(); freezeErr != nil {
			return fmt.Errorf("%v and freeze is uncertain; stop target service and verify transfer_frozen for deployment %s: %w", cause, input.DeploymentID, freezeErr)
		}
		return cause
	}

	if err = lease.ValidateRestoredAdmin(ctx, input.DeploymentID, input.BackendToken, input.AdminID); err != nil {
		return failFrozen(errors.New("database imported but frozen; restored scoped backend credential or administrator identity failed validation"))
	}
	if err = hooks.StartBot(input.Slug); err != nil {
		return failFrozen(fmt.Errorf("database imported but frozen; bot service did not start: %w", err))
	}
	if !hooks.BotActive(input.Slug) {
		return failFrozen(errors.New("database imported but frozen; bot service did not remain active"))
	}
	if err = lease.ValidateRestoredAdmin(ctx, input.DeploymentID, input.BackendToken, input.AdminID); err != nil {
		return failFrozen(errors.New("database imported but frozen; post-start scoped backend credential or administrator identity failed validation"))
	}
	if err = hooks.ValidateHealth(ctx, input.BackendURL); err != nil {
		return failFrozen(fmt.Errorf("database imported but frozen; post-start backend health check failed: %w", err))
	}
	if err = hooks.ReleaseSafe(ctx, input.DeploymentID); err != nil {
		return failFrozen(fmt.Errorf("database imported but frozen; safe pending work release failed: %w", err))
	}
	if err = releaseLease(); err != nil {
		return failFrozen(fmt.Errorf("database imported but frozen; activation transfer lock could not be released: %w", err))
	}
	finalState, err := hooks.Unfreeze(ctx, input.DeploymentID, input.Fingerprint)
	switch finalState {
	case restoredUnfreezeUnfrozen:
		if err != nil {
			if hooks.ReportWarning != nil {
				hooks.ReportWarning(fmt.Sprintf("final transfer-freeze update reported an error after persisted state became active; activation may already be visible: %v", err))
			}
		}
		return nil
	case restoredUnfreezeFrozen:
		if err != nil {
			return failFrozen(fmt.Errorf("database imported but frozen; could not enable backend authentication: %w", err))
		}
		return failFrozen(errors.New("database imported but frozen; final activation state remained frozen"))
	default:
		message := "instance activation outcome is uncertain; leave the target bot running and inspect transfer_frozen before retrying"
		if err != nil {
			message = fmt.Sprintf("%s: %v", message, err)
		}
		if hooks.ReportWarning != nil {
			hooks.ReportWarning(message)
		}
		return errors.New(message)
	}
}
