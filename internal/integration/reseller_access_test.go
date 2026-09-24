package integration

import (
	"context"
	"encoding/json"
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

func fmtInt(value int64) string {
	return strconv.FormatInt(value, 10)
}
