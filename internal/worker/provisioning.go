package worker

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"time"

	"example.com/xui-commerce/backend/internal/config"
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
	token := r.Config.PanelTokens[w.PanelID]
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
	if w.Kind != "provision_add" {
		return r.Store.ManualReviewWork(ctx, w.ID, "unsupported durable work kind", map[string]any{"kind": w.Kind})
	}
	return r.processProvision(ctx, panel, w)
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
		result := p.Add(ctx, xui.ClientConfig{ID: w.Desired.ClientUUID, Email: w.Desired.Email, SubID: w.Desired.SubID, Enable: true, ExpiryTime: w.Desired.ExpiryTimeMS, LimitIP: w.Desired.IPLimit, LimitHWID: 0, TotalGB: w.Desired.TrafficLimitBytes, Flow: w.Desired.Flow, Comment: w.Desired.PlanName, TgID: w.Desired.TelegramID}, w.Desired.InboundIDs)
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
	if !sameCore(w, remote) || remote.Enable != true || remote.ExpiryTime != w.Desired.ExpiryTimeMS || remote.LimitIP != w.Desired.IPLimit || remote.TrafficLimit() != w.Desired.TrafficLimitBytes {
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
