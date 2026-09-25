package integration

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"example.com/xui-commerce/backend/internal/api"
	"example.com/xui-commerce/backend/internal/config"
	"example.com/xui-commerce/backend/internal/secrets"
	"example.com/xui-commerce/backend/internal/store"
)

func TestDeploymentTransferLeaseDrainsRequestsAndBlocksNewOnes(t *testing.T) {
	s, _ := testStore(t)
	request, err := s.AcquireDeploymentRequestLease(context.Background(), "lease-test-deployment")
	if err != nil {
		t.Fatal(err)
	}
	gotTransfer := make(chan *store.DeploymentRequestLease, 1)
	gotErr := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		lease, e := s.AcquireDeploymentTransferLease(ctx, "lease-test-deployment")
		if e != nil {
			gotErr <- e
			return
		}
		gotTransfer <- lease
	}()
	select {
	case lease := <-gotTransfer:
		_ = lease.Release(context.Background())
		t.Fatal("transfer lock passed an in-flight request")
	case err := <-gotErr:
		t.Fatal(err)
	case <-time.After(100 * time.Millisecond):
	}
	if err = request.Release(context.Background()); err != nil {
		t.Fatal(err)
	}
	var transfer *store.DeploymentRequestLease
	select {
	case transfer = <-gotTransfer:
	case err = <-gotErr:
		t.Fatal(err)
	case <-time.After(3 * time.Second):
		t.Fatal("transfer lock did not acquire after request completion")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	if request, err = s.AcquireDeploymentRequestLease(ctx, "lease-test-deployment"); err == nil {
		_ = request.Release(context.Background())
		t.Fatal("new request passed an active transfer lock")
	}
	if err = transfer.Release(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestRestoredDeploymentActivationKeepsFrozenUntilChecksPass(t *testing.T) {
	s, db := testStore(t)
	ctx := context.Background()
	key := bytes.Repeat([]byte{0x44}, 32)
	registered, err := s.RegisterInstance(ctx, store.RegisterInstanceInput{
		DeploymentID: "retail-activation-test", Channel: "retail", DisplayName: "Activation test",
		PanelID: "panel-retail-activation-test", PanelURL: "https://panel.example.test", PanelToken: "panel-key",
		TelegramToken: "telegram-key", AdminTelegramID: 99124,
	}, key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(ctx, `UPDATE deployments SET transfer_frozen=true,restore_fingerprint='activation-hash' WHERE id=$1`, registered.DeploymentID); err != nil {
		t.Fatal(err)
	}
	var accountID, actorID int64
	if err = db.QueryRow(ctx, `SELECT a.account_id,a.id FROM actors a WHERE a.deployment_id=$1 AND a.telegram_id=$2`, registered.DeploymentID, 99124).Scan(&accountID, &actorID); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(ctx, `INSERT INTO work_items(deployment_id,account_id,actor_id,panel_id,operation_key,input_hash,kind,desired_state) VALUES($1,$2,$3,$4,'activation-work','activation-hash','provision_add','{}'::jsonb)`, registered.DeploymentID, accountID, actorID, "panel-retail-activation-test"); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(ctx, `INSERT INTO outbox(deployment_id,account_id,actor_id,dedupe_key,topic,payload) VALUES($1,$2,$3,'activation-notice','subscription.ready','{}'::jsonb)`, registered.DeploymentID, accountID, actorID); err != nil {
		t.Fatal(err)
	}

	transfer, err := s.AcquireDeploymentTransferLease(ctx, registered.DeploymentID)
	if err != nil {
		t.Fatal(err)
	}
	defer transfer.Release(context.Background())
	if err = transfer.ValidateRestoredAdmin(ctx, registered.DeploymentID, registered.BackendToken, 99124); err != nil {
		t.Fatalf("restored credential could not be validated while frozen: %v", err)
	}
	if enabled, err := transfer.IsEnabled(ctx, true, registered.DeploymentID); err != nil || enabled {
		t.Fatalf("frozen deployment became enabled during activation checks: enabled=%t err=%v", enabled, err)
	}

	handler := api.New(s, config.Config{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	req := httptest.NewRequest(http.MethodGet, "/v1/admin/config", nil)
	req.Header.Set("Authorization", "Bearer "+registered.BackendToken)
	req.Header.Set("X-Actor-Telegram-ID", "99124")
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusUnauthorized {
		t.Fatalf("frozen restored credential reached the HTTP API: status=%d body=%s", res.Code, res.Body.String())
	}
	if work, err := s.ClaimWork(ctx); err != nil || work != nil {
		t.Fatalf("worker claimed work during frozen activation: work=%+v err=%v", work, err)
	}
	if notifications, err := s.OutboxPending(ctx, 10); err != nil || len(notifications) != 0 {
		t.Fatalf("notification worker acted during frozen activation: items=%d err=%v", len(notifications), err)
	}
	requestCtx, cancel := context.WithTimeout(ctx, 150*time.Millisecond)
	defer cancel()
	if request, err := s.AcquireDeploymentRequestLease(requestCtx, registered.DeploymentID); err == nil {
		_ = request.Release(context.Background())
		t.Fatal("request lease passed the activation transfer lock")
	}
}

func TestRestoredDeploymentRefreezeVerifiesCheckpointAndState(t *testing.T) {
	s, db := testStore(t)
	ctx := context.Background()
	key := bytes.Repeat([]byte{0x33}, 32)
	registered, err := s.RegisterInstance(ctx, store.RegisterInstanceInput{
		DeploymentID: "retail-refreeze-test", Channel: "retail", DisplayName: "Refreeze test",
		PanelID: "panel-refreeze-test", PanelURL: "https://panel.example.test", PanelToken: "panel-key",
		TelegramToken: "telegram-key", AdminTelegramID: 99123,
	}, key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(ctx, `UPDATE deployments SET transfer_frozen=false,restore_fingerprint='archive-hash' WHERE id=$1`, registered.DeploymentID); err != nil {
		t.Fatal(err)
	}
	if err = s.RefreezeRestoredDeployment(ctx, registered.DeploymentID, "wrong-archive"); err == nil {
		t.Fatal("freeze must refuse a mismatched restore checkpoint")
	}
	var frozen bool
	if err = db.QueryRow(ctx, `SELECT transfer_frozen FROM deployments WHERE id=$1`, registered.DeploymentID).Scan(&frozen); err != nil || frozen {
		t.Fatalf("mismatched checkpoint changed state unexpectedly: frozen=%t err=%v", frozen, err)
	}
	if err = s.RefreezeRestoredDeployment(ctx, registered.DeploymentID, "archive-hash"); err != nil {
		t.Fatal(err)
	}
	if err = db.QueryRow(ctx, `SELECT transfer_frozen FROM deployments WHERE id=$1`, registered.DeploymentID).Scan(&frozen); err != nil || !frozen {
		t.Fatalf("refreeze was not persisted and verified: frozen=%t err=%v", frozen, err)
	}
}

func TestRegisterInstanceCreatesScopedRuntimeIdentity(t *testing.T) {
	s, _ := testStore(t)
	key := bytes.Repeat([]byte{0x42}, 32)
	first, err := s.RegisterInstance(context.Background(), store.RegisterInstanceInput{
		DeploymentID: "retail-north-test", Channel: "retail", DisplayName: "North retail",
		PanelID: "panel-retail-north-test", PanelURL: "https://panel.example.test",
		PanelToken: "panel-secret-value", TelegramToken: "telegram-secret-value", AdminTelegramID: 123456,
	}, key)
	if err != nil {
		t.Fatal(err)
	}
	if first.DeploymentID != "retail-north-test" || first.BackendToken == "" {
		t.Fatalf("registration result is incomplete: %+v", first)
	}
	for _, tc := range []struct {
		table, column, where string
		secret               string
	}{
		{"panels", "encrypted_api_token", "id='panel-retail-north-test'", "panel-secret-value"},
		{"deployments", "encrypted_telegram_token", "id='retail-north-test'", "telegram-secret-value"},
	} {
		var ciphertext []byte
		if err := s.DB.QueryRow(context.Background(), "SELECT "+tc.column+" FROM "+tc.table+" WHERE "+tc.where).Scan(&ciphertext); err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(ciphertext, []byte(tc.secret)) {
			t.Fatalf("%s stored a secret in plaintext", tc.table)
		}
	}

	second, err := s.RegisterInstance(context.Background(), store.RegisterInstanceInput{
		DeploymentID: "reseller-north-test", Channel: "reseller", DisplayName: "North reseller",
		PanelID: "panel-reseller-north-test", PanelURL: "https://reseller-panel.example.test",
		PanelToken: "second-panel-secret", TelegramToken: "second-telegram-secret", AdminTelegramID: 123456,
	}, key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.RegisterInstance(context.Background(), store.RegisterInstanceInput{
		DeploymentID: "retail-north-test", Channel: "retail", DisplayName: "Duplicate",
		PanelID: "panel-duplicate", PanelURL: "https://duplicate-panel.example.test",
		PanelToken: "duplicate-panel-secret", TelegramToken: "duplicate-telegram-secret", AdminTelegramID: 123456,
	}, key); err == nil {
		t.Fatal("duplicate deployment ID was accepted")
	}
	if _, err = s.RegisterInstance(context.Background(), store.RegisterInstanceInput{
		DeploymentID: "retail-collision-test", Channel: "retail", DisplayName: "Panel collision",
		PanelID: "panel-retail-north-test", PanelURL: "https://collision-panel.example.test",
		PanelToken: "collision-panel-secret", TelegramToken: "collision-telegram-secret", AdminTelegramID: 123456,
	}, key); err == nil {
		t.Fatal("duplicate panel ID was accepted")
	}
	for token, wantDeployment := range map[string]string{first.BackendToken: first.DeploymentID, second.BackendToken: second.DeploymentID} {
		got, err := s.AuthenticateClient(context.Background(), token)
		if err != nil || got != wantDeployment {
			t.Fatalf("credential mapped to %q, want %q (err=%v)", got, wantDeployment, err)
		}
	}
	var firstAccount, secondAccount int64
	if err = s.DB.QueryRow(context.Background(), `SELECT account_id FROM actors WHERE deployment_id=$1 AND telegram_id=123456`, first.DeploymentID).Scan(&firstAccount); err != nil {
		t.Fatal(err)
	}
	if err = s.DB.QueryRow(context.Background(), `SELECT account_id FROM actors WHERE deployment_id=$1 AND telegram_id=123456`, second.DeploymentID).Scan(&secondAccount); err != nil {
		t.Fatal(err)
	}
	if firstAccount == secondAccount {
		t.Fatal("the same Telegram ID shared an account across deployments")
	}
	var secondActor int64
	if err = s.DB.QueryRow(context.Background(), `SELECT id FROM actors WHERE deployment_id=$1 AND telegram_id=123456`, second.DeploymentID).Scan(&secondActor); err != nil {
		t.Fatal(err)
	}
	if _, err = s.DB.Exec(context.Background(), `INSERT INTO work_items(deployment_id,account_id,actor_id,panel_id,operation_key,input_hash,kind,desired_state) VALUES($1,$2,$3,$4,'instance-removal-work','instance-removal-hash','provision_add','{}'::jsonb)`, second.DeploymentID, secondAccount, secondActor, "panel-reseller-north-test"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.DB.Exec(context.Background(), `INSERT INTO outbox(deployment_id,account_id,actor_id,dedupe_key,topic,payload) VALUES($1,$2,$3,'instance-removal-notification','subscription.ready','{}'::jsonb)`, second.DeploymentID, secondAccount, secondActor); err != nil {
		t.Fatal(err)
	}

	handler := api.New(s, config.Config{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	for _, tc := range []struct{ token, deployment, channel string }{
		{first.BackendToken, first.DeploymentID, "retail"},
		{second.BackendToken, second.DeploymentID, "reseller"},
	} {
		req := httptest.NewRequest(http.MethodGet, "/v1/admin/config", nil)
		req.Header.Set("Authorization", "Bearer "+tc.token)
		req.Header.Set("X-Actor-Telegram-ID", "123456")
		res := httptest.NewRecorder()
		handler.ServeHTTP(res, req)
		if res.Code != http.StatusOK || !bytes.Contains(res.Body.Bytes(), []byte(`"deployment_id":"`+tc.deployment+`"`)) || !bytes.Contains(res.Body.Bytes(), []byte(`"channel":"`+tc.channel+`"`)) {
			t.Fatalf("registered credential escaped its scope: status=%d body=%s", res.Code, res.Body.String())
		}
	}
	if err = s.DeactivateInstance(context.Background(), second.DeploymentID); err != nil {
		t.Fatal(err)
	}
	revoked := httptest.NewRequest(http.MethodGet, "/v1/admin/config", nil)
	revoked.Header.Set("Authorization", "Bearer "+second.BackendToken)
	revoked.Header.Set("X-Actor-Telegram-ID", "123456")
	revokedResponse := httptest.NewRecorder()
	handler.ServeHTTP(revokedResponse, revoked)
	if revokedResponse.Code != http.StatusUnauthorized {
		t.Fatalf("deactivated credential still authenticated: %d %s", revokedResponse.Code, revokedResponse.Body.String())
	}
	var retained int
	if err = s.DB.QueryRow(context.Background(), `SELECT count(*) FROM commercial_accounts WHERE home_deployment_id=$1`, second.DeploymentID).Scan(&retained); err != nil {
		t.Fatal(err)
	}
	if retained != 1 {
		t.Fatalf("deactivation removed commercial history; retained account count=%d", retained)
	}
	if work, err := s.ClaimWork(context.Background()); err != nil || work == nil || work.OperationKey != "instance-removal-work" {
		t.Fatalf("deactivation did not preserve provisioning work: work=%+v err=%v", work, err)
	}
	if notifications, err := s.OutboxPending(context.Background(), 10); err != nil || len(notifications) != 0 {
		t.Fatalf("deactivation did not pause Telegram notifications: items=%d err=%v", len(notifications), err)
	}

	reactivated, err := s.RegisterInstance(context.Background(), store.RegisterInstanceInput{
		DeploymentID: second.DeploymentID, Channel: "reseller", DisplayName: "North reseller",
		PanelID: "panel-reseller-north-test", PanelURL: "https://reseller-panel.example.test",
		PanelToken: "rotated-panel-secret", TelegramToken: "rotated-telegram-secret", AdminTelegramID: 123456,
	}, key)
	if err != nil {
		t.Fatalf("same retained instance could not be reactivated safely: %v", err)
	}
	if reactivated.BackendToken == second.BackendToken {
		t.Fatal("reactivation reused a revoked backend credential")
	}
	if _, err := s.AuthenticateClient(context.Background(), second.BackendToken); err == nil {
		t.Fatal("old credential remained valid after reactivation")
	}
	var encryptedTelegram []byte
	if err = s.DB.QueryRow(context.Background(), `SELECT encrypted_telegram_token FROM deployments WHERE id=$1`, second.DeploymentID).Scan(&encryptedTelegram); err != nil {
		t.Fatal(err)
	}
	plainTelegram, err := secrets.Open(key, encryptedTelegram)
	if err != nil || string(plainTelegram) != "rotated-telegram-secret" {
		t.Fatalf("reactivation did not rotate encrypted Telegram token: err=%v", err)
	}
	resumed, err := s.OutboxPending(context.Background(), 10)
	if err != nil || len(resumed) != 1 {
		t.Fatalf("reactivation did not resume retained notification: items=%d err=%v", len(resumed), err)
	}
}
