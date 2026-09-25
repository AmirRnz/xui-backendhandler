package instancecli

import "testing"

func TestLocalBackendURLUsesConfiguredPortAndSafeLoopback(t *testing.T) {
	for _, tc := range []struct{ listen, want string }{
		{"127.0.0.1:8088", "http://127.0.0.1:8088"},
		{"0.0.0.0:9001", "http://127.0.0.1:9001"},
		{"[::]:8443", "http://127.0.0.1:8443"},
	} {
		got, ok := localBackendURL(tc.listen)
		if !ok || got != tc.want {
			t.Fatalf("localBackendURL(%q) = %q, %v; want %q", tc.listen, got, ok, tc.want)
		}
	}
}

func TestLocalBackendURLRejectsInvalidListenAddress(t *testing.T) {
	for _, raw := range []string{"", "localhost", "127.0.0.1", ":", "bad:port:extra"} {
		if got, ok := localBackendURL(raw); ok {
			t.Fatalf("localBackendURL(%q) = %q, want invalid", raw, got)
		}
	}
}

func TestResumeTransferRequiresExplicitStoppedDestinationConfirmation(t *testing.T) {
	if err := Command([]string{"transfer", "resume", "source"}); err == nil {
		t.Fatal("resume must require explicit confirmation destination is stopped")
	}
}

func TestInstanceRestoreRequiresExplicitSourceStoppedConfirmation(t *testing.T) {
	err := Command([]string{"restore", "instance", "missing.zip", "retail-copy"})
	if err == nil || err.Error() != "usage: xui-backend restore instance <archive> <new-instance-slug> [--dry-run|--source-stopped]" {
		t.Fatalf("restore without source-stopped confirmation was not rejected: %v", err)
	}
	// With the explicit confirmation, execution advances to archive inspection.
	if err = Command([]string{"restore", "instance", "missing.zip", "retail-copy", "--source-stopped"}); err == nil || err.Error() == "usage: xui-backend restore instance <archive> <new-instance-slug> [--dry-run|--source-stopped]" {
		t.Fatalf("confirmed restore did not reach archive validation: %v", err)
	}
}

func TestBackendURLRequiresTLSOutsideLoopback(t *testing.T) {
	for _, tc := range []struct {
		url  string
		want bool
	}{{"http://127.0.0.1:8088", true}, {"http://localhost:8088", true}, {"https://backend.example.test", true}, {"http://backend.example.test", false}} {
		if got := validBackendTransport(tc.url); got != tc.want {
			t.Errorf("validBackendTransport(%q)=%t want %t", tc.url, got, tc.want)
		}
	}
}
