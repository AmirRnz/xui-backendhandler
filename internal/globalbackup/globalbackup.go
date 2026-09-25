// Package globalbackup creates portable whole-system archives. A global
// archive contains the complete PostgreSQL database, backend and instance
// configuration, and the canonical runtime service definitions.
package globalbackup

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/user"
	pathpkg "path"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"example.com/xui-commerce/backend/internal/secrets"
	"example.com/xui-commerce/backend/internal/store"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	manifestName  = "manifest.json"
	archiveFormat = 1
	maxEntryBytes = 2 << 30
)

type Manifest struct {
	Format                  int               `json:"format"`
	Scope                   string            `json:"scope"`
	CreatedAt               time.Time         `json:"created_at"`
	DatabaseSHA256          string            `json:"database_sha256"`
	Files                   map[string]string `json:"files_sha256"`
	Instances               []InstanceState   `json:"instances"`
	BackendEnabled          bool              `json:"backend_enabled"`
	BackendActive           bool              `json:"backend_active"`
	DatabaseOnlyDeployments []string          `json:"database_only_deployments,omitempty"`
}

type InstanceState struct {
	Slug    string `json:"slug"`
	Enabled bool   `json:"enabled"`
	Active  bool   `json:"active"`
}

type CreateOptions struct {
	DatabaseURL    string
	BackendEnvPath string
	InstanceRoot   string
	UnitRoot       string
	Destination    string
	QuiesceForMove bool
}

type RestoreOptions struct {
	DatabaseURL    string
	Archive        string
	BackendEnvPath string
	InstanceRoot   string
	UnitRoot       string
	DryRun         bool
	Activate       bool
	ListenAddr     string
}

type RecoverOptions struct {
	DatabaseURL    string
	Archive        string
	Stage          string
	BackendEnvPath string
	InstanceRoot   string
	UnitRoot       string
}

type recoveryManifest struct {
	Format            int      `json:"format"`
	DatabaseURLSHA256 string   `json:"database_url_sha256"`
	ArchiveSHA256     string   `json:"archive_sha256"`
	CheckpointSHA256  string   `json:"checkpoint_sha256"`
	BackendEnvSHA256  string   `json:"backend_env_sha256,omitempty"`
	HadBackendEnv     bool     `json:"had_backend_env"`
	Instances         []string `json:"instances"`
}

type RestorePlan struct {
	Instances               []string
	ListenAddr              string
	DatabaseEmpty           bool
	DatabaseMode            string
	WouldActivate           bool
	DatabaseOnlyDeployments []string
}

// Create writes a protected, checksummed portable global archive. When
// QuiesceForMove is true, currently active local units are stopped before the
// snapshot and remain stopped after success. They are restarted if backup fails.
func Create(ctx context.Context, o CreateOptions) error {
	if o.DatabaseURL == "" || o.BackendEnvPath == "" || o.InstanceRoot == "" || o.UnitRoot == "" || o.Destination == "" {
		return errors.New("global backup requires database, backend config, instance registry, unit directory, and archive path")
	}
	if _, err := os.Lstat(o.Destination); err == nil {
		return errors.New("backup destination already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	backend, err := readPrivateFile(o.BackendEnvPath)
	if err != nil {
		return err
	}
	backendValues, err := parseEnv(backend)
	if err != nil {
		return fmt.Errorf("invalid backend environment file: %w", err)
	}
	if _, err = secrets.ParseKey(backendValues["BACKEND_PANEL_SECRETS_KEY"]); err != nil {
		return errors.New("backend environment must contain the original valid secret encryption key")
	}
	if backendValues["DATABASE_URL"] == "" {
		return errors.New("backend environment is missing DATABASE_URL")
	}
	if strings.TrimSpace(backendValues["DATABASE_URL"]) != strings.TrimSpace(o.DatabaseURL) {
		return errors.New("global backup database URL must match backend.env")
	}

	pool, err := pgxpool.New(ctx, o.DatabaseURL)
	if err != nil {
		return errors.New("cannot connect to source database for a stable runtime snapshot")
	}
	defer pool.Close()
	if err = pool.Ping(ctx); err != nil {
		return errors.New("cannot reach source database for a stable runtime snapshot")
	}
	registryTx, err := pool.Begin(ctx)
	if err != nil {
		return errors.New("cannot start stable runtime snapshot")
	}
	defer registryTx.Rollback(context.Background())
	if err = lockRuntimeRegistry(ctx, registryTx); err != nil {
		return errors.New("could not lock instance registration while making global backup")
	}

	configs, slugs, err := readInstances(o.InstanceRoot)
	if err != nil {
		return err
	}
	if err = verifyUnitRegistryClosure(o.UnitRoot, slugs); err != nil {
		return err
	}
	databaseOnly, err := validateDatabaseRuntimeClosure(ctx, registryTx, configs)
	if err != nil {
		return err
	}
	unitData := map[string][]byte{"systemd/xui-backend.service": []byte(backendUnit())}
	states := make([]InstanceState, 0, len(slugs))
	var stopped, disabled []string
	leaveStopped := false
	defer func() {
		if o.QuiesceForMove && !leaveStopped {
			startUnits, enableUnits := sourceRollbackActions(stopped, disabled)
			for _, name := range enableUnits {
				_ = systemctl(context.Background(), "enable", name)
			}
			for _, name := range startUnits {
				_ = systemctl(context.Background(), "start", name)
			}
		}
	}()
	state, err := unitState(ctx, "xui-backend.service", o.UnitRoot, unitData["systemd/xui-backend.service"])
	if err != nil {
		return err
	}
	for _, slug := range slugs {
		name := instanceUnitName(slug)
		content := instanceUnit(slug)
		unitData["systemd/"+name] = []byte(content)
		s, err := unitState(ctx, name, o.UnitRoot, []byte(content))
		if err != nil {
			return err
		}
		states = append(states, InstanceState{Slug: slug, Enabled: s.Enabled, Active: s.Active})
	}
	if o.QuiesceForMove {
		stopUnits, disableUnits := sourceMoveActions(state, states)
		for _, name := range stopUnits {
			if err := systemctl(ctx, "stop", name); err != nil {
				return fmt.Errorf("could not stop source unit %s for move backup", name)
			}
			stopped = append(stopped, name)
		}
		for _, name := range disableUnits {
			if err := systemctl(ctx, "disable", name); err != nil {
				return fmt.Errorf("could not disable source unit %s for move backup", name)
			}
			disabled = append(disabled, name)
		}
	}
	files := map[string][]byte{"backend.env": backend}
	for name, data := range configs {
		files[name] = data
	}
	if err = validateImportedDatabase(ctx, o.DatabaseURL, files, Manifest{Instances: states}); err != nil {
		return fmt.Errorf("source database and runtime credentials do not match; backup refused: %w", err)
	}

	conn, env, err := pgEnvironment(o.DatabaseURL)
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, "pg_dump", conn, "--no-owner", "--no-privileges", "--format=plain")
	cmd.Env = append(os.Environ(), env...)
	dump, err := cmd.Output()
	if err != nil {
		return errors.New("database backup failed")
	}
	if err = registryTx.Commit(ctx); err != nil {
		return errors.New("could not complete stable database/runtime snapshot")
	}
	for name, data := range unitData {
		files[name] = data
	}
	fileHashes := map[string]string{}
	for name, data := range files {
		sum := sha256.Sum256(data)
		fileHashes[name] = hex.EncodeToString(sum[:])
	}
	dbSum := sha256.Sum256(dump)
	manifest := Manifest{Format: archiveFormat, Scope: "global-system", CreatedAt: time.Now().UTC(), DatabaseSHA256: hex.EncodeToString(dbSum[:]), Files: fileHashes, Instances: states, BackendEnabled: state.Enabled, BackendActive: state.Active, DatabaseOnlyDeployments: databaseOnly}
	if err = writeArchive(o.Destination, manifest, dump, files); err != nil {
		return err
	}
	if len(databaseOnly) > 0 {
		fmt.Printf("Global backup retained database-only bootstrap/inert deployments (no configured bot credentials): %s\n", strings.Join(databaseOnly, ", "))
	}
	if o.QuiesceForMove {
		leaveStopped = true
	}
	return nil
}

func sourceMoveActions(backend unitStatus, instances []InstanceState) (stop, disable []string) {
	for _, instance := range instances {
		name := instanceUnitName(instance.Slug)
		if instance.Active {
			stop = append(stop, name)
		}
		if instance.Enabled {
			disable = append(disable, name)
		}
	}
	if backend.Active {
		stop = append(stop, "xui-backend.service")
	}
	if backend.Enabled {
		disable = append(disable, "xui-backend.service")
	}
	return stop, disable
}

func sourceRollbackActions(stopped, disabled []string) (start, enable []string) {
	for i := len(stopped) - 1; i >= 0; i-- {
		start = append(start, stopped[i])
	}
	for i := len(disabled) - 1; i >= 0; i-- {
		enable = append(enable, disabled[i])
	}
	return start, enable
}

type runtimeDeploymentRow struct {
	ID                    string
	HasTelegramCredential bool
	HasBackendCredential  bool
}

func lockRuntimeRegistry(ctx context.Context, tx pgx.Tx) error {
	_, err := tx.Exec(ctx, `LOCK TABLE deployments,backend_client_credentials IN SHARE MODE`)
	return err
}

func validateDatabaseRuntimeClosure(ctx context.Context, tx pgx.Tx, configs map[string][]byte) ([]string, error) {
	rows, err := tx.Query(ctx, `SELECT d.id,d.encrypted_telegram_token IS NOT NULL,EXISTS(SELECT 1 FROM backend_client_credentials c WHERE c.deployment_id=d.id AND c.enabled) FROM deployments d ORDER BY d.id`)
	if err != nil {
		return nil, errors.New("could not enumerate registered deployments for global backup")
	}
	defer rows.Close()
	databaseRows := []runtimeDeploymentRow{}
	databaseIDs := map[string]runtimeDeploymentRow{}
	for rows.Next() {
		var row runtimeDeploymentRow
		if err = rows.Scan(&row.ID, &row.HasTelegramCredential, &row.HasBackendCredential); err != nil {
			return nil, errors.New("could not inspect deployment runtime credentials")
		}
		databaseRows = append(databaseRows, row)
		databaseIDs[row.ID] = row
	}
	if err = rows.Err(); err != nil {
		return nil, errors.New("could not inspect deployment runtime credentials")
	}
	configByDeployment := map[string]string{}
	for name, body := range configs {
		if !strings.HasPrefix(name, "instances/") || !strings.HasSuffix(name, "/instance.env") {
			continue
		}
		values, parseErr := parseEnv(body)
		if parseErr != nil {
			return nil, fmt.Errorf("runtime config %s is invalid", pathpkg.Base(filepath.Dir(name)))
		}
		deployment := strings.TrimSpace(values["BACKEND_DEPLOYMENT_ID"])
		if deployment == "" {
			return nil, fmt.Errorf("runtime config %s has no deployment identity", pathpkg.Base(filepath.Dir(name)))
		}
		if _, exists := databaseIDs[deployment]; !exists {
			return nil, fmt.Errorf("runtime config %s refers to a deployment missing from the database", pathpkg.Base(filepath.Dir(name)))
		}
		if existing, exists := configByDeployment[deployment]; exists {
			return nil, fmt.Errorf("deployments %s and %s both claim the same database runtime", existing, pathpkg.Base(filepath.Dir(name)))
		}
		configByDeployment[deployment] = pathpkg.Base(filepath.Dir(name))
	}
	return validateRuntimeDeploymentRows(databaseRows, configByDeployment)
}

func validateRuntimeDeploymentRows(databaseRows []runtimeDeploymentRow, configByDeployment map[string]string) ([]string, error) {
	databaseOnly := []string{}
	databaseIDs := map[string]bool{}
	for _, row := range databaseRows {
		databaseIDs[row.ID] = true
	}
	for deployment, slug := range configByDeployment {
		if !databaseIDs[deployment] {
			return nil, fmt.Errorf("runtime config %s refers to a deployment missing from the database", slug)
		}
	}
	for _, row := range databaseRows {
		if row.HasTelegramCredential != row.HasBackendCredential {
			return nil, fmt.Errorf("deployment %s has only part of its bot runtime credentials; refusing an incomplete global archive", row.ID)
		}
		_, configured := configByDeployment[row.ID]
		if row.HasTelegramCredential && !configured {
			return nil, fmt.Errorf("deployment %s has usable bot credentials but no registered instance config/unit", row.ID)
		}
		if configured && !row.HasTelegramCredential {
			return nil, fmt.Errorf("instance config %s has no complete matching database runtime credentials", configByDeployment[row.ID])
		}
		if !row.HasTelegramCredential {
			databaseOnly = append(databaseOnly, row.ID)
		}
	}
	sort.Strings(databaseOnly)
	return databaseOnly, nil
}

func verifyUnitRegistryClosure(unitRoot string, slugs []string) error {
	entries, err := os.ReadDir(unitRoot)
	if errors.Is(err, os.ErrNotExist) {
		if len(slugs) == 0 {
			return nil
		}
		return errors.New("instance units directory is missing")
	}
	if err != nil {
		return err
	}
	expected := map[string]bool{}
	for _, slug := range slugs {
		expected[instanceUnitName(slug)] = true
	}
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, "xui-backend-instance-") || !strings.HasSuffix(name, ".service") {
			continue
		}
		if entry.Type()&os.ModeSymlink != 0 || !entry.Type().IsRegular() {
			return fmt.Errorf("instance unit %s is not a regular file", name)
		}
		if !expected[name] {
			return fmt.Errorf("instance unit %s has no matching registered instance config", name)
		}
	}
	for name := range expected {
		info, err := os.Lstat(filepath.Join(unitRoot, name))
		if err != nil {
			return fmt.Errorf("registered instance unit %s is missing", name)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("registered instance unit %s is not a regular file", name)
		}
	}
	return nil
}

// Preflight validates the archive and destination without modifying either.
// It refuses a non-empty database and chooses an available loopback port.
func Preflight(ctx context.Context, o RestoreOptions) (RestorePlan, error) {
	m, files, dump, err := readArchive(o.Archive)
	if err != nil {
		return RestorePlan{}, err
	}
	if m.Scope != "global-system" {
		return RestorePlan{}, errors.New("archive is not a portable global system backup")
	}
	for _, s := range m.Instances {
		if s.Active && !m.BackendActive {
			return RestorePlan{}, errors.New("archive has an active bot but inactive backend service")
		}
	}
	backendValues, err := parseEnv(files["backend.env"])
	if err != nil {
		return RestorePlan{}, errors.New("archived backend configuration is invalid")
	}
	if _, err = secrets.ParseKey(backendValues["BACKEND_PANEL_SECRETS_KEY"]); err != nil {
		return RestorePlan{}, errors.New("archive is missing a usable secret encryption key")
	}
	addr := o.ListenAddr
	if addr == "" {
		addr = backendValues["BACKEND_LISTEN_ADDR"]
	}
	addr, err = availableLoopbackAddr(addr)
	if err != nil {
		return RestorePlan{}, err
	}
	if err = validateTargetPaths(o); err != nil {
		return RestorePlan{}, err
	}
	if err = ensureTargetUnitsInactive(ctx, o.UnitRoot); err != nil {
		return RestorePlan{}, err
	}
	if err = ensureUnitAvailable(filepath.Join(o.UnitRoot, "xui-backend.service"), []byte(backendUnit())); err != nil {
		return RestorePlan{}, err
	}
	for _, s := range m.Instances {
		if active := systemctlQuery(ctx, "is-active", instanceUnitName(s.Slug)); active == "active" {
			return RestorePlan{}, fmt.Errorf("target instance service %s is active; stop target services before restoring", s.Slug)
		}
		if err = ensureUnitAvailable(filepath.Join(o.UnitRoot, instanceUnitName(s.Slug)), []byte(instanceUnit(s.Slug))); err != nil {
			return RestorePlan{}, err
		}
	}
	mode, err := inspectTargetDatabase(ctx, o.DatabaseURL)
	if err != nil {
		return RestorePlan{}, err
	}
	probe, err := normalizeDatabaseDumpTransactions(dump)
	if err != nil {
		return RestorePlan{}, err
	}
	if mode == "pristine-install" {
		probe = append([]byte(pristineResetPreamble()), probe...)
	}
	probe = append(probe, []byte("\nSET search_path = public;\n")...)
	probe = append(probe, credentialPreflightSQL(files, m)...)
	probe = append(probe, databaseClosurePreflightSQL(files, m)...)
	probe = append(probe, []byte("\nROLLBACK;\n")...)
	conn, env, err := pgEnvironment(o.DatabaseURL)
	if err != nil {
		return RestorePlan{}, err
	}
	check := exec.CommandContext(ctx, "psql", conn, "-X", "-v", "ON_ERROR_STOP=1", "--single-transaction")
	check.Env = append(os.Environ(), env...)
	check.Stdin = bytes.NewReader(probe)
	check.Stdout = os.Stderr
	check.Stderr = os.Stderr
	if err = check.Run(); err != nil {
		return RestorePlan{}, errors.New("database archive failed PostgreSQL restore preflight; target was rolled back")
	}
	instances := make([]string, 0, len(m.Instances))
	for _, instance := range m.Instances {
		instances = append(instances, instance.Slug)
	}
	sort.Strings(instances)
	return RestorePlan{Instances: instances, DatabaseOnlyDeployments: append([]string(nil), m.DatabaseOnlyDeployments...), ListenAddr: addr, DatabaseEmpty: mode == "empty", DatabaseMode: mode, WouldActivate: o.Activate}, nil
}

func ensureTargetUnitsInactive(ctx context.Context, unitRoot string) error {
	entries, err := os.ReadDir(unitRoot)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		name := entry.Name()
		if name != "xui-backend.service" && !(strings.HasPrefix(name, "xui-backend-instance-") && strings.HasSuffix(name, ".service")) {
			continue
		}
		if systemctlQuery(ctx, "is-active", name) == "active" {
			return fmt.Errorf("target service %s is active; stop target xui-backend services before restoring", name)
		}
		if systemctlQuery(ctx, "is-enabled", name) == "enabled" {
			return fmt.Errorf("target service %s is enabled; disable target xui-backend services before restoring", name)
		}
	}
	return nil
}

func credentialPreflightSQL(files map[string][]byte, m Manifest) []byte {
	var b strings.Builder
	for _, state := range m.Instances {
		cfg, err := parseEnv(files["instances/"+state.Slug+"/instance.env"])
		if err != nil {
			continue
		}
		deployment := sqlLiteral(cfg["BACKEND_DEPLOYMENT_ID"])
		channel := sqlLiteral(cfg["CHANNEL"])
		digest := sha256.Sum256([]byte(strings.TrimSpace(cfg["BACKEND_TOKEN"])))
		fmt.Fprintf(&b, `DO $$ BEGIN
 IF NOT EXISTS (SELECT 1 FROM deployments WHERE id=%s AND channel=%s AND enabled AND bot_instance_active AND encrypted_telegram_token IS NOT NULL)
 THEN RAISE EXCEPTION 'restored deployment configuration mismatch'; END IF;
 IF NOT EXISTS (SELECT 1 FROM backend_client_credentials WHERE deployment_id=%s AND enabled AND encode(token_hash,'hex')='%s')
 THEN RAISE EXCEPTION 'restored backend credential mismatch'; END IF;
END $$;
`, deployment, channel, deployment, hex.EncodeToString(digest[:]))
	}
	return []byte(b.String())
}

func databaseClosurePreflightSQL(files map[string][]byte, m Manifest) []byte {
	configured := []string{}
	for _, state := range m.Instances {
		values, err := parseEnv(files["instances/"+state.Slug+"/instance.env"])
		if err == nil && strings.TrimSpace(values["BACKEND_DEPLOYMENT_ID"]) != "" {
			configured = append(configured, strings.TrimSpace(values["BACKEND_DEPLOYMENT_ID"]))
		}
	}
	return []byte(fmt.Sprintf(`DO $$ BEGIN
 IF EXISTS (SELECT 1 FROM deployments d WHERE (d.encrypted_telegram_token IS NOT NULL) <> EXISTS(SELECT 1 FROM backend_client_credentials c WHERE c.deployment_id=d.id AND c.enabled))
 THEN RAISE EXCEPTION 'incomplete deployment runtime credentials'; END IF;
 IF EXISTS (SELECT 1 FROM deployments d WHERE d.encrypted_telegram_token IS NOT NULL AND d.id NOT IN %s)
 THEN RAISE EXCEPTION 'database runtime has no matching archived instance config'; END IF;
 IF EXISTS (SELECT 1 FROM deployments d WHERE d.encrypted_telegram_token IS NULL AND d.id NOT IN %s)
 THEN RAISE EXCEPTION 'database-only deployment manifest is incomplete'; END IF;
END $$;
`, sqlInList(configured), sqlInList(m.DatabaseOnlyDeployments)))
}

func sqlInList(values []string) string {
	if len(values) == 0 {
		return "('')"
	}
	quoted := make([]string, 0, len(values))
	for _, value := range values {
		quoted = append(quoted, sqlLiteral(value))
	}
	return "(" + strings.Join(quoted, ",") + ")"
}

func sqlLiteral(value string) string { return "'" + strings.ReplaceAll(value, "'", "''") + "'" }

// Restore imports into an empty target database and installs reviewed config
// and canonical units. Activation is optional and occurs only after the
// database and all configuration files have been validated and installed.
func Restore(ctx context.Context, o RestoreOptions) (RestorePlan, error) {
	plan, err := Preflight(ctx, o)
	if err != nil {
		return RestorePlan{}, err
	}
	if o.DryRun {
		return plan, nil
	}
	uid, gid, err := serviceIDs()
	if err != nil {
		return RestorePlan{}, err
	}
	m, files, dump, err := readArchive(o.Archive)
	if err != nil {
		return RestorePlan{}, err
	}
	backendValues, err := parseEnv(files["backend.env"])
	if err != nil {
		return RestorePlan{}, errors.New("archived backend configuration is invalid")
	}
	backendValues["DATABASE_URL"] = o.DatabaseURL
	backendValues["BACKEND_LISTEN_ADDR"] = plan.ListenAddr
	backend := formatEnv(backendValues)
	instanceFiles := map[string][]byte{}
	for _, state := range m.Instances {
		name := "instances/" + state.Slug + "/instance.env"
		values, e := parseEnv(files[name])
		if e != nil {
			return RestorePlan{}, fmt.Errorf("archived instance %s configuration is invalid", state.Slug)
		}
		values["BACKEND_URL"] = "http://" + plan.ListenAddr
		instanceFiles[name] = formatEnv(values)
		if meta, ok := files["instances/"+state.Slug+"/metadata.json"]; ok {
			var metadata map[string]any
			if e := json.Unmarshal(meta, &metadata); e != nil {
				return RestorePlan{}, fmt.Errorf("instance %s metadata is invalid", state.Slug)
			}
			metadata["backend_url"] = "http://" + plan.ListenAddr
			updated, e := json.MarshalIndent(metadata, "", "  ")
			if e != nil {
				return RestorePlan{}, fmt.Errorf("instance %s metadata is invalid", state.Slug)
			}
			instanceFiles["instances/"+state.Slug+"/metadata.json"] = updated
		}
	}
	stage, err := stageFiles(o, backend, instanceFiles, m)
	if err != nil {
		return RestorePlan{}, err
	}
	preserveStage := false
	defer func() {
		if !preserveStage {
			_ = os.RemoveAll(stage)
		}
	}()
	previousBackend, hadBackend, err := readOptionalRegularFile(o.BackendEnvPath)
	if err != nil {
		return RestorePlan{}, err
	}
	checkpoint, err := snapshotTargetDatabase(ctx, o.DatabaseURL)
	if err != nil {
		return RestorePlan{}, fmt.Errorf("could not checkpoint target database before import: %w", err)
	}
	if err = saveRecoveryCheckpoint(stage, o, m, checkpoint, previousBackend, hadBackend); err != nil {
		return RestorePlan{}, fmt.Errorf("could not persist protected recovery checkpoint before import: %w", err)
	}
	rollback := func(cause error) (RestorePlan, error) {
		dbErr := rollbackTargetDatabase(context.Background(), o.DatabaseURL, checkpoint)
		fileErr := restoreTargetFiles(o, m, previousBackend, hadBackend)
		if dbErr != nil || fileErr != nil {
			preserveStage = true
			return RestorePlan{}, fmt.Errorf("restore failed (%v); automatic rollback incomplete (database: %v; runtime files: %v); protected checkpoint and stage retained at %s", cause, dbErr, fileErr, stage)
		}
		return RestorePlan{}, fmt.Errorf("restore failed and target database/files were reset to their pre-restore checkpoint; retry is safe: %w", cause)
	}

	conn, env, err := pgEnvironment(o.DatabaseURL)
	if err != nil {
		return RestorePlan{}, err
	}
	cmd := exec.CommandContext(ctx, "psql", conn, "-X", "-v", "ON_ERROR_STOP=1", "--single-transaction")
	cmd.Env = append(os.Environ(), env...)
	databaseDump, err := normalizeDatabaseDumpTransactions(dump)
	if err != nil {
		return RestorePlan{}, err
	}
	if plan.DatabaseMode == "pristine-install" {
		databaseDump = append([]byte(pristineResetPreamble()), databaseDump...)
	}
	cmd.Stdin = bytes.NewReader(databaseDump)
	if err = cmd.Run(); err != nil {
		return RestorePlan{}, errors.New("database restore failed; inspect PostgreSQL logs without exposing credentials")
	}
	preserveStage = true
	if err = migrateAndQuarantine(ctx, o.DatabaseURL); err != nil {
		return rollback(fmt.Errorf("recovery queues could not be quarantined: %w", err))
	}
	if err = validateImportedDatabase(ctx, o.DatabaseURL, files, m); err != nil {
		return rollback(fmt.Errorf("imported credentials or instance identity failed validation: %w", err))
	}
	if err = installStagedFiles(stage, o, m, uid, gid); err != nil {
		return rollback(fmt.Errorf("runtime config installation failed: %w", err))
	}
	preserveStage = false
	if !o.Activate {
		return plan, nil
	}
	if err = activate(ctx, o, plan, m, instanceFiles); err != nil {
		return rollback(fmt.Errorf("activation failed: %w", err))
	}
	return plan, nil
}

func activate(ctx context.Context, o RestoreOptions, p RestorePlan, m Manifest, instanceFiles map[string][]byte) (retErr error) {
	started, enabled := []string{}, []string{}
	defer func() {
		if retErr == nil {
			return
		}
		for i := len(enabled) - 1; i >= 0; i-- {
			_ = systemctl(context.Background(), "disable", enabled[i])
		}
		stopStarted(started)
		for _, unit := range activationTargets(m) {
			_ = systemctl(context.Background(), "stop", unit)
		}
	}()
	if err := systemctl(ctx, "daemon-reload"); err != nil {
		return errors.New("systemd could not reload restored service definitions")
	}
	if m.BackendActive {
		if err := systemctl(ctx, "start", "xui-backend.service"); err != nil {
			return errors.New("restored backend did not start")
		}
		started = append(started, "xui-backend.service")
		if err := verifyServiceRemainsActive(ctx, "xui-backend.service", func(unit string) string { return systemctlQuery(ctx, "is-active", unit) }); err != nil {
			return errors.New("restored backend did not remain active")
		}
	}
	if m.BackendActive {
		client := &http.Client{Timeout: 4 * time.Second}
		deadline := time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) {
			resp, err := client.Get("http://" + p.ListenAddr + "/healthz")
			if err == nil {
				resp.Body.Close()
				if resp.StatusCode == http.StatusOK {
					break
				}
			}
			time.Sleep(500 * time.Millisecond)
		}
		resp, err := client.Get("http://" + p.ListenAddr + "/healthz")
		if err != nil {
			stopStarted(started)
			return errors.New("restored backend health check failed; started services were stopped")
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			stopStarted(started)
			return errors.New("restored backend health check failed; started services were stopped")
		}
		if err := checkScopedBackendIdentity(ctx, p.ListenAddr, instanceFiles, m); err != nil {
			stopStarted(started)
			return fmt.Errorf("restored backend scoped authentication check failed; started services were stopped: %w", err)
		}
	}
	for _, name := range activationTargets(m) {
		if name == "xui-backend.service" {
			continue
		}
		if err := systemctl(ctx, "start", name); err != nil {
			stopStarted(started)
			return fmt.Errorf("restored instance service %s did not start; started services were stopped", name)
		}
		started = append(started, name)
		if err := verifyServiceRemainsActive(ctx, name, func(unit string) string { return systemctlQuery(ctx, "is-active", unit) }); err != nil {
			stopStarted(started)
			return fmt.Errorf("restored instance service %s did not remain active; started services were stopped", name)
		}
	}
	if m.BackendEnabled {
		if err := systemctl(ctx, "enable", "xui-backend.service"); err != nil {
			return errors.New("services started but backend boot enablement failed")
		}
		enabled = append(enabled, "xui-backend.service")
	}
	for _, state := range m.Instances {
		if state.Enabled {
			if err := systemctl(ctx, "enable", instanceUnitName(state.Slug)); err != nil {
				return fmt.Errorf("services started but boot enablement failed for %s", state.Slug)
			}
			enabled = append(enabled, instanceUnitName(state.Slug))
		}
	}
	return nil
}

func verifyServiceRemainsActive(ctx context.Context, name string, query func(string) string) error {
	for check := 0; check < 3; check++ {
		if query(name) != "active" {
			return errors.New("service is not active")
		}
		if check < 2 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(500 * time.Millisecond):
			}
		}
	}
	return nil
}

func checkScopedBackendIdentity(ctx context.Context, listenAddr string, instanceFiles map[string][]byte, m Manifest) error {
	for _, instance := range m.Instances {
		config, err := parseEnv(instanceFiles["instances/"+instance.Slug+"/instance.env"])
		if err != nil {
			return errors.New("restored instance config cannot be read for authentication check")
		}
		adminID, err := strconv.ParseInt(config["ADMIN_TELEGRAM_ID"], 10, 64)
		if err != nil || adminID <= 0 || config["BACKEND_TOKEN"] == "" {
			return errors.New("restored instance authentication config is invalid")
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+listenAddr+"/v1/admin/config", nil)
		if err != nil {
			return errors.New("could not construct scoped backend authentication check")
		}
		request.Header.Set("Authorization", "Bearer "+strings.TrimSpace(config["BACKEND_TOKEN"]))
		request.Header.Set("X-Actor-Telegram-ID", strconv.FormatInt(adminID, 10))
		client := &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		response, err := client.Do(request)
		if err != nil {
			return errors.New("restored backend did not accept a registered scoped credential")
		}
		response.Body.Close()
		if response.StatusCode != http.StatusOK {
			return errors.New("restored backend rejected the registered scoped credential or administrator")
		}
	}
	return nil
}

func stopStarted(started []string) {
	for i := len(started) - 1; i >= 0; i-- {
		_ = systemctl(context.Background(), "stop", started[i])
	}
}

func activationTargets(m Manifest) []string {
	var targets []string
	if m.BackendActive {
		targets = append(targets, "xui-backend.service")
	}
	for _, instance := range m.Instances {
		if instance.Active {
			targets = append(targets, instanceUnitName(instance.Slug))
		}
	}
	return targets
}

type runtimeModes struct {
	Registry os.FileMode
	Instance os.FileMode
	Secret   os.FileMode
	Env      os.FileMode
	Unit     os.FileMode
}

func targetRuntimeModes() runtimeModes {
	return runtimeModes{Registry: 0710, Instance: 0700, Secret: 0600, Env: 0640, Unit: 0644}
}

func serviceIDs() (int, int, error) {
	if runtime.GOOS != "linux" {
		return 0, 0, nil
	}
	serviceUser, err := user.Lookup("xui-backend")
	if err != nil {
		return 0, 0, errors.New("target xui-backend service account is missing; run the installer before restore")
	}
	uid, err := strconv.Atoi(serviceUser.Uid)
	if err != nil {
		return 0, 0, errors.New("target xui-backend service UID is invalid")
	}
	gid, err := strconv.Atoi(serviceUser.Gid)
	if err != nil {
		return 0, 0, errors.New("target xui-backend service GID is invalid")
	}
	return uid, gid, nil
}

func readOptionalRegularFile(path string) ([]byte, bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if !info.Mode().IsRegular() {
		return nil, false, fmt.Errorf("target file %s must be a regular file", filepath.Base(path))
	}
	data, err := os.ReadFile(path)
	return data, true, err
}

func snapshotTargetDatabase(ctx context.Context, databaseURL string) ([]byte, error) {
	conn, env, err := pgEnvironment(databaseURL)
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, "pg_dump", conn, "--no-owner", "--no-privileges", "--format=plain")
	cmd.Env = append(os.Environ(), env...)
	dump, err := cmd.Output()
	if err != nil {
		return nil, errors.New("could not create pre-restore target database checkpoint")
	}
	return dump, nil
}

func rollbackTargetDatabase(ctx context.Context, databaseURL string, checkpoint []byte) error {
	conn, env, err := pgEnvironment(databaseURL)
	if err != nil {
		return err
	}
	checkpointDump, err := normalizeDatabaseDumpTransactions(checkpoint)
	if err != nil {
		return err
	}
	rollback := append([]byte("DROP SCHEMA public CASCADE;\nCREATE SCHEMA public AUTHORIZATION CURRENT_USER;\n"), checkpointDump...)
	cmd := exec.CommandContext(ctx, "psql", conn, "-X", "-v", "ON_ERROR_STOP=1", "--single-transaction")
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdin = bytes.NewReader(rollback)
	if err = cmd.Run(); err != nil {
		return errors.New("target database checkpoint could not be restored")
	}
	return nil
}

func normalizeDatabaseDumpTransactions(dump []byte) ([]byte, error) {
	lines := bytes.Split(dump, []byte("\n"))
	begin, commit := -1, -1
	for i, line := range lines {
		trimmed := strings.TrimSpace(string(line))
		if begin == -1 && trimmed == "BEGIN;" {
			begin = i
		}
		if trimmed == "COMMIT;" {
			commit = i
		}
	}
	if (begin == -1) != (commit == -1) {
		return nil, errors.New("database dump has an incomplete transaction wrapper")
	}
	if begin >= 0 {
		lines[begin] = nil
		lines[commit] = nil
	}
	return bytes.Join(lines, []byte("\n")), nil
}

func saveRecoveryCheckpoint(stage string, o RestoreOptions, m Manifest, checkpoint, previousBackend []byte, hadBackend bool) error {
	if err := writePrivateSync(filepath.Join(stage, "target-database.sql"), checkpoint); err != nil {
		return err
	}
	if hadBackend {
		if err := writePrivateSync(filepath.Join(stage, "target-backend.env"), previousBackend); err != nil {
			return err
		}
	}
	archive, err := os.ReadFile(o.Archive)
	if err != nil {
		return err
	}
	checkpointHash := sha256.Sum256(checkpoint)
	archiveHash := sha256.Sum256(archive)
	databaseHash := sha256.Sum256([]byte(o.DatabaseURL))
	state := recoveryManifest{Format: 1, DatabaseURLSHA256: hex.EncodeToString(databaseHash[:]), ArchiveSHA256: hex.EncodeToString(archiveHash[:]), CheckpointSHA256: hex.EncodeToString(checkpointHash[:]), HadBackendEnv: hadBackend}
	if hadBackend {
		backendHash := sha256.Sum256(previousBackend)
		state.BackendEnvSHA256 = hex.EncodeToString(backendHash[:])
	}
	for _, instance := range m.Instances {
		state.Instances = append(state.Instances, instance.Slug)
	}
	body, err := json.Marshal(state)
	if err != nil {
		return err
	}
	if err = writePrivateSync(filepath.Join(stage, "recovery.json"), body); err != nil {
		return err
	}
	directory, err := os.Open(stage)
	if err != nil {
		return err
	}
	defer directory.Close()
	if err = directory.Sync(); err != nil && runtime.GOOS != "windows" {
		return err
	}
	return nil
}

func writePrivateSync(path string, body []byte) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	if err = file.Chmod(0600); err == nil {
		_, err = file.Write(body)
	}
	if err == nil {
		err = file.Sync()
	}
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	return err
}

// Recover restores a failed global move's exact pre-import database/config
// checkpoint. It requires the original archive and refuses active/enabled
// target units so a rollback cannot race target workers.
func Recover(ctx context.Context, o RecoverOptions) error {
	for _, value := range []string{o.DatabaseURL, o.Archive, o.Stage, o.BackendEnvPath, o.InstanceRoot, o.UnitRoot} {
		if value == "" {
			return errors.New("global recovery requires archive, stage, database, and target paths")
		}
	}
	stage, err := filepath.Abs(o.Stage)
	if err != nil {
		return err
	}
	parent, err := filepath.Abs(filepath.Dir(o.BackendEnvPath))
	if err != nil || filepath.Dir(stage) != parent || !strings.HasPrefix(filepath.Base(stage), ".global-restore-") {
		return errors.New("recovery stage is outside the protected backend config directory")
	}
	info, err := os.Lstat(stage)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || (runtime.GOOS != "windows" && info.Mode().Perm()&0077 != 0) {
		return errors.New("recovery stage must be a private, regular directory")
	}
	if err = ensureTargetUnitsInactive(ctx, o.UnitRoot); err != nil {
		return err
	}
	stateBytes, err := readPrivateFile(filepath.Join(stage, "recovery.json"))
	if err != nil {
		return errors.New("recovery stage has no valid durable checkpoint")
	}
	var state recoveryManifest
	if err = json.Unmarshal(stateBytes, &state); err != nil || state.Format != 1 {
		return errors.New("recovery checkpoint metadata is invalid")
	}
	archive, err := os.ReadFile(o.Archive)
	if err != nil {
		return err
	}
	archiveManifest, _, _, err := readArchive(o.Archive)
	if err != nil || archiveManifest.Scope != "global-system" {
		return errors.New("recovery archive is invalid")
	}
	archiveHash := sha256.Sum256(archive)
	databaseHash := sha256.Sum256([]byte(o.DatabaseURL))
	if state.ArchiveSHA256 != hex.EncodeToString(archiveHash[:]) || state.DatabaseURLSHA256 != hex.EncodeToString(databaseHash[:]) {
		return errors.New("recovery stage belongs to a different archive or target database")
	}
	checkpoint, err := readPrivateFile(filepath.Join(stage, "target-database.sql"))
	if err != nil {
		return errors.New("recovery database checkpoint is missing")
	}
	checkpointHash := sha256.Sum256(checkpoint)
	if state.CheckpointSHA256 != hex.EncodeToString(checkpointHash[:]) {
		return errors.New("recovery database checkpoint checksum does not match")
	}
	var previousBackend []byte
	if state.HadBackendEnv {
		previousBackend, err = readPrivateFile(filepath.Join(stage, "target-backend.env"))
		if err != nil {
			return errors.New("recovery backend config checkpoint is missing")
		}
		backendHash := sha256.Sum256(previousBackend)
		if state.BackendEnvSHA256 != hex.EncodeToString(backendHash[:]) {
			return errors.New("recovery backend config checksum does not match")
		}
	}
	m := Manifest{Instances: make([]InstanceState, 0, len(state.Instances))}
	for _, slug := range state.Instances {
		if !validSlug(slug) {
			return errors.New("recovery checkpoint has an invalid instance slug")
		}
		m.Instances = append(m.Instances, InstanceState{Slug: slug})
	}
	if len(m.Instances) != len(archiveManifest.Instances) {
		return errors.New("recovery checkpoint instance list does not match the archive")
	}
	archiveSlugs := map[string]bool{}
	for _, instance := range archiveManifest.Instances {
		archiveSlugs[instance.Slug] = true
	}
	for _, instance := range m.Instances {
		if !archiveSlugs[instance.Slug] {
			return errors.New("recovery checkpoint instance list does not match the archive")
		}
	}
	if err = rollbackTargetDatabase(ctx, o.DatabaseURL, checkpoint); err != nil {
		return err
	}
	if err = restoreTargetFiles(RestoreOptions{BackendEnvPath: o.BackendEnvPath, InstanceRoot: o.InstanceRoot}, m, previousBackend, state.HadBackendEnv); err != nil {
		return fmt.Errorf("database rolled back but target files need recovery: %w", err)
	}
	return os.RemoveAll(stage)
}

func restoreTargetFiles(o RestoreOptions, m Manifest, previousBackend []byte, hadBackend bool) error {
	var errs []string
	for _, instance := range m.Instances {
		if err := os.RemoveAll(filepath.Join(o.InstanceRoot, instance.Slug)); err != nil {
			errs = append(errs, "instance directory cleanup failed")
		}
	}
	if hadBackend {
		if err := os.WriteFile(o.BackendEnvPath, previousBackend, 0600); err != nil {
			errs = append(errs, "backend.env restore failed")
		} else if runtime.GOOS == "linux" {
			_, gid, idErr := serviceIDs()
			if idErr == nil {
				if os.Chown(o.BackendEnvPath, 0, gid) != nil || os.Chmod(o.BackendEnvPath, 0640) != nil {
					errs = append(errs, "backend.env permissions restore failed")
				}
			}
		}
	} else if err := os.Remove(o.BackendEnvPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		errs = append(errs, "backend.env cleanup failed")
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}

func stageFiles(o RestoreOptions, backend []byte, instances map[string][]byte, m Manifest) (string, error) {
	stageParent := filepath.Dir(o.BackendEnvPath)
	if err := os.MkdirAll(stageParent, 0700); err != nil {
		return "", err
	}
	stage, err := os.MkdirTemp(stageParent, ".global-restore-*")
	if err != nil {
		return "", err
	}
	if err = os.Chmod(stage, 0700); err != nil {
		_ = os.RemoveAll(stage)
		return "", err
	}
	if err = os.WriteFile(filepath.Join(stage, "backend.env"), backend, 0600); err != nil {
		_ = os.RemoveAll(stage)
		return "", err
	}
	for name, data := range instances {
		p := filepath.Join(stage, filepath.FromSlash(name))
		if err = os.MkdirAll(filepath.Dir(p), 0700); err != nil {
			_ = os.RemoveAll(stage)
			return "", err
		}
		if err = os.WriteFile(p, data, 0600); err != nil {
			_ = os.RemoveAll(stage)
			return "", err
		}
	}
	units := filepath.Join(stage, "systemd")
	if err = os.Mkdir(units, 0700); err != nil {
		_ = os.RemoveAll(stage)
		return "", err
	}
	for _, state := range m.Instances {
		if err = os.WriteFile(filepath.Join(units, instanceUnitName(state.Slug)), []byte(instanceUnit(state.Slug)), 0600); err != nil {
			_ = os.RemoveAll(stage)
			return "", err
		}
	}
	if err = os.WriteFile(filepath.Join(units, "xui-backend.service"), []byte(backendUnit()), 0600); err != nil {
		_ = os.RemoveAll(stage)
		return "", err
	}
	return stage, nil
}

func installStagedFiles(stage string, o RestoreOptions, m Manifest, uid, gid int) error {
	modes := targetRuntimeModes()
	if err := ensureEmptyDir(o.InstanceRoot); err != nil {
		return err
	}
	if err := ensureUnitAvailable(filepath.Join(o.UnitRoot, "xui-backend.service"), []byte(backendUnit())); err != nil {
		return err
	}
	for _, s := range m.Instances {
		if err := ensureUnitAvailable(filepath.Join(o.UnitRoot, instanceUnitName(s.Slug)), []byte(instanceUnit(s.Slug))); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(filepath.Dir(o.BackendEnvPath), 0750); err != nil {
		return err
	}
	if err := os.MkdirAll(o.InstanceRoot, modes.Registry); err != nil {
		return err
	}
	if err := os.MkdirAll(o.UnitRoot, 0755); err != nil {
		return err
	}
	backendPath := filepath.Join(stage, "backend.env")
	if err := replaceConfig(backendPath, o.BackendEnvPath); err != nil {
		return err
	}
	for _, s := range m.Instances {
		dir := filepath.Join(o.InstanceRoot, s.Slug)
		if err := os.Mkdir(dir, modes.Instance); err != nil && !errors.Is(err, os.ErrExist) {
			return err
		}
		for _, name := range []string{"instance.env", "metadata.json"} {
			src := filepath.Join(stage, "instances", s.Slug, name)
			if _, err := os.Stat(src); errors.Is(err, os.ErrNotExist) {
				continue
			} else if err != nil {
				return err
			}
			if err := atomicInstall(src, filepath.Join(dir, name), modes.Secret); err != nil {
				return err
			}
		}
		if err := os.Chmod(dir, modes.Instance); err != nil {
			return err
		}
		if runtime.GOOS == "linux" {
			if err := os.Chown(dir, uid, gid); err != nil {
				return fmt.Errorf("could not set service ownership on instance directory %s", s.Slug)
			}
			for _, name := range []string{"instance.env", "metadata.json"} {
				path := filepath.Join(dir, name)
				if _, err := os.Stat(path); err == nil {
					if err = os.Chmod(path, modes.Secret); err != nil {
						return err
					}
					if err = os.Chown(path, uid, gid); err != nil {
						return fmt.Errorf("could not set service ownership on instance file %s", name)
					}
				}
			}
		}
	}
	unitNames := []string{"xui-backend.service"}
	for _, s := range m.Instances {
		unitNames = append(unitNames, instanceUnitName(s.Slug))
	}
	for _, name := range unitNames {
		src := filepath.Join(stage, "systemd", name)
		if err := atomicInstall(src, filepath.Join(o.UnitRoot, name), modes.Unit); err != nil {
			return err
		}
	}
	if runtime.GOOS == "linux" {
		if err := os.Chown(o.InstanceRoot, 0, gid); err != nil {
			return errors.New("could not set service group on instance registry directory")
		}
		if err := os.Chmod(o.InstanceRoot, modes.Registry); err != nil {
			return err
		}
		if err := os.Chown(o.BackendEnvPath, 0, gid); err != nil {
			return errors.New("could not set service group on backend.env")
		}
		if err := os.Chmod(o.BackendEnvPath, modes.Env); err != nil {
			return err
		}
		if err := os.Chown(filepath.Dir(o.BackendEnvPath), 0, gid); err != nil {
			return errors.New("could not set service group on backend config directory")
		}
		if err := os.Chmod(filepath.Dir(o.BackendEnvPath), 0750); err != nil {
			return err
		}
	}
	return nil
}

func atomicInstall(src, dst string, mode os.FileMode) error {
	if _, err := os.Lstat(dst); err == nil {
		existing, readErr := os.ReadFile(dst)
		staged, stageErr := os.ReadFile(src)
		if readErr != nil || stageErr != nil || !bytes.Equal(existing, staged) {
			return fmt.Errorf("refusing to overwrite different existing target file %s", filepath.Base(dst))
		}
		return os.Remove(src)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Chmod(src, mode); err != nil {
		return err
	}
	return os.Rename(src, dst)
}

func replaceConfig(src, dst string) error {
	if old, err := os.ReadFile(dst); err == nil {
		backup := dst + ".pre-global-restore-" + time.Now().UTC().Format("20060102T150405Z")
		f, e := os.OpenFile(backup, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if e != nil {
			return errors.New("could not preserve target backend configuration before restore")
		}
		if _, e = f.Write(old); e == nil {
			e = f.Sync()
		}
		if closeErr := f.Close(); e == nil {
			e = closeErr
		}
		if e != nil {
			_ = os.Remove(backup)
			return errors.New("could not preserve target backend configuration before restore")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Chmod(src, 0600); err != nil {
		return err
	}
	if err := os.Rename(src, dst); err != nil {
		return errors.New("could not install restored backend configuration")
	}
	return nil
}

func validateTargetPaths(o RestoreOptions) error {
	for _, p := range []string{o.BackendEnvPath, o.InstanceRoot, o.UnitRoot} {
		if p == "" {
			return errors.New("target config and unit paths are required")
		}
	}
	if err := ensureEmptyDir(o.InstanceRoot); err != nil {
		return err
	}
	if info, err := os.Lstat(o.BackendEnvPath); err == nil {
		if !info.Mode().IsRegular() {
			return errors.New("target backend.env must be a regular file")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func ensureEmptyDir(path string) error {
	entries, err := os.ReadDir(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if len(entries) != 0 {
		return fmt.Errorf("target instance registry %s is not empty", path)
	}
	return nil
}

func ensureUnitAvailable(path string, expected []byte) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("target unit %s is not a regular file", filepath.Base(path))
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if !bytes.Equal(b, expected) {
		return fmt.Errorf("target unit %s exists and differs from the portable unit definition", filepath.Base(path))
	}
	return nil
}

func availableLoopbackAddr(request string) (string, error) {
	host, portText, err := net.SplitHostPort(request)
	if err != nil {
		host, portText = "127.0.0.1", "8088"
	}
	if host == "" || strings.EqualFold(host, "localhost") {
		host = "127.0.0.1"
	} else if ip := net.ParseIP(host); ip == nil || (!ip.IsLoopback() && !ip.IsUnspecified()) {
		return "", errors.New("target backend listen address must be a loopback or unspecified IP address")
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		port = 8088
	}
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	listener, err := net.Listen("tcp", addr)
	if err == nil {
		_ = listener.Close()
		return addr, nil
	}
	listener, err = net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", errors.New("no available loopback port for the restored backend")
	}
	defer listener.Close()
	return listener.Addr().String(), nil
}

type unitStatus struct{ Enabled, Active bool }

func unitState(ctx context.Context, name, root string, expected []byte) (unitStatus, error) {
	path := filepath.Join(root, name)
	info, err := os.Lstat(path)
	if err != nil {
		return unitStatus{}, fmt.Errorf("runtime unit %s is missing", name)
	}
	if !info.Mode().IsRegular() {
		return unitStatus{}, fmt.Errorf("runtime unit %s is not a regular file", name)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return unitStatus{}, err
	}
	if !bytes.Equal(data, expected) {
		return unitStatus{}, fmt.Errorf("runtime unit %s differs from the portable definition", name)
	}
	enabled := systemctlQuery(ctx, "is-enabled", name) == "enabled"
	active := systemctlQuery(ctx, "is-active", name) == "active"
	return unitStatus{Enabled: enabled, Active: active}, nil
}

func readInstances(root string) (map[string][]byte, []string, error) {
	if info, err := os.Stat(root); err != nil || !info.IsDir() {
		return nil, nil, errors.New("instance registry is unavailable")
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, nil, err
	}
	files := map[string][]byte{}
	var slugs []string
	for _, entry := range entries {
		if !entry.IsDir() || !validSlug(entry.Name()) {
			continue
		}
		dir := filepath.Join(root, entry.Name())
		for _, name := range []string{"instance.env", "metadata.json"} {
			p := filepath.Join(dir, name)
			data, e := readPrivateFileOptional(p, name == "metadata.json")
			if e != nil {
				return nil, nil, fmt.Errorf("instance %s: %w", entry.Name(), e)
			}
			if data != nil {
				files["instances/"+entry.Name()+"/"+name] = data
			}
		}
		if _, ok := files["instances/"+entry.Name()+"/instance.env"]; !ok {
			return nil, nil, fmt.Errorf("instance %s is missing instance.env", entry.Name())
		}
		slugs = append(slugs, entry.Name())
	}
	sort.Strings(slugs)
	return files, slugs, nil
}

func readPrivateFile(path string) ([]byte, error) { return readPrivateFileOptional(path, false) }
func readPrivateFileOptional(path string, optional bool) ([]byte, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) && optional {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || (runtime.GOOS != "windows" && info.Mode().Perm()&0007 != 0) {
		return nil, fmt.Errorf("%s must be a regular private file", filepath.Base(path))
	}
	return os.ReadFile(path)
}

func readArchive(path string) (Manifest, map[string][]byte, []byte, error) {
	zr, err := zip.OpenReader(path)
	if err != nil {
		return Manifest{}, nil, nil, err
	}
	defer zr.Close()
	if len(zr.File) > 10000 {
		return Manifest{}, nil, nil, errors.New("backup contains too many files")
	}
	files := map[string][]byte{}
	seen := map[string]bool{}
	var dump []byte
	var m Manifest
	for _, f := range zr.File {
		clean := pathpkg.Clean(f.Name)
		if strings.HasPrefix(f.Name, "/") || clean == ".." || strings.HasPrefix(clean, "../") || strings.Contains(f.Name, "\\") || (len(f.Name) > 2 && f.Name[1] == ':') {
			return m, nil, nil, errors.New("backup contains an unsafe path")
		}
		if seen[f.Name] {
			return m, nil, nil, errors.New("backup contains duplicate paths")
		}
		seen[f.Name] = true
		if f.UncompressedSize64 > maxEntryBytes {
			return m, nil, nil, errors.New("backup entry exceeds size limit")
		}
		data, e := readZip(f)
		if e != nil {
			return m, nil, nil, e
		}
		if f.Name == manifestName {
			if e = json.Unmarshal(data, &m); e != nil {
				return m, nil, nil, errors.New("invalid backup manifest")
			}
			continue
		}
		if f.Name == "database.sql" {
			dump = data
			continue
		}
		files[f.Name] = data
	}
	if m.Format != archiveFormat || m.Scope != "global-system" || len(dump) == 0 {
		return m, nil, nil, errors.New("unsupported or incomplete portable global backup")
	}
	sum := sha256.Sum256(dump)
	if hex.EncodeToString(sum[:]) != m.DatabaseSHA256 {
		return m, nil, nil, errors.New("database dump checksum mismatch")
	}
	if len(m.Files) != len(files) {
		return m, nil, nil, errors.New("backup file manifest mismatch")
	}
	for name, data := range files {
		expected, ok := m.Files[name]
		if !ok {
			return m, nil, nil, errors.New("backup contains an unlisted file")
		}
		s := sha256.Sum256(data)
		if hex.EncodeToString(s[:]) != expected {
			return m, nil, nil, errors.New("backup file checksum mismatch")
		}
	}
	if _, ok := files["backend.env"]; !ok {
		return m, nil, nil, errors.New("backup is missing backend.env")
	}
	if _, ok := files["systemd/xui-backend.service"]; !ok {
		return m, nil, nil, errors.New("backup is missing backend unit")
	}
	expectedFiles := map[string]bool{"backend.env": true, "systemd/xui-backend.service": true}
	seenSlugs := map[string]bool{}
	for _, s := range m.Instances {
		if !validSlug(s.Slug) {
			return m, nil, nil, errors.New("backup has invalid instance slug")
		}
		if seenSlugs[s.Slug] {
			return m, nil, nil, errors.New("backup repeats an instance slug")
		}
		seenSlugs[s.Slug] = true
		expectedFiles["systemd/"+instanceUnitName(s.Slug)] = true
		expectedFiles["instances/"+s.Slug+"/instance.env"] = true
		if _, ok := files["instances/"+s.Slug+"/instance.env"]; !ok {
			return m, nil, nil, errors.New("backup is missing instance configuration")
		}
		if _, ok := files["instances/"+s.Slug+"/metadata.json"]; ok {
			expectedFiles["instances/"+s.Slug+"/metadata.json"] = true
		}
		unit, ok := files["systemd/"+instanceUnitName(s.Slug)]
		if !ok || !bytes.Equal(unit, []byte(instanceUnit(s.Slug))) {
			return m, nil, nil, errors.New("backup has invalid instance unit definition")
		}
	}
	if !bytes.Equal(files["systemd/xui-backend.service"], []byte(backendUnit())) {
		return m, nil, nil, errors.New("backup has invalid backend unit definition")
	}
	if len(expectedFiles) != len(files) {
		return m, nil, nil, errors.New("backup contains unexpected runtime files")
	}
	for name := range files {
		if !expectedFiles[name] {
			return m, nil, nil, errors.New("backup contains an unexpected file")
		}
	}
	backendValues, e := parseEnv(files["backend.env"])
	if e != nil || backendValues["DATABASE_URL"] == "" {
		return m, nil, nil, errors.New("backup backend configuration is invalid")
	}
	for _, s := range m.Instances {
		cfg, e := parseEnv(files["instances/"+s.Slug+"/instance.env"])
		if e != nil || cfg["BACKEND_DEPLOYMENT_ID"] == "" || cfg["CHANNEL"] != "retail" && cfg["CHANNEL"] != "reseller" || cfg["BACKEND_TOKEN"] == "" || cfg["TELEGRAM_BOT_TOKEN"] == "" {
			return m, nil, nil, fmt.Errorf("instance %s configuration is incomplete", s.Slug)
		}
	}
	return m, files, dump, nil
}

func writeArchive(destination string, m Manifest, dump []byte, files map[string][]byte) error {
	if err := os.MkdirAll(filepath.Dir(destination), 0700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(destination), ".global-backup-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err = tmp.Chmod(0600); err != nil {
		tmp.Close()
		return err
	}
	zw := zip.NewWriter(tmp)
	if err = writeZip(zw, "database.sql", dump); err != nil {
		zw.Close()
		tmp.Close()
		return err
	}
	keys := make([]string, 0, len(files))
	for k := range files {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if err = writeZip(zw, k, files[k]); err != nil {
			zw.Close()
			tmp.Close()
			return err
		}
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err == nil {
		err = writeZip(zw, manifestName, b)
	}
	if err == nil {
		err = zw.Close()
	} else {
		_ = zw.Close()
	}
	if err != nil {
		tmp.Close()
		return err
	}
	if err = tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	if err = os.Link(name, destination); err != nil {
		return errors.New("could not create backup archive without overwriting an existing path")
	}
	return os.Remove(name)
}
func writeZip(zw *zip.Writer, name string, b []byte) error {
	h := &zip.FileHeader{Name: name, Method: zip.Deflate}
	h.SetMode(0600)
	w, e := zw.CreateHeader(h)
	if e != nil {
		return e
	}
	_, e = w.Write(b)
	return e
}
func readZip(f *zip.File) ([]byte, error) {
	r, e := f.Open()
	if e != nil {
		return nil, e
	}
	defer r.Close()
	b, e := io.ReadAll(io.LimitReader(r, maxEntryBytes+1))
	if e == nil && len(b) > maxEntryBytes {
		return nil, errors.New("backup entry exceeds size limit")
	}
	return b, e
}

func pgEnvironment(raw string) (string, []string, error) {
	u, e := url.Parse(raw)
	if e != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") {
		return "", nil, errors.New("invalid PostgreSQL URL")
	}
	db := strings.TrimPrefix(u.Path, "/")
	if db == "" {
		return "", nil, errors.New("database name is required")
	}
	host := u.Hostname()
	if host == "" {
		host = "localhost"
	}
	port := u.Port()
	if port == "" {
		port = "5432"
	}
	env := []string{"PGHOST=" + host, "PGPORT=" + port, "PGDATABASE=" + db}
	if u.User != nil {
		env = append(env, "PGUSER="+u.User.Username())
		if p, ok := u.User.Password(); ok {
			env = append(env, "PGPASSWORD="+p)
		}
	}
	if ssl := u.Query().Get("sslmode"); ssl != "" {
		env = append(env, "PGSSLMODE="+ssl)
	}
	return db, env, nil
}
func systemctl(ctx context.Context, args ...string) error {
	cmd := exec.CommandContext(ctx, "systemctl", args...)
	if err := cmd.Run(); err != nil {
		return err
	}
	return nil
}
func systemctlQuery(ctx context.Context, args ...string) string {
	cmd := exec.CommandContext(ctx, "systemctl", args...)
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
func validSlug(s string) bool {
	if s == "" || len(s) > 48 {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
			return false
		}
	}
	return true
}
func instanceUnitName(slug string) string { return "xui-backend-instance-" + slug + ".service" }
func instanceUnit(slug string) string {
	return fmt.Sprintf("[Unit]\nDescription=xui-backend bot instance %s\nAfter=network-online.target xui-backend.service\nWants=network-online.target\n\n[Service]\nType=simple\nExecStart=/usr/local/bin/xui-backend run-instance %s\nRestart=on-failure\nRestartSec=5\nUser=xui-backend\nGroup=xui-backend\nUMask=0077\nNoNewPrivileges=true\nPrivateTmp=true\nProtectSystem=strict\nProtectHome=true\n\n[Install]\nWantedBy=multi-user.target\n", slug, slug)
}
func backendUnit() string {
	return "[Unit]\nDescription=XUI Backend commerce authority\nAfter=network-online.target postgresql.service\nWants=network-online.target\n\n[Service]\nType=simple\nUser=xui-backend\nGroup=xui-backend\nEnvironmentFile=/etc/xui-backend/backend.env\nExecStart=/usr/local/bin/xui-backend serve\nRestart=on-failure\nRestartSec=5\nNoNewPrivileges=true\nPrivateTmp=true\nProtectSystem=strict\nProtectHome=true\nReadWritePaths=/var/lib/xui-backend /etc/xui-backend/instances\n\n[Install]\nWantedBy=multi-user.target\n"
}

func parseEnv(data []byte) (map[string]string, error) {
	out := map[string]string{}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		i := strings.IndexByte(line, '=')
		if i < 1 {
			return nil, errors.New("invalid environment line")
		}
		key := strings.TrimSpace(line[:i])
		value := strings.TrimSpace(line[i+1:])
		if strings.HasPrefix(value, "\"") {
			v, e := strconv.Unquote(value)
			if e != nil {
				return nil, e
			}
			value = v
		}
		out[key] = value
	}
	return out, nil
}
func formatEnv(values map[string]string) []byte {
	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&b, "%s=%s\n", k, strconv.Quote(values[k]))
	}
	return []byte(b.String())
}

var publicTables = []string{
	"schema_migrations", "client_services", "panels", "deployments", "commercial_accounts", "actors", "plans", "purchase_quotes", "wallet_ledger", "subscriptions", "payment_intents", "orders", "payment_settlements", "work_items", "trial_usage", "trial_claims", "outbox", "topup_requests", "wallet_credit_approvals", "legacy_import_batches", "legacy_id_map", "legacy_records", "migration_audit", "refund_requests", "refund_approvals", "legacy_panel_assignments", "legacy_obligations", "plan_access", "admin_configuration_audit", "reseller_access_requests", "backend_client_credentials",
}

func inspectTargetDatabase(ctx context.Context, databaseURL string) (string, error) {
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		return "", errors.New("could not configure target database connection")
	}
	defer pool.Close()
	if err = pool.Ping(ctx); err != nil {
		return "", errors.New("could not reach target database")
	}
	rows, err := pool.Query(ctx, `SELECT tablename FROM pg_tables WHERE schemaname='public' ORDER BY tablename`)
	if err != nil {
		return "", errors.New("could not inspect target database schema")
	}
	tables := []string{}
	for rows.Next() {
		var name string
		if err = rows.Scan(&name); err != nil {
			rows.Close()
			return "", err
		}
		tables = append(tables, name)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return "", err
	}
	if len(tables) == 0 {
		return "empty", nil
	}
	if len(tables) != len(publicTables) {
		return "", errors.New("target database has objects outside a fresh xui-backend install; refusing global restore")
	}
	expectedTables := append([]string(nil), publicTables...)
	sort.Strings(expectedTables)
	for i, name := range expectedTables {
		if tables[i] != name {
			return "", errors.New("target database schema does not match a fresh xui-backend install; refusing restore")
		}
	}
	var migrations int
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM schema_migrations`).Scan(&migrations); err != nil || migrations < 12 {
		return "", errors.New("target database is not a complete fresh xui-backend installation")
	}
	for _, name := range publicTables {
		if name == "schema_migrations" || name == "client_services" || name == "panels" || name == "deployments" || name == "commercial_accounts" || name == "actors" {
			continue
		}
		var n int
		if err = pool.QueryRow(ctx, `SELECT count(*) FROM `+name).Scan(&n); err != nil || n != 0 {
			return "", fmt.Errorf("target database contains data in %s; refusing global restore", name)
		}
	}
	checks := []struct {
		query string
		want  int
	}{
		{`SELECT count(*) FROM client_services WHERE id IN ('retail-bot','reseller-bot')`, 2},
		{`SELECT count(*) FROM panels WHERE id IN ('panel-retail-finland','panel-retail-germany','panel-reseller-turk1') AND base_url LIKE '%.invalid' AND encrypted_api_token IS NULL`, 3},
		{`SELECT count(*) FROM deployments WHERE (id='retail-finland' AND channel='retail' AND client_service_id='retail-bot' AND default_panel_id='panel-retail-finland' AND admin_telegram_id=96937669) OR (id='retail-germany' AND channel='retail' AND client_service_id='retail-bot' AND default_panel_id='panel-retail-germany' AND admin_telegram_id IS NULL) OR (id='reseller-turk1' AND channel='reseller' AND client_service_id='reseller-bot' AND default_panel_id='panel-reseller-turk1' AND admin_telegram_id=96937669)`, 3},
		{`SELECT count(*) FROM deployments WHERE bot_instance_active AND telegram_notifications_enabled AND enabled AND encrypted_telegram_token IS NULL AND payment_card_number='' AND payment_card_owner='' AND payment_instructions='' AND configuration='{"features": {}, "text": {}}'::jsonb`, 3},
		{`SELECT count(*) FROM commercial_accounts WHERE (home_deployment_id='retail-finland' AND system_key='telegram-admin:96937669:retail-finland') OR (home_deployment_id='reseller-turk1' AND system_key='telegram-admin:96937669:reseller-turk1')`, 2},
		{`SELECT count(*) FROM actors WHERE telegram_id=96937669 AND identity_provider='telegram' AND external_subject='96937669' AND role='admin' AND approval_status='approved' AND enabled`, 2},
	}
	for _, c := range checks {
		var n int
		if err = pool.QueryRow(ctx, c.query).Scan(&n); err != nil || n != c.want {
			return "", errors.New("target database contains modified bootstrap configuration or user data; refusing global restore")
		}
	}
	var n int
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM client_services`).Scan(&n); err != nil || n != 2 {
		return "", errors.New("target bootstrap services differ from installer defaults")
	}
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM panels`).Scan(&n); err != nil || n != 3 {
		return "", errors.New("target bootstrap panels differ from installer defaults")
	}
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM deployments`).Scan(&n); err != nil || n != 3 {
		return "", errors.New("target bootstrap deployments differ from installer defaults")
	}
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM commercial_accounts`).Scan(&n); err != nil || n != 2 {
		return "", errors.New("target bootstrap accounts differ from installer defaults")
	}
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM actors`).Scan(&n); err != nil || n != 2 {
		return "", errors.New("target bootstrap actors differ from installer defaults")
	}
	return "pristine-install", nil
}

func pristineResetPreamble() string {
	quoted := make([]string, len(publicTables))
	for i, n := range publicTables {
		quoted[i] = "public." + n
	}
	var b strings.Builder
	fmt.Fprintf(&b, "LOCK TABLE %s IN ACCESS EXCLUSIVE MODE;\n", strings.Join(quoted, ", "))
	b.WriteString(`DO $$ BEGIN
  IF (SELECT count(*) FROM schema_migrations) < 12
   OR (SELECT count(*) FROM client_services) <> 2
   OR (SELECT count(*) FROM panels) <> 3
   OR (SELECT count(*) FROM deployments) <> 3
   OR (SELECT count(*) FROM commercial_accounts) <> 2
   OR (SELECT count(*) FROM actors) <> 2
   OR EXISTS (SELECT 1 FROM plans) OR EXISTS (SELECT 1 FROM purchase_quotes)
   OR EXISTS (SELECT 1 FROM wallet_ledger) OR EXISTS (SELECT 1 FROM subscriptions)
   OR EXISTS (SELECT 1 FROM payment_intents) OR EXISTS (SELECT 1 FROM orders)
   OR EXISTS (SELECT 1 FROM payment_settlements) OR EXISTS (SELECT 1 FROM work_items)
   OR EXISTS (SELECT 1 FROM trial_usage) OR EXISTS (SELECT 1 FROM trial_claims)
   OR EXISTS (SELECT 1 FROM outbox) OR EXISTS (SELECT 1 FROM topup_requests)
   OR EXISTS (SELECT 1 FROM wallet_credit_approvals) OR EXISTS (SELECT 1 FROM legacy_import_batches)
   OR EXISTS (SELECT 1 FROM legacy_id_map) OR EXISTS (SELECT 1 FROM legacy_records)
   OR EXISTS (SELECT 1 FROM migration_audit) OR EXISTS (SELECT 1 FROM refund_requests)
   OR EXISTS (SELECT 1 FROM refund_approvals) OR EXISTS (SELECT 1 FROM legacy_panel_assignments)
   OR EXISTS (SELECT 1 FROM legacy_obligations) OR EXISTS (SELECT 1 FROM plan_access)
   OR EXISTS (SELECT 1 FROM admin_configuration_audit) OR EXISTS (SELECT 1 FROM reseller_access_requests)
   OR EXISTS (SELECT 1 FROM backend_client_credentials)
   OR (SELECT count(*) FROM client_services WHERE id IN ('retail-bot','reseller-bot')) <> 2
   OR (SELECT count(*) FROM panels WHERE id IN ('panel-retail-finland','panel-retail-germany','panel-reseller-turk1') AND base_url LIKE '%.invalid' AND encrypted_api_token IS NULL) <> 3
   OR (SELECT count(*) FROM deployments WHERE (id='retail-finland' AND channel='retail' AND client_service_id='retail-bot' AND default_panel_id='panel-retail-finland' AND admin_telegram_id=96937669) OR (id='retail-germany' AND channel='retail' AND client_service_id='retail-bot' AND default_panel_id='panel-retail-germany' AND admin_telegram_id IS NULL) OR (id='reseller-turk1' AND channel='reseller' AND client_service_id='reseller-bot' AND default_panel_id='panel-reseller-turk1' AND admin_telegram_id=96937669)) <> 3
   OR (SELECT count(*) FROM deployments WHERE bot_instance_active AND telegram_notifications_enabled AND enabled AND encrypted_telegram_token IS NULL AND payment_card_number='' AND payment_card_owner='' AND payment_instructions='' AND configuration='{"features": {}, "text": {}}'::jsonb) <> 3
   OR (SELECT count(*) FROM commercial_accounts WHERE (home_deployment_id='retail-finland' AND system_key='telegram-admin:96937669:retail-finland') OR (home_deployment_id='reseller-turk1' AND system_key='telegram-admin:96937669:reseller-turk1')) <> 2
   OR (SELECT count(*) FROM actors WHERE telegram_id=96937669 AND identity_provider='telegram' AND external_subject='96937669' AND role='admin' AND approval_status='approved' AND enabled) <> 2
  THEN RAISE EXCEPTION 'target database no longer matches a pristine xui-backend install'; END IF;
END $$;
DROP SCHEMA public CASCADE;
CREATE SCHEMA public;
`)
	return b.String()
}

func migrateAndQuarantine(ctx context.Context, databaseURL string) error {
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		return errors.New("could not connect to restored database")
	}
	defer pool.Close()
	if err = pool.Ping(ctx); err != nil {
		return errors.New("could not reach restored database")
	}
	repo := &store.Store{DB: pool}
	if err = repo.Migrate(ctx); err != nil {
		return fmt.Errorf("apply destination migrations: %w", err)
	}
	if _, err = pool.Exec(ctx, `UPDATE work_items SET restore_quarantined=true WHERE status<>'succeeded'`); err != nil {
		return errors.New("could not quarantine restored provisioning work")
	}
	if _, err = pool.Exec(ctx, `UPDATE outbox SET restore_quarantined=true WHERE status<>'sent'`); err != nil {
		return errors.New("could not quarantine restored notifications")
	}
	return nil
}

func validateImportedDatabase(ctx context.Context, databaseURL string, files map[string][]byte, m Manifest) error {
	backend, err := parseEnv(files["backend.env"])
	if err != nil {
		return errors.New("backend environment is invalid")
	}
	key, err := secrets.ParseKey(backend["BACKEND_PANEL_SECRETS_KEY"])
	if err != nil {
		return errors.New("secret encryption key is invalid")
	}
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		return errors.New("could not connect to restored database")
	}
	defer pool.Close()
	if err = pool.Ping(ctx); err != nil {
		return errors.New("could not reach restored database")
	}
	rows, err := pool.Query(ctx, `SELECT encrypted_api_token FROM panels WHERE encrypted_api_token IS NOT NULL`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var cipher []byte
		if err = rows.Scan(&cipher); err != nil {
			rows.Close()
			return err
		}
		if _, err = secrets.Open(key, cipher); err != nil {
			rows.Close()
			return errors.New("secret encryption key cannot decrypt a restored panel token")
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	rows, err = pool.Query(ctx, `SELECT id, encrypted_telegram_token FROM deployments WHERE encrypted_telegram_token IS NOT NULL`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id string
		var cipher []byte
		if err = rows.Scan(&id, &cipher); err != nil {
			rows.Close()
			return err
		}
		if _, err = secrets.Open(key, cipher); err != nil {
			rows.Close()
			return fmt.Errorf("secret encryption key cannot decrypt deployment %s Telegram token", id)
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, state := range m.Instances {
		config, e := parseEnv(files["instances/"+state.Slug+"/instance.env"])
		if e != nil {
			return fmt.Errorf("instance %s environment is invalid", state.Slug)
		}
		deployment := config["BACKEND_DEPLOYMENT_ID"]
		if deployment == "" || (config["CHANNEL"] != "retail" && config["CHANNEL"] != "reseller") || config["BACKEND_TOKEN"] == "" || config["TELEGRAM_BOT_TOKEN"] == "" {
			return fmt.Errorf("instance %s has incomplete runtime credentials", state.Slug)
		}
		var channel string
		var enabled bool
		var encryptedTelegram []byte
		if e = pool.QueryRow(ctx, `SELECT channel,enabled AND bot_instance_active,encrypted_telegram_token FROM deployments WHERE id=$1`, deployment).Scan(&channel, &enabled, &encryptedTelegram); e != nil {
			return fmt.Errorf("instance %s deployment is missing", state.Slug)
		}
		if channel != config["CHANNEL"] || !enabled {
			return fmt.Errorf("instance %s deployment channel or active state does not match", state.Slug)
		}
		plain, e := secrets.Open(key, encryptedTelegram)
		if e != nil || string(plain) != config["TELEGRAM_BOT_TOKEN"] {
			return fmt.Errorf("instance %s Telegram token does not match the encrypted database credential", state.Slug)
		}
		digest := sha256.Sum256([]byte(strings.TrimSpace(config["BACKEND_TOKEN"])))
		var credentialMatches bool
		if e = pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM backend_client_credentials WHERE deployment_id=$1 AND token_hash=$2 AND enabled)`, deployment, digest[:]).Scan(&credentialMatches); e != nil {
			return e
		}
		if !credentialMatches {
			return fmt.Errorf("instance %s backend credential does not match the database", state.Slug)
		}
	}
	return nil
}
