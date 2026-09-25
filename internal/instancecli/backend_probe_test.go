package instancecli

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
)

func TestBackendHealthAndScopedIdentityProbe(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/healthz":
			w.WriteHeader(http.StatusOK)
		case "/v1/admin/config":
			if r.Header.Get("Authorization") != "Bearer scoped-secret" || r.Header.Get("X-Actor-Telegram-ID") != strconv.FormatInt(77, 10) {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	if err := checkBackendHealth(context.Background(), server.URL); err != nil {
		t.Fatal(err)
	}
	cfg := Config{BackendURL: server.URL, BackendToken: "scoped-secret", AdminID: 77}
	if err := checkBackendIdentity(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
}

func TestBackendIdentityProbeRejectsWrongPortOrCredentialWithoutPrintingToken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()
	if err := checkBackendIdentity(context.Background(), Config{BackendURL: server.URL, BackendToken: "wrong-secret", AdminID: 77}); err == nil {
		t.Fatal("expected bad scoped credential to fail")
	}
	if err := checkBackendHealth(context.Background(), "http://127.0.0.1:1"); err == nil {
		t.Fatal("expected unreachable backend URL to fail")
	}
}
