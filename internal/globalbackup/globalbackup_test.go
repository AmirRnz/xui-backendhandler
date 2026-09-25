package globalbackup

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"example.com/xui-commerce/backend/internal/store"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestAvailableLoopbackAddrPreservesFreePortAndRebindsBusyPort(t *testing.T) {
	free, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	freeAddr := free.Addr().String()
	_ = free.Close()
	got, err := availableLoopbackAddr(freeAddr)
	if err != nil {
		t.Fatal(err)
	}
	if got != freeAddr {
		t.Fatalf("free loopback address changed: got %s want %s", got, freeAddr)
	}

	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	got, err = availableLoopbackAddr(busy.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	if got == busy.Addr().String() {
		t.Fatalf("busy listener address was reused: %s", got)
	}
	if host, _, err := net.SplitHostPort(got); err != nil || host != "127.0.0.1" {
		t.Fatalf("fallback is not IPv4 loopback: %q (%v)", got, err)
	}
}

func TestActivationTargetsPreserveSourceActiveState(t *testing.T) {
	manifest := Manifest{BackendActive: true, Instances: []InstanceState{
		{Slug: "active-one", Active: true},
		{Slug: "inactive", Active: false},
	}}
	want := []string{"xui-backend.service", "xui-backend-instance-active-one.service"}
	got := activationTargets(manifest)
	if len(got) != len(want) {
		t.Fatalf("activation targets = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("activation targets = %v, want %v", got, want)
		}
	}
	manifest.BackendActive = false
	manifest.Instances = []InstanceState{{Slug: "idle", Active: false}}
	if got := activationTargets(manifest); len(got) != 0 {
		t.Fatalf("inactive source should restore without starting services, got %v", got)
	}
}

func TestGlobalMoveQuiescesEnabledUnitsAndRollbackRestoresPriorState(t *testing.T) {
	stop, disable := sourceMoveActions(unitStatus{Enabled: true, Active: true}, []InstanceState{
		{Slug: "running", Enabled: true, Active: true},
		{Slug: "stopped-but-enabled", Enabled: true, Active: false},
		{Slug: "disabled", Enabled: false, Active: false},
	})
	wantStop := []string{"xui-backend-instance-running.service", "xui-backend.service"}
	wantDisable := []string{"xui-backend-instance-running.service", "xui-backend-instance-stopped-but-enabled.service", "xui-backend.service"}
	if !reflect.DeepEqual(stop, wantStop) || !reflect.DeepEqual(disable, wantDisable) {
		t.Fatalf("move actions stop=%v disable=%v", stop, disable)
	}
	start, enable := sourceRollbackActions(stop, disable)
	if !reflect.DeepEqual(start, []string{"xui-backend.service", "xui-backend-instance-running.service"}) || !reflect.DeepEqual(enable, []string{"xui-backend.service", "xui-backend-instance-stopped-but-enabled.service", "xui-backend-instance-running.service"}) {
		t.Fatalf("failure rollback actions start=%v enable=%v", start, enable)
	}
}

func TestActivationRequiresBotServiceToRemainActive(t *testing.T) {
	if err := verifyServiceRemainsActive(context.Background(), "bot.service", func(string) string { return "active" }); err != nil {
		t.Fatalf("stable active service rejected: %v", err)
	}
	states := []string{"active", "activating"}
	index := 0
	err := verifyServiceRemainsActive(context.Background(), "bot.service", func(string) string {
		state := states[index]
		if index < len(states)-1 {
			index++
		}
		return state
	})
	if err == nil {
		t.Fatal("service that leaves active state was accepted")
	}
}

func TestScopedBackendIdentityProbeUsesRegisteredTokenAndAdmin(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/admin/config" || r.Header.Get("Authorization") != "Bearer scoped" || r.Header.Get("X-Actor-Telegram-ID") != "778899" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	_, port, err := net.SplitHostPort(strings.TrimPrefix(server.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{"instances/retail/instance.env": []byte("BACKEND_TOKEN=scoped\nADMIN_TELEGRAM_ID=778899\n")}
	if err = checkScopedBackendIdentity(context.Background(), "127.0.0.1:"+port, files, Manifest{Instances: []InstanceState{{Slug: "retail"}}}); err != nil {
		t.Fatal(err)
	}
}

func TestScopedBackendIdentityProbeRejectsLaterInvalidInstance(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.URL.Path != "/v1/admin/config" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.Header.Get("Authorization") != "Bearer valid" || r.Header.Get("X-Actor-Telegram-ID") != "778899" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	_, port, err := net.SplitHostPort(strings.TrimPrefix(server.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{
		"instances/first/instance.env":  []byte("BACKEND_TOKEN=valid\nADMIN_TELEGRAM_ID=778899\n"),
		"instances/second/instance.env": []byte("BACKEND_TOKEN=invalid\nADMIN_TELEGRAM_ID=990011\n"),
	}
	manifest := Manifest{Instances: []InstanceState{{Slug: "first"}, {Slug: "second"}}}
	if err = checkScopedBackendIdentity(context.Background(), "127.0.0.1:"+port, files, manifest); err == nil {
		t.Fatal("second instance with invalid credential/admin was accepted")
	}
	if requests != 2 {
		t.Fatalf("checked %d instance credentials; want all 2", requests)
	}
}

func TestRuntimeModesAllowServiceToTraverseRegistryAndReadPrivateConfig(t *testing.T) {
	m := targetRuntimeModes()
	if m.Registry.Perm() != 0710 || m.Instance.Perm() != 0700 || m.Secret.Perm() != 0600 || m.Env.Perm() != 0640 || m.Unit.Perm() != 0644 {
		t.Fatalf("unexpected portable runtime permission policy: %+v", m)
	}
}

func TestAtomicInstallAcceptsIdenticalExistingUnit(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "staged.service")
	dst := filepath.Join(dir, "xui-backend.service")
	want := []byte(backendUnit())
	if err := os.WriteFile(src, want, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, want, 0644); err != nil {
		t.Fatal(err)
	}
	if err := atomicInstall(src, dst, 0644); err != nil {
		t.Fatalf("identical preinstalled unit should be reusable: %v", err)
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Fatalf("staged duplicate was not removed: %v", err)
	}
	if err := os.WriteFile(src, []byte("different"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := atomicInstall(src, dst, 0644); err == nil {
		t.Fatal("different existing unit must not be overwritten")
	}
}

func TestRestoreTargetFilesRemovesPartiallyInstalledRegistry(t *testing.T) {
	root := t.TempDir()
	backend := filepath.Join(root, "backend.env")
	instances := filepath.Join(root, "instances")
	instanceDir := filepath.Join(instances, "partial")
	if err := os.MkdirAll(instanceDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(backend, []byte("new config"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := restoreTargetFiles(RestoreOptions{BackendEnvPath: backend, InstanceRoot: instances}, Manifest{Instances: []InstanceState{{Slug: "partial"}}}, nil, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(instanceDir); !os.IsNotExist(err) {
		t.Fatalf("partial instance directory remains: %v", err)
	}
	if _, err := os.Stat(backend); !os.IsNotExist(err) {
		t.Fatalf("new backend config remains: %v", err)
	}
}

func TestDatabaseRuntimeClosureFailsOnMissingConfigAndReportsInertRows(t *testing.T) {
	rows := []runtimeDeploymentRow{
		{ID: "configured", HasTelegramCredential: true, HasBackendCredential: true},
		{ID: "bootstrap", HasTelegramCredential: false, HasBackendCredential: false},
	}
	if _, err := validateRuntimeDeploymentRows(rows, map[string]string{}); err == nil || !strings.Contains(err.Error(), "no registered instance config/unit") {
		t.Fatalf("missing runtime config should fail closed, got %v", err)
	}
	inert, err := validateRuntimeDeploymentRows(rows[1:], map[string]string{})
	if err != nil || !reflect.DeepEqual(inert, []string{"bootstrap"}) {
		t.Fatalf("inert database-only row classification = %v, %v", inert, err)
	}
	if _, err = validateRuntimeDeploymentRows(rows, map[string]string{"configured": "kitten"}); err != nil {
		t.Fatalf("complete runtime closure rejected: %v", err)
	}
	if _, err = validateRuntimeDeploymentRows([]runtimeDeploymentRow{{ID: "partial", HasTelegramCredential: true}}, map[string]string{}); err == nil {
		t.Fatal("partial registered credentials must fail closed")
	}
}

func TestUnitRegistryClosureRejectsMissingAndOrphanUnits(t *testing.T) {
	root := t.TempDir()
	if err := verifyUnitRegistryClosure(root, []string{"kitten"}); err == nil {
		t.Fatal("missing registered instance unit was accepted")
	}
	unit := filepath.Join(root, instanceUnitName("orphan"))
	if err := os.WriteFile(unit, []byte(instanceUnit("orphan")), 0600); err != nil {
		t.Fatal(err)
	}
	if err := verifyUnitRegistryClosure(root, nil); err == nil {
		t.Fatal("orphan systemd unit without registry config was accepted")
	}
}

func TestNormalizeDatabaseDumpKeepsCopyDataAndRemovesTransactionWrapper(t *testing.T) {
	dump := []byte("SET standard_conforming_strings = on;\nBEGIN;\nCOPY public.sample(value) FROM stdin;\nBEGIN;\nCOMMIT;\n\\.\nCOMMIT;\n")
	got, err := normalizeDatabaseDumpTransactions(dump)
	if err != nil {
		t.Fatal(err)
	}
	want := []byte("SET standard_conforming_strings = on;\n\nCOPY public.sample(value) FROM stdin;\nBEGIN;\nCOMMIT;\n\\.\n\n")
	if !bytes.Equal(got, want) {
		t.Fatalf("normalized dump changed unexpectedly:\n%s", got)
	}
	if _, err = normalizeDatabaseDumpTransactions([]byte("BEGIN;\nSELECT 1;\n")); err == nil {
		t.Fatal("incomplete dump transaction wrapper should fail")
	}
}

func TestGlobalArchiveValidatesEveryPayloadChecksum(t *testing.T) {
	path := filepath.Join(t.TempDir(), "global.zip")
	files := map[string][]byte{
		"backend.env":                 []byte("DATABASE_URL=postgres://xui_test@127.0.0.1:5432/xui_test?sslmode=disable\nBACKEND_PANEL_SECRETS_KEY=AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=\n"),
		"systemd/xui-backend.service": []byte(backendUnit()),
	}
	hashes := map[string]string{}
	for name, body := range files {
		sum := sha256.Sum256(body)
		hashes[name] = hex.EncodeToString(sum[:])
	}
	dump := []byte("portable test database archive")
	dbHash := sha256.Sum256(dump)
	manifest := Manifest{Format: archiveFormat, Scope: "global-system", CreatedAt: time.Now().UTC(), DatabaseSHA256: hex.EncodeToString(dbHash[:]), Files: hashes}
	if err := writeArchive(path, manifest, dump, files); err != nil {
		t.Fatal(err)
	}
	got, contents, db, err := readArchive(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Scope != "global-system" || !bytes.Equal(contents["backend.env"], files["backend.env"]) || !bytes.Equal(db, dump) {
		t.Fatal("validated archive content did not round-trip")
	}

	corruptArchivePayload(t, path, "backend.env")
	if _, _, _, err = readArchive(path); err == nil {
		t.Fatal("expected corrupted config checksum to be rejected")
	}
}

func TestGlobalRestorePreflightAcceptsFreshMigratedDatabase(t *testing.T) {
	dbURL := os.Getenv("XUI_GLOBALBACKUP_TEST_DATABASE_URL")
	if dbURL == "" {
		t.Skip("set XUI_GLOBALBACKUP_TEST_DATABASE_URL to an isolated disposable PostgreSQL 16 database")
	}
	pool, err := pgxpool.New(context.Background(), dbURL)
	if err != nil {
		t.Fatal(err)
	}
	repo := &store.Store{DB: pool}
	if err = repo.Migrate(context.Background()); err != nil {
		pool.Close()
		t.Fatal(err)
	}
	pool.Close()
	mode, err := inspectTargetDatabase(context.Background(), dbURL)
	if err != nil {
		t.Fatal(err)
	}
	if mode != "pristine-install" {
		t.Fatalf("fresh installer schema classified as %q", mode)
	}
	if _, err = exec.LookPath("psql"); err != nil {
		t.Skip("fresh install classified; psql is unavailable for transactional archive preflight")
	}
	root := t.TempDir()
	archive := filepath.Join(root, "global.zip")
	files := map[string][]byte{"backend.env": []byte("DATABASE_URL=postgres://ignored@localhost/ignored\nBACKEND_LISTEN_ADDR=127.0.0.1:8088\nBACKEND_PANEL_SECRETS_KEY=AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=\n"), "systemd/xui-backend.service": []byte(backendUnit())}
	hashes := map[string]string{}
	for name, data := range files {
		sum := sha256.Sum256(data)
		hashes[name] = hex.EncodeToString(sum[:])
	}
	dump, err := snapshotTargetDatabase(context.Background(), dbURL)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(dump)
	pool, err = pgxpool.New(context.Background(), dbURL)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := pool.Begin(context.Background())
	if err != nil {
		pool.Close()
		t.Fatal(err)
	}
	databaseOnly, err := validateDatabaseRuntimeClosure(context.Background(), tx, nil)
	_ = tx.Rollback(context.Background())
	pool.Close()
	if err != nil {
		t.Fatal(err)
	}
	manifest := Manifest{Format: archiveFormat, Scope: "global-system", CreatedAt: time.Now().UTC(), DatabaseSHA256: hex.EncodeToString(sum[:]), Files: hashes, DatabaseOnlyDeployments: databaseOnly}
	if err = writeArchive(archive, manifest, dump, files); err != nil {
		t.Fatal(err)
	}
	backendEnvPath := filepath.Join(root, "backend.env")
	if err = os.WriteFile(backendEnvPath, []byte("DATABASE_URL=target-database\n"), 0600); err != nil {
		t.Fatal(err)
	}
	plan, err := Preflight(context.Background(), RestoreOptions{DatabaseURL: dbURL, Archive: archive, BackendEnvPath: backendEnvPath, InstanceRoot: filepath.Join(root, "instances"), UnitRoot: filepath.Join(root, "units"), DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if plan.DatabaseMode != "pristine-install" {
		t.Fatalf("unexpected restore mode %q", plan.DatabaseMode)
	}
	if !reflect.DeepEqual(plan.DatabaseOnlyDeployments, databaseOnly) {
		t.Fatalf("dry-run database-only deployments = %v, want %v", plan.DatabaseOnlyDeployments, databaseOnly)
	}
	mode, err = inspectTargetDatabase(context.Background(), dbURL)
	if err != nil || mode != "pristine-install" {
		t.Fatalf("dry-run changed the target database: mode=%q err=%v", mode, err)
	}
	checkpoint, err := snapshotTargetDatabase(context.Background(), dbURL)
	if err != nil {
		t.Fatal(err)
	}
	instanceRoot := filepath.Join(root, "instances")
	unitRoot := filepath.Join(root, "units")
	if err = os.MkdirAll(instanceRoot, 0700); err != nil {
		t.Fatal(err)
	}
	stage, err := os.MkdirTemp(root, ".global-restore-")
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Chmod(stage, 0700); err != nil {
		t.Fatal(err)
	}
	restoreOpts := RestoreOptions{DatabaseURL: dbURL, Archive: archive, BackendEnvPath: backendEnvPath, InstanceRoot: instanceRoot, UnitRoot: unitRoot}
	priorBackend := []byte("DATABASE_URL=target-database\n")
	if err = saveRecoveryCheckpoint(stage, restoreOpts, manifest, checkpoint, priorBackend, true); err != nil {
		t.Fatal(err)
	}
	mutator, err := pgxpool.New(context.Background(), dbURL)
	if err != nil {
		t.Fatal(err)
	}
	_, err = mutator.Exec(context.Background(), `CREATE TABLE public.global_restore_rollback_probe(id INTEGER)`)
	mutator.Close()
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(backendEnvPath, []byte("DATABASE_URL=overwritten\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = Recover(context.Background(), RecoverOptions{DatabaseURL: dbURL, Archive: archive, Stage: stage, BackendEnvPath: backendEnvPath, InstanceRoot: instanceRoot, UnitRoot: unitRoot}); err != nil {
		t.Fatalf("durable checkpoint retry path failed: %v", err)
	}
	mode, err = inspectTargetDatabase(context.Background(), dbURL)
	if err != nil || mode != "pristine-install" {
		t.Fatalf("checkpoint rollback did not restore retryable fresh target: mode=%q err=%v", mode, err)
	}
	gotBackend, err := os.ReadFile(backendEnvPath)
	if err != nil || !bytes.Equal(gotBackend, priorBackend) {
		t.Fatalf("target backend config checkpoint was not restored: %q err=%v", gotBackend, err)
	}
	if _, err = os.Stat(stage); !os.IsNotExist(err) {
		t.Fatalf("successful recovery did not remove its protected stage: %v", err)
	}
}

func TestGlobalBackupRegistryLockBlocksConcurrentRegistrationWrites(t *testing.T) {
	dbURL := os.Getenv("XUI_GLOBALBACKUP_TEST_DATABASE_URL")
	if dbURL == "" {
		t.Skip("set XUI_GLOBALBACKUP_TEST_DATABASE_URL to an isolated disposable PostgreSQL 16 database")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err = (&store.Store{DB: pool}).Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	registryTx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer registryTx.Rollback(context.Background())
	if err = lockRuntimeRegistry(ctx, registryTx); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, execErr := pool.Exec(ctx, `UPDATE deployments SET enabled=enabled WHERE id='retail-finland'`)
		done <- execErr
	}()
	select {
	case err = <-done:
		t.Fatalf("registration-like deployment write was not blocked by backup lock: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	if err = registryTx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err = <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("registration-like deployment write did not resume after backup lock released")
	}
}

func corruptArchivePayload(t *testing.T, path, name string) {
	t.Helper()
	reader, err := zip.OpenReader(path)
	if err != nil {
		t.Fatal(err)
	}
	manifest := Manifest{}
	var dump []byte
	files := map[string][]byte{}
	for _, f := range reader.File {
		r, e := f.Open()
		if e != nil {
			reader.Close()
			t.Fatal(e)
		}
		var b bytes.Buffer
		_, e = b.ReadFrom(r)
		r.Close()
		if e != nil {
			reader.Close()
			t.Fatal(e)
		}
		switch f.Name {
		case manifestName:
			if e = json.Unmarshal(b.Bytes(), &manifest); e != nil {
				reader.Close()
				t.Fatal(e)
			}
		case "database.sql":
			dump = append([]byte(nil), b.Bytes()...)
		default:
			files[f.Name] = append([]byte(nil), b.Bytes()...)
		}
	}
	reader.Close()
	files[name] = append(files[name], []byte("tamper")...)
	path2 := path + ".bad"
	if err = writeArchive(path2, manifest, dump, files); err != nil {
		t.Fatal(err)
	}
	// Make the bad archive discoverable by the caller without mutating the good one.
	if err = os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err = os.Rename(path2, path); err != nil {
		t.Fatal(err)
	}
}
