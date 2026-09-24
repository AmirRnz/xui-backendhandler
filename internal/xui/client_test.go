package xui

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestVersionGateRejectsNearbyPanelReleaseBeforeWrite(t *testing.T) {
	writes := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/panel/api/server/getPanelUpdateInfo":
			_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "obj": map[string]any{"currentVersion": "3.8.50"}})
		case "/panel/api/inbounds/options":
			_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "obj": []any{}})
		case "/panel/api/clients/add":
			writes++
			_ = json.NewEncoder(w).Encode(map[string]any{"success": true})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c, err := New(srv.URL, "test-panel-token", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	result := c.Add(context.Background(), ClientConfig{Email: "test@example.invalid"}, []int{1})
	if result.Outcome != DefinitiveNoWrite || result.Err == nil {
		t.Fatalf("unexpected readiness result: %+v", result)
	}
	if writes != 0 {
		t.Fatalf("readiness failure crossed the write boundary %d times", writes)
	}
}

func TestPartialAddResponseIsUnknownAndUsesDocumentedEndpoint(t *testing.T) {
	adds := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/panel/api/server/getPanelUpdateInfo":
			_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "obj": map[string]any{"currentVersion": "3.8.5"}})
		case "/panel/api/inbounds/options":
			_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "obj": []any{map[string]any{"id": 1}}})
		case "/panel/api/clients/add":
			adds++
			var req struct {
				Client     ClientConfig `json:"client"`
				InboundIDs []int        `json:"inboundIds"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Error(err)
			}
			if req.Client.LimitIP != 2 || req.Client.LimitHWID != 0 || len(req.InboundIDs) != 2 {
				t.Errorf("unexpected 3x-ui add payload: %+v", req)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"success": false, "msg": "inbound 2: write failed"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c, err := New(srv.URL, "test-panel-token", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	result := c.Add(context.Background(), ClientConfig{Email: "test@example.invalid", LimitIP: 2, LimitHWID: 0}, []int{1, 2})
	if result.Outcome != Unknown {
		t.Fatalf("partial panel response must be treated as unknown, got %+v", result)
	}
	if adds != 1 {
		t.Fatalf("expected one add request, got %d", adds)
	}
}

func TestGetClientAndLinksUseContractPaths(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/panel/api/clients/get/demo@example.invalid":
			_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "obj": map[string]any{"email": "demo@example.invalid", "uuid": "uuid-1", "subId": "sub-1", "inboundIds": []int{3}}})
		case "/panel/api/clients/subLinks/sub-1":
			_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "obj": []string{"vless://sample"}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c, err := New(srv.URL, "test-panel-token", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	remote, err := c.GetClient(context.Background(), "demo@example.invalid")
	if err != nil {
		t.Fatal(err)
	}
	if UUIDOf(remote) != "uuid-1" || remote.SubID != "sub-1" || len(remote.InboundIDs) != 1 {
		t.Fatalf("unexpected remote identity: %+v", remote)
	}
	links, err := c.SubscriptionLinks(context.Background(), "sub-1")
	if err != nil || len(links) != 1 {
		t.Fatalf("unexpected links: %v, %v", links, err)
	}
}
