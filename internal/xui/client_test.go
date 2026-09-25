package xui

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestNewPanelURLTransportPolicy(t *testing.T) {
	for _, tt := range []struct {
		url   string
		valid bool
	}{
		{url: "https://panel.example.test", valid: true},
		{url: "http://127.0.0.1:8088", valid: true},
		{url: "http://localhost:8088", valid: true},
		{url: "http://panel.example.test", valid: false},
	} {
		_, err := New(tt.url, "test-panel-token", time.Second)
		if (err == nil) != tt.valid {
			t.Errorf("New(%q) error = %v, want valid=%t", tt.url, err, tt.valid)
		}
	}
}

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

func TestRestoreCollisionReadbackRequiresTypedFullClientAndInboundLists(t *testing.T) {
	seen := map[string]bool{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			t.Error("missing panel auth")
		}
		seen[r.URL.Path] = true
		switch r.URL.Path {
		case "/panel/api/clients/list":
			_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "obj": []any{map[string]any{"email": "alice", "uuid": "u1", "subId": "s1", "inboundIds": []int{1}}}})
		case "/panel/api/inbounds/list":
			_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "obj": []any{map[string]any{"id": 1, "clientStats": []any{map[string]any{"email": "alice", "uuid": "u1", "subId": "s1"}}}}})
		case "/panel/api/inbounds/options":
			_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "obj": []any{map[string]any{"id": 1}}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c, err := New(srv.URL, "test-token", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	clients, err := c.ListClients(context.Background())
	if err != nil || len(clients) != 1 {
		t.Fatalf("ListClients = %v, %v", clients, err)
	}
	inbounds, err := c.ListInboundAttachments(context.Background())
	if err != nil || len(inbounds) != 1 || len(inbounds[0].Clients) != 1 {
		t.Fatalf("ListInboundAttachments = %v, %v", inbounds, err)
	}
	options, err := c.ListInboundOptions(context.Background())
	if err != nil || len(options) != 1 || options[0] != 1 {
		t.Fatalf("ListInboundOptions = %v, %v", options, err)
	}
	for _, path := range []string{"/panel/api/clients/list", "/panel/api/inbounds/list", "/panel/api/inbounds/options"} {
		if !seen[path] {
			t.Errorf("did not call documented endpoint %s", path)
		}
	}
}

func TestListClientsFailsClosedWhenIdentityFieldsAreMissing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "obj": []any{map[string]any{"email": "alice", "subId": "s1"}}})
	}))
	defer srv.Close()
	c, err := New(srv.URL, "test-token", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.ListClients(context.Background()); err == nil {
		t.Fatal("expected incomplete full-client response to be rejected")
	}
}
