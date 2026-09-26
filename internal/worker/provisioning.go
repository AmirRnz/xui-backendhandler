package worker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"time"

	"example.com/xui-commerce/backend/internal/config"
	"example.com/xui-commerce/backend/internal/secrets"
	"example.com/xui-commerce/backend/internal/store"
	"example.com/xui-commerce/backend/internal/xui"
)

type Panel interface {
	CheckWriteReadiness(context.Context) error
	GetClient(context.Context, string) (*xui.RemoteClient, error)
	Add(context.Context, xui.ClientConfig, []int) xui.WriteResult
	Attach(context.Context, string, []int) error
	Delete(context.Context, string) xui.WriteResult
	SubscriptionLinks(context.Context, string) ([]string, error)
}

type Runner struct {
	Store        *store.Store
	Config       config.Config
	Logger       *slog.Logger
	PanelFactory func(context.Context, string, string) (Panel, error)
}

func (r *Runner) Run(ctx context.Context) {
	ticker := time.NewTicker(r.Config.WorkerInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := r.ProcessOne(ctx); err != nil && r.Logger != nil {
				r.Logger.Error("provisioning worker iteration failed", "error", err)
			}
		}
	}
}

func (r *Runner) ProcessOne(ctx context.Context) error {
	w, err := r.Store.ClaimWork(ctx)
	if err != nil || w == nil {
		return err
	}
	leaseCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	lease, err := r.Store.AcquireDeploymentRequestLease(leaseCtx, w.DeploymentID)
	cancel()
	if err != nil {
		return err
	}
	defer func() {
		releaseCtx, releaseCancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer releaseCancel()
		if releaseErr := lease.Release(releaseCtx); releaseErr != nil && r.Logger != nil {
			r.Logger.Error("deployment work lease release failed", "work_id", w.ID, "error", releaseErr)
		}
	}()
	active, err := lease.IsEnabled(ctx, false, w.DeploymentID)
	if err != nil || !active {
		return err
	}
	token := r.Config.PanelTokens[w.PanelID]
	if len(r.Config.PanelSecretsKey) == 32 {
		ciphertext, secretErr := r.Store.EncryptedPanelToken(ctx, w.PanelID)
		if secretErr == nil && len(ciphertext) > 0 {
			plain, openErr := secrets.Open(r.Config.PanelSecretsKey, ciphertext)
			if openErr != nil {
				return r.Store.RetryWork(ctx, w.ID, "panel secret could not be decrypted", w.Phase == "ready")
			}
			token = string(plain)
		}
	}
	if token == "" {
		return r.Store.RetryWork(ctx, w.ID, "panel API token is not configured", w.Phase == "ready")
	}
	var panel Panel
	if r.PanelFactory != nil {
		panel, err = r.PanelFactory(ctx, w.PanelID, token)
		if err != nil {
			return r.Store.RetryWork(ctx, w.ID, "panel configuration is invalid", w.Phase == "ready")
		}
	} else {
		baseURL, endpointErr := r.Store.PanelEndpoint(ctx, w.PanelID)
		if endpointErr != nil {
			return r.Store.RetryWork(ctx, w.ID, "panel configuration is unavailable", w.Phase == "ready")
		}
		client, clientErr := xui.New(baseURL, token, 12*time.Second)
		if clientErr != nil {
			return r.Store.RetryWork(ctx, w.ID, "panel configuration is invalid", w.Phase == "ready")
		}
		panel = client
	}
	if err = panel.CheckWriteReadiness(ctx); err != nil {
		return r.Store.RetryWork(ctx, w.ID, "panel write readiness check failed: "+err.Error(), w.Phase == "ready")
	}
	if w.Kind == "subscription_delete" {
		return r.processDelete(ctx, panel, w)
	}
	if w.Kind == "subscription_update" {
		return r.processSubscriptionUpdate(ctx, panel, w)
	}
	if w.Kind != "provision_add" {
		return r.Store.ManualReviewWork(ctx, w.ID, "unsupported durable work kind", map[string]any{"kind": w.Kind})
	}
	return r.processProvision(ctx, panel, w)
}

type clientUpdater interface {
	Update(context.Context, string, xui.ClientConfig) xui.WriteResult
}

func (r *Runner) processSubscriptionUpdate(ctx context.Context, p Panel, w *store.WorkItem) error {
	if w.Desired.MutationAction != "extend" && w.Desired.MutationAction != "upgrade_ip" {
		return r.Store.ManualReviewWork(ctx, w.ID, "unsupported subscription update action", map[string]any{"action": w.Desired.MutationAction})
	}
	remote, err := p.GetClient(ctx, w.Desired.Email)
	if errors.Is(err, xui.ErrNotFound) {
		return r.Store.ManualReviewWork(ctx, w.ID, "existing panel client is absent; refusing to recreate it during an update", map[string]any{"email": w.Desired.Email})
	}
	if err != nil {
		if w.Attempts < 5 {
			return r.Store.RetryWork(ctx, w.ID, "panel client read failed before update: "+err.Error(), true)
		}
		return r.Store.ManualReviewWork(ctx, w.ID, "cannot verify panel client before update", map[string]any{"error": err.Error()})
	}
	if !matchesUpdateIdentity(w, remote) {
		return r.Store.ManualReviewWork(ctx, w.ID, "panel client identity does not match the owned subscription", map[string]any{"email": remoteEmail(remote), "uuid": xui.UUIDOf(remote), "sub_id": remoteSub(remote)})
	}
	devices, err := xui.CustomerDeviceLimit(remote.Comment)
	if err != nil || (devices != w.Desired.ExpectedIPLimit && devices != w.Desired.IPLimit) {
		return r.Store.ManualReviewWork(ctx, w.ID, "panel device marker does not match the recorded subscription limit", map[string]any{"email": remote.Email, "expected_devices": w.Desired.ExpectedIPLimit})
	}
	preservedFingerprint, fingerprintErr := preservedClientFingerprint(remote)
	if fingerprintErr != nil {
		return r.Store.ManualReviewWork(ctx, w.ID, "panel client contains fields that cannot be safely fingerprinted", map[string]any{"email": remote.Email})
	}
	if updateStateMatches(w, remote) {
		if w.Phase != "ready" {
			return r.Store.ManualReviewWork(ctx, w.ID, "panel reached the requested update state after a prior attempt; full-row preservation cannot be proven after restart", map[string]any{"email": remote.Email})
		}
		return r.Store.SucceedWork(ctx, w, nil)
	}
	if devices != w.Desired.ExpectedIPLimit {
		return r.Store.ManualReviewWork(ctx, w.ID, "panel device marker reached a partial or conflicting update state", map[string]any{"email": remote.Email, "observed_devices": devices, "expected_devices": w.Desired.ExpectedIPLimit, "desired_devices": w.Desired.IPLimit})
	}
	if remote.ExpiryTime != w.Desired.ExpectedExpiryTimeMS {
		return r.Store.ManualReviewWork(ctx, w.ID, "panel expiry changed outside the requested update", map[string]any{"email": remote.Email, "expected_expiry_time_ms": w.Desired.ExpectedExpiryTimeMS, "observed_expiry_time_ms": remote.ExpiryTime})
	}
	if w.Phase != "ready" {
		return r.Store.ManualReviewWork(ctx, w.ID, "previous full-row update attempt is still at the prior client state; refusing to issue it again", map[string]any{"email": remote.Email, "phase": w.Phase})
	}
	if w.Desired.MutationAction == "upgrade_ip" && w.Desired.IPLimit != w.Desired.ExpectedIPLimit {
		updatedComment, commentErr := xui.UpdateCustomerDeviceComment(remote.Comment, w.Desired.IPLimit)
		if commentErr != nil {
			return r.Store.ManualReviewWork(ctx, w.ID, "panel device marker cannot be updated safely", map[string]any{"email": remote.Email})
		}
		remote.Comment = updatedComment
	}
	updater, ok := p.(clientUpdater)
	if !ok {
		return r.Store.ManualReviewWork(ctx, w.ID, "panel adapter does not support full-row client updates", map[string]any{"email": remote.Email})
	}
	id := remote.UUID
	if id == "" && len(remote.ID) > 0 {
		id = strings.Trim(string(remote.ID), `"`)
	}
	config := xui.ClientConfig{
		ID: id, Email: remote.Email, SubID: remote.SubID, Enable: remote.Enable,
		ExpiryTime: w.Desired.ExpiryTimeMS, LimitIP: 0, LimitHWID: remote.LimitHWID,
		TotalGB: remote.TrafficLimit(), Flow: remote.Flow, Group: remote.Group, Comment: remote.Comment,
		TgID: remote.TgID, Extra: remote.Extra,
	}
	if err := r.Store.MarkCreateAttempted(ctx, w.ID); err != nil {
		return err
	}
	write := updater.Update(ctx, remote.Email, config)
	observed, readErr := p.GetClient(ctx, w.Desired.Email)
	if errors.Is(readErr, xui.ErrNotFound) {
		return r.Store.ManualReviewWork(ctx, w.ID, "panel client disappeared during update", map[string]any{"email": w.Desired.Email, "write_outcome": write.Outcome})
	}
	if readErr != nil {
		if w.Attempts < 5 {
			return r.Store.RetryWork(ctx, w.ID, "panel readback failed after update: "+readErr.Error(), false)
		}
		return r.Store.ManualReviewWork(ctx, w.ID, "cannot read back panel client after update", map[string]any{"email": w.Desired.Email, "write_outcome": write.Outcome})
	}
	if !matchesUpdateIdentity(w, observed) {
		return r.Store.ManualReviewWork(ctx, w.ID, "panel client identity changed during update", map[string]any{"email": remoteEmail(observed), "uuid": xui.UUIDOf(observed), "sub_id": remoteSub(observed)})
	}
	if updateStateMatches(w, observed) {
		observedFingerprint, verifyErr := preservedClientFingerprint(observed)
		if verifyErr != nil || observedFingerprint != preservedFingerprint {
			return r.Store.ManualReviewWork(ctx, w.ID, "full-row update changed panel-owned client fields; service requires manual review", map[string]any{"email": observed.Email, "write_outcome": write.Outcome})
		}
		return r.Store.SucceedWork(ctx, w, nil)
	}
	observedDevices, markerErr := xui.CustomerDeviceLimit(observed.Comment)
	if observed.ExpiryTime == w.Desired.ExpectedExpiryTimeMS && markerErr == nil && observedDevices == w.Desired.ExpectedIPLimit {
		observedFingerprint, verifyErr := preservedClientFingerprint(observed)
		if verifyErr != nil || observedFingerprint != preservedFingerprint {
			return r.Store.ManualReviewWork(ctx, w.ID, "panel-owned client fields changed while checking the update result", map[string]any{"email": observed.Email, "write_outcome": write.Outcome})
		}
		if write.Outcome == xui.DefinitiveNoWrite && w.Attempts < 5 {
			return r.Store.RetryWork(ctx, w.ID, "panel update was read back at its prior state; safe to reapply after verification", true)
		}
		return r.Store.ManualReviewWork(ctx, w.ID, "panel update outcome is unresolved at its prior client state; refusing to repeat a possibly partial full-row update", map[string]any{"email": observed.Email, "write_outcome": write.Outcome})
	}
	return r.Store.ManualReviewWork(ctx, w.ID, "panel client has a partial or conflicting update state", map[string]any{"email": observed.Email, "observed_expiry_time_ms": observed.ExpiryTime, "expected_expiry_time_ms": w.Desired.ExpectedExpiryTimeMS})
}

func matchesUpdateIdentity(w *store.WorkItem, remote *xui.RemoteClient) bool {
	return w != nil && remote != nil && remote.Email == w.Desired.Email && xui.UUIDOf(remote) == w.Desired.UUID && remoteSub(remote) == w.Desired.SubID
}

func preservedClientFingerprint(remote *xui.RemoteClient) (string, error) {
	if remote == nil {
		return "", errors.New("panel client is required")
	}
	extra := make(map[string]any, len(remote.Extra))
	for key, raw := range remote.Extra {
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.UseNumber()
		var value any
		if err := decoder.Decode(&value); err != nil {
			return "", err
		}
		extra[key] = value
	}
	fields := struct {
		ID        string         `json:"id"`
		Email     string         `json:"email"`
		SubID     string         `json:"sub_id"`
		Enable    bool           `json:"enable"`
		LimitHWID int            `json:"limit_hwid"`
		TotalGB   int64          `json:"total_gb"`
		Flow      string         `json:"flow"`
		Group     string         `json:"group"`
		TgID      int64          `json:"tg_id"`
		Extra     map[string]any `json:"extra"`
	}{
		ID: xui.UUIDOf(remote), Email: remote.Email, SubID: remote.SubID, Enable: remote.Enable,
		LimitHWID: remote.LimitHWID, TotalGB: remote.TrafficLimit(),
		Flow: remote.Flow, Group: remote.Group, TgID: remote.TgID, Extra: extra,
	}
	data, err := json.Marshal(fields)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func updateStateMatches(w *store.WorkItem, remote *xui.RemoteClient) bool {
	if w == nil || remote == nil || remote.ExpiryTime != w.Desired.ExpiryTimeMS || remote.LimitIP != 0 {
		return false
	}
	devices, err := xui.CustomerDeviceLimit(remote.Comment)
	return err == nil && devices == w.Desired.IPLimit
}

func (r *Runner) processProvision(ctx context.Context, p Panel, w *store.WorkItem) error {
	remote, err := p.GetClient(ctx, w.Desired.Email)
	if errors.Is(err, xui.ErrNotFound) {
		if w.Phase != "ready" {
			if w.Attempts < 5 {
				return r.Store.RetryWork(ctx, w.ID, "prior create was attempted but the client is not visible yet; will verify again", false)
			}
			return r.Store.ManualReviewWork(ctx, w.ID, "client absent after an ambiguous create; refusing duplicate provisioning", map[string]any{"email": w.Desired.Email})
		}
		if err = r.Store.MarkCreateAttempted(ctx, w.ID); err != nil {
			return err
		}
		comment := xui.CustomerClientComment(w.Desired.PlanName, w.Desired.TelegramID, w.Desired.IPLimit)
		result := p.Add(ctx, xui.ClientConfig{ID: w.Desired.ClientUUID, Email: w.Desired.Email, SubID: w.Desired.SubID, Enable: true, ExpiryTime: w.Desired.ExpiryTimeMS, LimitIP: 0, LimitHWID: 0, TotalGB: w.Desired.TrafficLimitBytes, Flow: w.Desired.Flow, Comment: comment, TgID: w.Desired.TelegramID}, w.Desired.InboundIDs)
		if result.Outcome == xui.DefinitiveNoWrite {
			reason := "panel confirmed no write"
			if result.Err != nil {
				reason = result.Err.Error()
			}
			return r.Store.RetryWork(ctx, w.ID, reason, true)
		}
		remote, err = p.GetClient(ctx, w.Desired.Email)
		if err != nil && !errors.Is(err, xui.ErrNotFound) {
			if w.Attempts < 5 {
				return r.Store.RetryWork(ctx, w.ID, "remote readback failed after create attempt: "+err.Error(), false)
			}
			return r.Store.ManualReviewWork(ctx, w.ID, "cannot verify panel after create attempt", map[string]any{"error": err.Error()})
		}
		if errors.Is(err, xui.ErrNotFound) {
			if w.Attempts < 5 {
				return r.Store.RetryWork(ctx, w.ID, "create result is ambiguous and client is not visible yet", false)
			}
			return r.Store.ManualReviewWork(ctx, w.ID, "create result remained ambiguous; client absent and create will not be repeated", map[string]any{"email": w.Desired.Email, "write_outcome": result.Outcome})
		}
	} else if err != nil {
		if w.Attempts < 5 {
			return r.Store.RetryWork(ctx, w.ID, "panel client read failed: "+err.Error(), false)
		}
		return r.Store.ManualReviewWork(ctx, w.ID, "cannot verify panel client identity", map[string]any{"error": err.Error()})
	}
	return r.verifyProvision(ctx, p, w, remote)
}

func (r *Runner) verifyProvision(ctx context.Context, p Panel, w *store.WorkItem, remote *xui.RemoteClient) error {
	if !matchesProvisionFields(w, remote) {
		return r.Store.ManualReviewWork(ctx, w.ID, "panel client does not match the persisted identity and desired state", map[string]any{"email": remoteEmail(remote), "uuid": xui.UUIDOf(remote), "sub_id": remoteSub(remote), "inbounds": remoteInbounds(remote)})
	}
	desired := unique(w.Desired.InboundIDs)
	have := unique(remote.InboundIDs)
	missing := difference(desired, have)
	unexpected := difference(have, desired)
	if len(unexpected) > 0 {
		return r.Store.ManualReviewWork(ctx, w.ID, "panel client has inbound attachments outside the persisted desired set", map[string]any{"inbounds": have, "unexpected": unexpected})
	}
	if len(missing) > 0 {
		if err := p.Attach(ctx, w.Desired.Email, missing); err != nil && r.Logger != nil {
			r.Logger.Warn("panel attach returned an uncertain result", "work_id", w.ID, "error", err)
		}
		verified, err := p.GetClient(ctx, w.Desired.Email)
		if err != nil {
			if w.Attempts < 5 {
				return r.Store.RetryWork(ctx, w.ID, "readback failed after inbound attach", false)
			}
			return r.Store.ManualReviewWork(ctx, w.ID, "cannot resolve partial inbound attach", map[string]any{"missing": missing})
		}
		if !sameCore(w, verified) {
			return r.Store.ManualReviewWork(ctx, w.ID, "panel identity changed during inbound repair", map[string]any{"uuid": xui.UUIDOf(verified), "sub_id": verified.SubID})
		}
		missing = difference(desired, unique(verified.InboundIDs))
		unexpected = difference(unique(verified.InboundIDs), desired)
		if len(unexpected) > 0 || len(missing) > 0 {
			return r.Store.ManualReviewWork(ctx, w.ID, "partial inbound result needs manual review", map[string]any{"missing": missing, "unexpected": unexpected, "inbounds": verified.InboundIDs})
		}
	}
	links, err := p.SubscriptionLinks(ctx, w.Desired.SubID)
	if err != nil {
		if r.Logger != nil {
			r.Logger.Warn("subscription link lookup failed after verified create", "work_id", w.ID, "error", err)
		}
		links = []string{}
	}
	return r.Store.SucceedWork(ctx, w, links)
}

func matchesProvisionFields(w *store.WorkItem, remote *xui.RemoteClient) bool {
	if w == nil || remote == nil || !sameCore(w, remote) || !remote.Enable || remote.ExpiryTime != w.Desired.ExpiryTimeMS || remote.TrafficLimit() != w.Desired.TrafficLimitBytes {
		return false
	}
	modernComment := xui.CustomerClientComment(w.Desired.PlanName, w.Desired.TelegramID, w.Desired.IPLimit)
	if remote.LimitIP == 0 && remote.Comment == modernComment {
		return true
	}
	// Work created before comment-based device limits were introduced can still
	// finish safely when its original panel state matches the old desired values.
	// This keeps an already-attempted provision out of manual review during rollout.
	return remote.LimitIP == w.Desired.IPLimit && remote.Comment == w.Desired.PlanName
}

func sameCore(w *store.WorkItem, remote *xui.RemoteClient) bool {
	return w != nil && remote != nil && remote.Email == w.Desired.Email && xui.UUIDOf(remote) == w.Desired.ClientUUID && remote.SubID == w.Desired.SubID
}

func (r *Runner) processDelete(ctx context.Context, p Panel, w *store.WorkItem) error {
	remote, err := p.GetClient(ctx, w.Desired.Email)
	if errors.Is(err, xui.ErrNotFound) {
		return r.Store.SucceedWork(ctx, w, []string{})
	}
	if err != nil {
		if w.Attempts < 5 {
			return r.Store.RetryWork(ctx, w.ID, "panel client read failed before delete: "+err.Error(), w.Phase == "ready")
		}
		return r.Store.ManualReviewWork(ctx, w.ID, "cannot verify remote identity before delete", map[string]any{"error": err.Error()})
	}
	if xui.UUIDOf(remote) != w.Desired.UUID || remote.SubID != w.Desired.SubID {
		return r.Store.ManualReviewWork(ctx, w.ID, "refusing to delete a panel client with a different UUID or subId", map[string]any{"uuid": xui.UUIDOf(remote), "sub_id": remote.SubID})
	}
	result := p.Delete(ctx, w.Desired.Email)
	if result.Outcome == xui.DefinitiveNoWrite {
		reason := "panel confirmed delete did not write"
		if result.Err != nil {
			reason = result.Err.Error()
		}
		return r.Store.RetryWork(ctx, w.ID, reason, true)
	}
	_, readErr := p.GetClient(ctx, w.Desired.Email)
	if errors.Is(readErr, xui.ErrNotFound) {
		return r.Store.SucceedWork(ctx, w, []string{})
	}
	if readErr != nil {
		if w.Attempts < 5 {
			return r.Store.RetryWork(ctx, w.ID, "delete readback failed: "+readErr.Error(), false)
		}
		return r.Store.ManualReviewWork(ctx, w.ID, "delete outcome needs manual review", map[string]any{"error": readErr.Error()})
	}
	return r.Store.ManualReviewWork(ctx, w.ID, "delete may be partial; remote client remains and will not be removed again automatically", map[string]any{"email": w.Desired.Email, "write_outcome": result.Outcome})
}

func unique(in []int) []int {
	out := []int{}
	for _, v := range in {
		if !slices.Contains(out, v) {
			out = append(out, v)
		}
	}
	return out
}
func difference(a, b []int) []int {
	out := []int{}
	for _, v := range a {
		if !slices.Contains(b, v) {
			out = append(out, v)
		}
	}
	return out
}
func remoteEmail(r *xui.RemoteClient) string {
	if r == nil {
		return ""
	}
	return r.Email
}
func remoteSub(r *xui.RemoteClient) string {
	if r == nil {
		return ""
	}
	return r.SubID
}
func remoteInbounds(r *xui.RemoteClient) []int {
	if r == nil {
		return nil
	}
	return r.InboundIDs
}
