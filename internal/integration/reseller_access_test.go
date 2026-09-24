package integration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"example.com/xui-commerce/backend/internal/api"
	"example.com/xui-commerce/backend/internal/config"
	"example.com/xui-commerce/backend/internal/store"
)

const resellerAccessToken = "reseller-turk1-test-token-000000000000"

func resellerAccessHandler(s *store.Store) http.Handler {
	return api.New(s, config.Config{ClientCredentials: []config.ClientCredential{{DeploymentID: "reseller-turk1", Token: resellerAccessToken}}}, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func resellerAccessRequest(handler http.Handler, telegramID int64) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/v1/reseller/access-requests", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer "+resellerAccessToken)
	req.Header.Set("X-Actor-Telegram-ID", fmtInt(telegramID))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func TestResellerAccessRequestQueuesAdminNotificationAndEnforcesCooldown(t *testing.T) {
	s, _ := testStore(t)
	ctx := context.Background()
	admin := resolve(t, s, "reseller-turk1", 96937669)
	applicant := resolve(t, s, "reseller-turk1", 97654380)
	handler := resellerAccessHandler(s)

	first := resellerAccessRequest(handler, applicant.TelegramID)
	if first.Code != http.StatusAccepted || !strings.Contains(first.Body.String(), `"status":"submitted"`) {
		t.Fatalf("first access request status=%d body=%s", first.Code, first.Body.String())
	}
	var notification struct {
		ActorID int64  `json:"actor_id"`
		Topic   string `json:"topic"`
	}
	if err := s.DB.QueryRow(ctx, `SELECT actor_id,topic FROM outbox WHERE deployment_id='reseller-turk1' AND dedupe_key LIKE 'reseller-access-request:%'`).Scan(&notification.ActorID, &notification.Topic); err != nil {
		t.Fatal(err)
	}
	if notification.ActorID != admin.ID || notification.Topic != "reseller.access_requested" {
		t.Fatalf("notification recipient/topic = %+v, want admin actor %d and reseller.access_requested", notification, admin.ID)
	}

	replay := resellerAccessRequest(handler, applicant.TelegramID)
	if replay.Code != http.StatusAccepted || !strings.Contains(replay.Body.String(), `"status":"already_pending"`) {
		t.Fatalf("cooldown response status=%d body=%s", replay.Code, replay.Body.String())
	}
	var replayBody struct {
		Status        string    `json:"status"`
		NextRequestAt time.Time `json:"next_request_at"`
	}
	if err := json.Unmarshal(replay.Body.Bytes(), &replayBody); err != nil {
		t.Fatal(err)
	}
	if replayBody.NextRequestAt.Before(time.Now().Add(23 * time.Hour)) {
		t.Fatalf("next_request_at did not reflect 24-hour cooldown: %s", replayBody.NextRequestAt)
	}
	var requestCount, outboxCount int
	if err := s.DB.QueryRow(ctx, `SELECT count(*) FROM reseller_access_requests WHERE deployment_id='reseller-turk1' AND actor_id=$1`, applicant.ID).Scan(&requestCount); err != nil {
		t.Fatal(err)
	}
	if err := s.DB.QueryRow(ctx, `SELECT count(*) FROM outbox WHERE deployment_id='reseller-turk1' AND dedupe_key LIKE 'reseller-access-request:%'`).Scan(&outboxCount); err != nil {
		t.Fatal(err)
	}
	if requestCount != 1 || outboxCount != 1 {
		t.Fatalf("cooldown replay created duplicate durable work: requests=%d outbox=%d", requestCount, outboxCount)
	}

	retailActor := resolve(t, s, "retail-finland", applicant.TelegramID)
	retailHandler := api.New(s, config.Config{ClientCredentials: []config.ClientCredential{{DeploymentID: "retail-finland", Token: "retail-finland-test-token-000000000000"}}}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	req := httptest.NewRequest(http.MethodPost, "/v1/reseller/access-requests", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer retail-finland-test-token-000000000000")
	req.Header.Set("X-Actor-Telegram-ID", fmtInt(retailActor.TelegramID))
	req.Header.Set("Content-Type", "application/json")
	denied := httptest.NewRecorder()
	retailHandler.ServeHTTP(denied, req)
	if denied.Code != http.StatusForbidden {
		t.Fatalf("retail deployment access request should be forbidden: %d %s", denied.Code, denied.Body.String())
	}
	if _, err := s.SetResellerApproval(ctx, "reseller-turk1", admin.ID, applicant.TelegramID, "approved"); err != nil {
		t.Fatal(err)
	}
	afterApproval := resellerAccessRequest(handler, applicant.TelegramID)
	if afterApproval.Code != http.StatusForbidden {
		t.Fatalf("approved reseller access request should be forbidden: %d %s", afterApproval.Code, afterApproval.Body.String())
	}
}

func TestConcurrentResellerAccessClicksQueueOneNotification(t *testing.T) {
	s, _ := testStore(t)
	applicant := resolve(t, s, "reseller-turk1", 97654381)
	handler := resellerAccessHandler(s)
	results := make(chan *httptest.ResponseRecorder, 2)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results <- resellerAccessRequest(handler, applicant.TelegramID)
		}()
	}
	wg.Wait()
	close(results)
	submitted, alreadyPending := 0, 0
	for rec := range results {
		if rec.Code != http.StatusAccepted {
			t.Fatalf("concurrent request status=%d body=%s", rec.Code, rec.Body.String())
		}
		var body struct {
			Status string `json:"status"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		switch body.Status {
		case "submitted":
			submitted++
		case "already_pending":
			alreadyPending++
		default:
			t.Fatalf("unexpected concurrent response: %s", rec.Body.String())
		}
	}
	if submitted != 1 || alreadyPending != 1 {
		t.Fatalf("concurrent requests were not serialized: submitted=%d already_pending=%d", submitted, alreadyPending)
	}
	var requests, notifications int
	if err := s.DB.QueryRow(context.Background(), `SELECT count(*) FROM reseller_access_requests WHERE deployment_id='reseller-turk1' AND actor_id=$1`, applicant.ID).Scan(&requests); err != nil {
		t.Fatal(err)
	}
	if err := s.DB.QueryRow(context.Background(), `SELECT count(*) FROM outbox WHERE deployment_id='reseller-turk1' AND topic='reseller.access_requested'`).Scan(&notifications); err != nil {
		t.Fatal(err)
	}
	if requests != 1 || notifications != 1 {
		t.Fatalf("concurrent clicks created duplicates: requests=%d notifications=%d", requests, notifications)
	}
}

func TestResellerApprovalAndRejectionNotifyApplicantDurablyOnce(t *testing.T) {
	s, _ := testStore(t)
	ctx := context.Background()
	admin := resolve(t, s, "reseller-turk1", 96937669)
	approved := resolve(t, s, "reseller-turk1", 97654390)
	rejected := resolve(t, s, "reseller-turk1", 97654391)
	retailCollision := resolve(t, s, "retail-finland", approved.TelegramID)
	for _, tc := range []struct {
		applicant     *store.Actor
		status, topic string
	}{
		{approved, "approved", "reseller.access_approved"},
		{rejected, "rejected", "reseller.access_rejected"},
	} {
		if _, err := s.SetResellerApproval(ctx, "reseller-turk1", admin.ID, tc.applicant.TelegramID, tc.status); err != nil {
			t.Fatalf("set reseller status %s: %v", tc.status, err)
		}
		var deployment string
		var accountID, actorID int64
		var topic string
		var count int
		if err := s.DB.QueryRow(ctx, `SELECT deployment_id,account_id,actor_id,topic,count(*) OVER() FROM outbox WHERE deployment_id='reseller-turk1' AND dedupe_key=$1`, fmt.Sprintf("reseller-access-decision:%d", tc.applicant.ID)).Scan(&deployment, &accountID, &actorID, &topic, &count); err != nil {
			t.Fatal(err)
		}
		if deployment != "reseller-turk1" || accountID != tc.applicant.AccountID || actorID != tc.applicant.ID || topic != tc.topic || count != 1 {
			t.Fatalf("decision notification scope/topic mismatch: deployment=%s account=%d actor=%d topic=%s count=%d", deployment, accountID, actorID, topic, count)
		}
		if _, err := s.SetResellerApproval(ctx, "reseller-turk1", admin.ID, tc.applicant.TelegramID, tc.status); !errors.Is(err, store.ErrConflict) {
			t.Fatalf("decision replay should not create another notification: %v", err)
		}
		if err := s.DB.QueryRow(ctx, `SELECT count(*) FROM outbox WHERE deployment_id='reseller-turk1' AND actor_id=$1 AND topic=$2`, tc.applicant.ID, tc.topic).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Fatalf("replayed decision queued %d applicant notifications", count)
		}
	}
	var collisionNotifications int
	if err := s.DB.QueryRow(ctx, `SELECT count(*) FROM outbox WHERE deployment_id='retail-finland' AND actor_id=$1`, retailCollision.ID).Scan(&collisionNotifications); err != nil {
		t.Fatal(err)
	}
	if collisionNotifications != 0 {
		t.Fatalf("reseller decision notification crossed into colliding retail actor: %d", collisionNotifications)
	}
}

func TestResellerDecisionNotificationRollsBackWithUnauthorizedAdmin(t *testing.T) {
	s, _ := testStore(t)
	ctx := context.Background()
	applicant := resolve(t, s, "reseller-turk1", 97654392)
	if _, err := s.SetResellerApproval(ctx, "reseller-turk1", 0, applicant.TelegramID, "approved"); !errors.Is(err, store.ErrForbidden) {
		t.Fatalf("unauthorized decision error = %v", err)
	}
	var status string
	var notifications int
	if err := s.DB.QueryRow(ctx, `SELECT approval_status FROM actors WHERE id=$1`, applicant.ID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if err := s.DB.QueryRow(ctx, `SELECT count(*) FROM outbox WHERE deployment_id='reseller-turk1' AND actor_id=$1 AND topic IN ('reseller.access_approved','reseller.access_rejected')`, applicant.ID).Scan(&notifications); err != nil {
		t.Fatal(err)
	}
	if status != "pending" || notifications != 0 {
		t.Fatalf("failed decision left partial state: approval=%s notifications=%d", status, notifications)
	}
}

func fmtInt(value int64) string {
	return strconv.FormatInt(value, 10)
}
