package backend

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCallAnnotatesHTTPErrorWithOperation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":{"code":"forbidden","message":"admin only"}}`))
	}))
	defer server.Close()

	client, err := New(server.URL, "test-token", 0)
	if err != nil {
		t.Fatal(err)
	}
	err = client.Call(context.Background(), http.MethodPost, "/v1/admin/config/plans", 123, map[string]string{"name": "plan"}, nil)
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("Call error = %v, want *APIError", err)
	}
	if apiErr.Status != http.StatusForbidden || apiErr.Code != "forbidden" || apiErr.Method != http.MethodPost || apiErr.Path != "/v1/admin/config/plans" {
		t.Fatalf("API error lacks useful operation details: %+v", apiErr)
	}
}
