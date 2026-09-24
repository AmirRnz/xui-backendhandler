package instancecli

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestInstanceConfigRoundTripDoesNotDuplicateSecretsToMetadata(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XUI_BACKEND_INSTANCE_ROOT", root)
	cfg := Config{Slug: "retail-one", Channel: "retail", DisplayName: "Retail One", DeploymentID: "retail-one", BackendURL: "https://backend.example", BackendToken: "scoped-backend-secret", BotToken: "123456789:telegram-secret-valid-length", AdminID: 42}
	if err := save(cfg); err != nil {
		t.Fatal(err)
	}
	got, err := Read(cfg.Slug)
	if err != nil {
		t.Fatal(err)
	}
	if got != cfg {
		t.Fatalf("Read() = %#v, want %#v", got, cfg)
	}
	metadata, err := os.ReadFile(filepath.Join(root, cfg.Slug, "metadata.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(metadata), cfg.BackendToken) || strings.Contains(string(metadata), cfg.BotToken) {
		t.Fatal("metadata.json duplicated an instance secret")
	}
	secretFile, err := os.Stat(filepath.Join(root, cfg.Slug, "instance.env"))
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && secretFile.Mode().Perm() != 0600 {
		t.Fatalf("instance.env permissions = %o, want 600", secretFile.Mode().Perm())
	}
}

func TestValidSlug(t *testing.T) {
	for _, value := range []string{"ab", "retail-01", "reseller-east-2"} {
		if !validSlug(value) {
			t.Errorf("validSlug(%q) = false", value)
		}
	}
	for _, value := range []string{"", "a", "-bad", "bad-", "Upper", "has space", "../escape", strings.Repeat("a", 49)} {
		if validSlug(value) {
			t.Errorf("validSlug(%q) = true", value)
		}
	}
}

func TestNewResourceIDsAreUniqueAndScoped(t *testing.T) {
	first, err := newResourceID("instance")
	if err != nil {
		t.Fatal(err)
	}
	second, err := newResourceID("instance")
	if err != nil {
		t.Fatal(err)
	}
	if first == second || !strings.HasPrefix(first, "instance-") || len(first) != len("instance-")+16 {
		t.Fatalf("resource IDs are not unique scoped IDs: %q %q", first, second)
	}
}

func TestRegistrationCompensationOnlyCleansAfterRevocation(t *testing.T) {
	cfg := Config{Slug: "retail-one", DeploymentID: "instance-1234567890abcdef"}
	cause := errors.New("unit install failed")
	cleanupCalled := false
	revokeFailure := errors.New("database unavailable")
	err := compensateRegistrationWith(cfg, cause, func() error { return revokeFailure }, func() error { cleanupCalled = true; return nil })
	if !errors.Is(err, revokeFailure) {
		t.Fatalf("error = %v, expected revocation failure", err)
	}
	if cleanupCalled {
		t.Fatal("local files were cleaned up before credential revocation")
	}
	cleanupErr := errors.New("cleanup failure")
	err = compensateRegistrationWith(cfg, cause, func() error { return nil }, func() error { cleanupCalled = true; return cleanupErr })
	if !cleanupCalled || !errors.Is(err, cleanupErr) {
		t.Fatalf("cleanup compensation result = %v", err)
	}
	if !strings.Contains(err.Error(), cfg.DeploymentID) {
		t.Fatalf("error omitted deployment reference: %v", err)
	}
}
