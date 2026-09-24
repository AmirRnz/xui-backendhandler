// Package instancecli implements the local instance registry and operator menu.
package instancecli

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"example.com/xui-commerce/backend/internal/backup"
	"example.com/xui-commerce/backend/internal/panelurl"
	"example.com/xui-commerce/backend/internal/secrets"
	"example.com/xui-commerce/backend/internal/store"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/term"
)

const (
	defaultRoot         = "/etc/xui-backend/instances"
	defaultEnv          = "/etc/xui-backend/backend.env"
	defaultRestoreStage = "/var/lib/xui-backend/restore-staging"
)

type Config struct {
	Slug         string `json:"slug"`
	Channel      string `json:"channel"`
	DisplayName  string `json:"display_name"`
	DeploymentID string `json:"deployment_id"`
	BackendURL   string `json:"backend_url"`
	BackendToken string `json:"backend_token"`
	BotToken     string `json:"telegram_bot_token"`
	AdminID      int64  `json:"admin_telegram_id"`
}

type menu struct {
	in  *bufio.Reader
	out io.Writer
}

func Root() string {
	if value := strings.TrimSpace(os.Getenv("XUI_BACKEND_INSTANCE_ROOT")); value != "" {
		return value
	}
	return defaultRoot
}

func backendEnvPath() string {
	if value := strings.TrimSpace(os.Getenv("XUI_BACKEND_ENV_FILE")); value != "" {
		return value
	}
	return defaultEnv
}

func restoreStageRoot() string {
	if value := strings.TrimSpace(os.Getenv("XUI_BACKEND_RESTORE_STAGE_ROOT")); value != "" {
		return value
	}
	return defaultRestoreStage
}

func isLocalBackend(raw string) bool {
	u, err := url.ParseRequestURI(raw)
	if err != nil {
		return false
	}
	host := strings.ToLower(u.Hostname())
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func Menu(ctx context.Context) error {
	m := menu{in: bufio.NewReader(os.Stdin), out: os.Stdout}
	for {
		fmt.Fprintln(m.out, "\nxui-backend")
		fmt.Fprintln(m.out, "1) Add bot instance")
		fmt.Fprintln(m.out, "2) List/manage instances")
		fmt.Fprintln(m.out, "3) Global backup or restore")
		fmt.Fprintln(m.out, "4) Instance configuration backup or restore")
		fmt.Fprintln(m.out, "0) Exit")
		choice, err := m.ask("Select: ")
		if err != nil {
			return err
		}
		switch choice {
		case "0":
			return nil
		case "1":
			err = m.add(ctx)
		case "2":
			err = m.manage(ctx)
		case "3":
			err = m.globalBackup(ctx)
		case "4":
			err = m.instanceBackup()
		default:
			fmt.Fprintln(m.out, "Choose one of the displayed options.")
		}
		if err != nil {
			fmt.Fprintln(m.out, "Error:", err)
		}
	}
}

func Command(args []string) error {
	if len(args) >= 2 && args[0] == "backup" {
		switch args[1] {
		case "global":
			if len(args) != 3 {
				return errors.New("usage: xui-backend backup global <archive-path>")
			}
			values, err := readEnvFile(backendEnvPath())
			if err != nil {
				return err
			}
			return backup.CreateGlobal(context.Background(), values["DATABASE_URL"], Root(), args[2])
		case "instance-config":
			if len(args) != 4 {
				return errors.New("usage: xui-backend backup instance-config <slug> <archive-path>")
			}
			return backup.CreateInstanceConfig(Root(), args[2], args[3])
		}
	}
	if len(args) >= 2 && args[0] == "restore" {
		dry := len(args) == 3 && args[2] == "--dry-run"
		if len(args) == 2 || dry {
			values, err := readEnvFile(backendEnvPath())
			if err != nil {
				return err
			}
			if values["DATABASE_URL"] == "" {
				return errors.New("DATABASE_URL is missing from backend environment file")
			}
			stage, err := backup.StageGlobalConfig(args[1], restoreStageRoot(), dry)
			if err != nil {
				return err
			}
			if err := backup.RestoreGlobal(context.Background(), values["DATABASE_URL"], args[1], dry); err != nil {
				if !dry {
					return fmt.Errorf("configuration is staged at %s, but database restore failed: %w", stage, err)
				}
				return err
			}
			fmt.Println("Configuration stage:", stage)
			return nil
		}
		if (len(args) == 4 || len(args) == 5) && args[1] == "instance-config" {
			configDryRun := len(args) == 5 && args[4] == "--dry-run"
			if len(args) == 5 && !configDryRun {
				return errors.New("only --dry-run is supported after the new instance name")
			}
			stage, err := backup.StageInstanceConfig(args[2], restoreStageRoot(), args[3], configDryRun)
			if err == nil {
				fmt.Println("Inactive instance config stage:", stage)
			}
			return err
		}
	}
	return errors.New("usage: xui-backend [backup global <path>|backup instance-config <slug> <path>|restore <global-archive> [--dry-run]|restore instance-config <archive> <new-slug> [--dry-run]]")
}

func (m menu) add(ctx context.Context) error {
	channel, err := m.ask("Bot type (end-user/reseller): ")
	if err != nil {
		return err
	}
	if channel != "end-user" && channel != "reseller" {
		return errors.New("bot type must be end-user or reseller")
	}
	slug, err := m.ask("Instance name (lowercase letters, digits, hyphens): ")
	if err != nil {
		return err
	}
	if !validSlug(slug) {
		return errors.New("invalid instance name")
	}
	if _, err := os.Stat(filepath.Join(Root(), slug)); err == nil {
		return errors.New("an instance with that name already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	adminRaw, err := m.ask("Admin Telegram ID: ")
	if err != nil {
		return err
	}
	adminID, err := strconv.ParseInt(adminRaw, 10, 64)
	if err != nil || adminID <= 0 {
		return errors.New("admin Telegram ID must be a positive integer")
	}
	botToken, err := m.secret("Telegram bot token: ")
	if err != nil {
		return err
	}
	if !validTelegramToken(botToken) {
		return errors.New("Telegram token format is invalid")
	}
	panel, err := m.ask("3x-ui panel base URL: ")
	if err != nil {
		return err
	}
	if err := panelurl.Validate(panel); err != nil {
		return err
	}
	panelToken, err := m.secret("3x-ui API token: ")
	if err != nil {
		return err
	}
	if strings.TrimSpace(panelToken) == "" {
		return errors.New("panel authentication token is required")
	}
	backendURL, err := m.ask("Backend URL (for example http://127.0.0.1:8088): ")
	if err != nil {
		return err
	}
	if !validBackendURL(backendURL) {
		return errors.New("backend URL must be an HTTP(S) URL without credentials, query, or fragment")
	}
	backendValues, err := readEnvFile(backendEnvPath())
	if err != nil {
		return err
	}
	if backendValues["DATABASE_URL"] == "" {
		return errors.New("DATABASE_URL is missing from backend environment file")
	}
	key, err := secrets.ParseKey(backendValues["BACKEND_PANEL_SECRETS_KEY"])
	if err != nil {
		return fmt.Errorf("backend panel encryption key is not configured: %w", err)
	}
	display, err := m.ask("Instance display name: ")
	if err != nil {
		return err
	}
	if display == "" || len(display) > 100 {
		return errors.New("display name must be between 1 and 100 characters")
	}
	deploymentID, err := newResourceID("instance")
	if err != nil {
		return errors.New("could not allocate a unique deployment identity")
	}
	panelID, err := newResourceID("panel")
	if err != nil {
		return errors.New("could not allocate a unique panel identity")
	}
	if channel == "end-user" {
		channel = "retail"
	} else {
		channel = "reseller"
	}
	regCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	pool, err := pgxpool.New(regCtx, backendValues["DATABASE_URL"])
	if err != nil {
		return errors.New("cannot configure backend database connection")
	}
	defer pool.Close()
	if err := pool.Ping(regCtx); err != nil {
		return errors.New("cannot reach backend database")
	}
	repo := &store.Store{DB: pool}
	result, err := repo.RegisterInstance(regCtx, store.RegisterInstanceInput{DeploymentID: deploymentID, Channel: channel, DisplayName: display, PanelID: panelID, PanelURL: panel, PanelToken: panelToken, TelegramToken: botToken, AdminTelegramID: adminID}, key)
	if err != nil {
		if errors.Is(err, store.ErrConflict) {
			return errors.New("generated deployment or panel identity collided with an existing record; nothing was changed, please retry")
		}
		rollbackCtx, rollbackCancel := context.WithTimeout(context.Background(), 15*time.Second)
		rollbackErr := repo.DeactivateInstance(rollbackCtx, deploymentID)
		rollbackCancel()
		if rollbackErr != nil && !errors.Is(rollbackErr, store.ErrNotFound) {
			return fmt.Errorf("instance registration outcome is uncertain for deployment %s; revoke or inspect that deployment before retrying: %w", deploymentID, rollbackErr)
		}
		return errors.New("instance registration failed; no active instance was created")
	}
	cfg := Config{Slug: slug, Channel: channel, DisplayName: display, DeploymentID: result.DeploymentID, BackendURL: strings.TrimRight(backendURL, "/"), BackendToken: result.BackendToken, BotToken: botToken, AdminID: adminID}
	if err := save(cfg); err != nil {
		return compensateRegistration(cfg, err)
	}
	if err := installUnit(cfg); err != nil {
		return compensateRegistration(cfg, err)
	}
	fmt.Fprintf(m.out, "Instance %q registered. Scoped backend credential: %s.\n", slug, mask(result.BackendToken))
	fmt.Fprintln(m.out, "Subscription links are retrieved automatically from 3x-ui when services are provisioned; no subscription URL was needed.")
	if isLocalBackend(cfg.BackendURL) {
		if err := systemctl("enable", "--now", "xui-backend.service"); err != nil {
			return compensateRegistration(cfg, fmt.Errorf("backend service did not start: %w", err))
		}
	}
	if err := systemctl("daemon-reload"); err != nil {
		return compensateRegistration(cfg, err)
	}
	if err := systemctl("enable", "--now", unitName(slug)); err != nil {
		return compensateRegistration(cfg, fmt.Errorf("instance service did not start: %w", err))
	}
	fmt.Fprintln(m.out, "Instance is running.")
	return nil
}

func (m menu) manage(ctx context.Context) error {
	instances, err := List()
	if err != nil {
		return err
	}
	if len(instances) == 0 {
		fmt.Fprintln(m.out, "No instances are registered.")
		return nil
	}
	for i, instance := range instances {
		fmt.Fprintf(m.out, "%d) %s (%s, deployment %s)\n", i+1, instance.Slug, instance.Channel, instance.DeploymentID)
	}
	choice, err := m.ask("Instance number (0 to return): ")
	if err != nil {
		return err
	}
	index, err := strconv.Atoi(choice)
	if err != nil || index < 0 || index > len(instances) {
		return errors.New("invalid selection")
	}
	if index == 0 {
		return nil
	}
	cfg, err := Read(instances[index-1].Slug)
	if err != nil {
		return err
	}
	fmt.Fprintf(m.out, "\n%s (%s)\n1) Start\n2) Stop\n3) Restart\n4) Remove\n0) Return\n", cfg.Slug, cfg.Channel)
	action, err := m.ask("Select: ")
	if err != nil {
		return err
	}
	switch action {
	case "0":
		return nil
	case "1":
		if err := installUnit(cfg); err != nil {
			return err
		}
		if err := secureInstanceFiles(cfg.Slug); err != nil {
			return err
		}
		if isLocalBackend(cfg.BackendURL) {
			if err := systemctl("enable", "--now", "xui-backend.service"); err != nil {
				return err
			}
		}
		if err := systemctl("daemon-reload"); err != nil {
			return err
		}
		return systemctl("start", unitName(cfg.Slug))
	case "2":
		return systemctl("stop", unitName(cfg.Slug))
	case "3":
		if err := installUnit(cfg); err != nil {
			return err
		}
		if err := secureInstanceFiles(cfg.Slug); err != nil {
			return err
		}
		if isLocalBackend(cfg.BackendURL) {
			if err := systemctl("enable", "--now", "xui-backend.service"); err != nil {
				return err
			}
		}
		if err := systemctl("daemon-reload"); err != nil {
			return err
		}
		return systemctl("restart", unitName(cfg.Slug))
	case "4":
		confirm, err := m.ask("Remove this instance service and local credentials? Type REMOVE: ")
		if err != nil {
			return err
		}
		if confirm != "REMOVE" {
			return nil
		}
		if _, statErr := os.Stat(unitPath(cfg.Slug)); statErr == nil {
			if err := systemctl("disable", "--now", unitName(cfg.Slug)); err != nil {
				return fmt.Errorf("could not stop instance service; config was retained: %w", err)
			}
		} else if !errors.Is(statErr, os.ErrNotExist) {
			return statErr
		}
		if err := deactivate(ctx, cfg.DeploymentID); err != nil {
			return fmt.Errorf("instance stopped but backend credential remains active; local files were retained for retry: %w", err)
		}
		if err := os.Remove(unitPath(cfg.Slug)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err := os.RemoveAll(filepath.Join(Root(), cfg.Slug)); err != nil {
			return err
		}
		if err := systemctl("daemon-reload"); err != nil {
			return err
		}
		fmt.Fprintln(m.out, "Instance service and credentials removed. Its bot API credential was revoked; accounts and commerce history were retained. Backend workers will continue existing queued and uncertain work for reconciliation.")
		return nil
	default:
		return errors.New("invalid selection")
	}
}

func (m menu) globalBackup(ctx context.Context) error {
	action, err := m.ask("1) Create full global backup  2) Restore global database and stage configs  3) Validate restore (dry run): ")
	if err != nil {
		return err
	}
	values, err := readEnvFile(backendEnvPath())
	if err != nil {
		return err
	}
	if values["DATABASE_URL"] == "" {
		return errors.New("DATABASE_URL is missing from backend environment file")
	}
	path, err := m.ask("Archive path: ")
	if err != nil {
		return err
	}
	if action == "1" {
		return backup.CreateGlobal(ctx, values["DATABASE_URL"], Root(), path)
	}
	if action != "2" && action != "3" {
		return errors.New("invalid selection")
	}
	dry := action == "3"
	if !dry {
		confirm, err := m.ask("Restores into the configured empty target database; existing data is refused. Type RESTORE to continue: ")
		if err != nil {
			return err
		}
		if confirm != "RESTORE" {
			return nil
		}
	}
	stage, err := backup.StageGlobalConfig(path, restoreStageRoot(), dry)
	if err != nil {
		return err
	}
	if err := backup.RestoreGlobal(ctx, values["DATABASE_URL"], path, dry); err != nil {
		if !dry {
			fmt.Fprintln(m.out, "Configuration was staged at:", stage)
		}
		return err
	}
	if dry {
		fmt.Fprintln(m.out, "Archive validated. Config staging path after restore:", stage)
		return nil
	}
	fmt.Fprintln(m.out, "Database restored. Archived configuration was staged for review at:", stage)
	fmt.Fprintln(m.out, "Review backend.env and instance files there, then manually rebind and install selected configs. Nothing was activated.")
	return nil
}

func (m menu) instanceBackup() error {
	action, err := m.ask("1) Back up instance configuration  2) Restore instance configuration: ")
	if err != nil {
		return err
	}
	if action == "1" {
		slug, err := m.ask("Instance name: ")
		if err != nil {
			return err
		}
		path, err := m.ask("Archive path: ")
		if err != nil {
			return err
		}
		fmt.Fprintln(m.out, "This contains configuration and credentials only; it excludes customer and commerce history.")
		return backup.CreateInstanceConfig(Root(), slug, path)
	}
	if action != "2" {
		return errors.New("invalid selection")
	}
	path, err := m.ask("Archive path: ")
	if err != nil {
		return err
	}
	slug, err := m.ask("New instance name: ")
	if err != nil {
		return err
	}
	confirm, err := m.ask("This stages secrets outside the live instance registry; no bot will be created or started. Customer and commerce history is not included. Type RESTORE: ")
	if err != nil {
		return err
	}
	if confirm != "RESTORE" {
		return nil
	}
	stage, err := backup.StageInstanceConfig(path, restoreStageRoot(), slug, false)
	if err != nil {
		return err
	}
	fmt.Fprintln(m.out, "Inactive instance configuration staged at:", stage)
	fmt.Fprintln(m.out, "Review the secrets and panel settings there before manually registering the instance. No service was created or started.")
	return nil
}

func (m menu) ask(prompt string) (string, error) {
	fmt.Fprint(m.out, prompt)
	line, err := m.in.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	return strings.TrimSpace(line), nil
}

func (m menu) secret(prompt string) (string, error) {
	fmt.Fprint(m.out, prompt)
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return "", errors.New("secret input requires an interactive terminal")
	}
	value, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(m.out)
	if err != nil {
		return "", err
	}
	return string(value), nil
}

func validSlug(s string) bool {
	if len(s) < 2 || len(s) > 48 || s[0] == '-' || s[len(s)-1] == '-' {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
			return false
		}
	}
	return true
}

func validTelegramToken(s string) bool {
	if len(s) < 30 || len(s) > 128 {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == ':' || r == '_' || r == '-') {
			return false
		}
	}
	return strings.Contains(s, ":")
}

func validBackendURL(raw string) bool {
	u, err := url.ParseRequestURI(strings.TrimSpace(raw))
	return err == nil && (u.Scheme == "https" || u.Scheme == "http") && u.Host != "" && u.User == nil && u.RawQuery == "" && u.Fragment == ""
}

func mask(s string) string {
	if len(s) < 8 {
		return "[hidden]"
	}
	return s[:4] + strings.Repeat("*", len(s)-8) + s[len(s)-4:]
}

func newResourceID(prefix string) (string, error) {
	var random [8]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	return prefix + "-" + hex.EncodeToString(random[:]), nil
}

func compensateRegistration(cfg Config, cause error) error {
	return compensateRegistrationWith(cfg, cause, func() error {
		revokeCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		return deactivate(revokeCtx, cfg.DeploymentID)
	}, func() error {
		var cleanupErr error
		if _, statErr := os.Stat(unitPath(cfg.Slug)); statErr == nil {
			if err := systemctl("disable", "--now", unitName(cfg.Slug)); err != nil {
				cleanupErr = err
			}
		} else if !errors.Is(statErr, os.ErrNotExist) {
			cleanupErr = statErr
		}
		if err := os.Remove(unitPath(cfg.Slug)); err != nil && !errors.Is(err, os.ErrNotExist) && cleanupErr == nil {
			cleanupErr = err
		}
		if err := os.RemoveAll(filepath.Join(Root(), cfg.Slug)); err != nil && cleanupErr == nil {
			cleanupErr = err
		}
		if err := systemctl("daemon-reload"); err != nil && cleanupErr == nil {
			cleanupErr = err
		}
		return cleanupErr
	})
}

func compensateRegistrationWith(cfg Config, cause error, revoke func() error, cleanup func() error) error {
	if err := revoke(); err != nil && !errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("setup failed and deployment %s could not be revoked; local config was retained at %s for recovery: %w", cfg.DeploymentID, filepath.Join(Root(), cfg.Slug), err)
	}
	if err := cleanup(); err != nil {
		return fmt.Errorf("deployment %s was revoked after setup failed (%v), but local cleanup needs review: %w", cfg.DeploymentID, cause, err)
	}
	return fmt.Errorf("instance setup failed; deployment %s was revoked and its local files were removed: %w", cfg.DeploymentID, cause)
}

func save(cfg Config) error {
	root, err := filepath.Abs(Root())
	if err != nil {
		return err
	}
	dir := filepath.Join(root, cfg.Slug)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	if err := os.Chmod(root, 0710); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0700); err != nil {
		return err
	}
	values := map[string]string{"INSTANCE_ID": cfg.Slug, "CHANNEL": cfg.Channel, "DISPLAY_NAME": cfg.DisplayName, "BACKEND_DEPLOYMENT_ID": cfg.DeploymentID, "BACKEND_URL": cfg.BackendURL, "BACKEND_TOKEN": cfg.BackendToken, "TELEGRAM_BOT_TOKEN": cfg.BotToken, "ADMIN_TELEGRAM_ID": strconv.FormatInt(cfg.AdminID, 10)}
	var b strings.Builder
	for _, key := range []string{"INSTANCE_ID", "CHANNEL", "DISPLAY_NAME", "BACKEND_DEPLOYMENT_ID", "BACKEND_URL", "BACKEND_TOKEN", "TELEGRAM_BOT_TOKEN", "ADMIN_TELEGRAM_ID"} {
		fmt.Fprintf(&b, "%s=%s\n", key, strconv.Quote(values[key]))
	}
	envPath := filepath.Join(dir, "instance.env")
	if err := os.WriteFile(envPath, []byte(b.String()), 0600); err != nil {
		return err
	}
	metadata := struct {
		Slug         string `json:"slug"`
		Channel      string `json:"channel"`
		DisplayName  string `json:"display_name"`
		DeploymentID string `json:"deployment_id"`
		BackendURL   string `json:"backend_url"`
		AdminID      int64  `json:"admin_telegram_id"`
	}{cfg.Slug, cfg.Channel, cfg.DisplayName, cfg.DeploymentID, cfg.BackendURL, cfg.AdminID}
	data, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		return err
	}
	metaPath := filepath.Join(dir, "metadata.json")
	if err = os.WriteFile(metaPath, data, 0600); err != nil {
		return err
	}
	if serviceUser, err := user.Lookup("xui-backend"); err == nil {
		uid, _ := strconv.Atoi(serviceUser.Uid)
		gid, _ := strconv.Atoi(serviceUser.Gid)
		if err := os.Chown(root, 0, gid); err != nil {
			return err
		}
		if err := os.Chown(dir, uid, gid); err != nil {
			return err
		}
		if err := os.Chown(envPath, uid, gid); err != nil {
			return err
		}
		if err := os.Chown(metaPath, uid, gid); err != nil {
			return err
		}
	}
	return os.Chmod(metaPath, 0600)
}

func Read(slug string) (Config, error) {
	if !validSlug(slug) {
		return Config{}, errors.New("invalid instance slug")
	}
	path := filepath.Join(Root(), slug, "instance.env")
	values, err := readEnvFile(path)
	if err != nil {
		return Config{}, err
	}
	cfg := Config{Slug: slug, Channel: values["CHANNEL"], DisplayName: values["DISPLAY_NAME"], DeploymentID: values["BACKEND_DEPLOYMENT_ID"], BackendURL: values["BACKEND_URL"], BackendToken: values["BACKEND_TOKEN"], BotToken: values["TELEGRAM_BOT_TOKEN"], AdminID: parseID(values["ADMIN_TELEGRAM_ID"])}
	if cfg.Channel != "retail" && cfg.Channel != "reseller" {
		return Config{}, errors.New("instance has an unsupported bot channel")
	}
	if !validBackendURL(cfg.BackendURL) || cfg.BackendToken == "" || !validTelegramToken(cfg.BotToken) || cfg.AdminID <= 0 || cfg.DeploymentID == "" {
		return Config{}, errors.New("instance configuration is incomplete or invalid")
	}
	return cfg, nil
}

func ApplyEnvironment(cfg Config) error {
	for key, value := range map[string]string{"TELEGRAM_BOT_TOKEN": cfg.BotToken, "BACKEND_URL": cfg.BackendURL, "BACKEND_TOKEN": cfg.BackendToken, "ADMIN_TELEGRAM_ID": strconv.FormatInt(cfg.AdminID, 10)} {
		if err := os.Setenv(key, value); err != nil {
			return err
		}
	}
	return nil
}

type Listed struct{ Slug, Channel, DeploymentID string }

func List() ([]Listed, error) {
	entries, err := os.ReadDir(Root())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	out := make([]Listed, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() || !validSlug(entry.Name()) {
			continue
		}
		cfg, err := Read(entry.Name())
		if err != nil {
			continue
		}
		out = append(out, Listed{cfg.Slug, cfg.Channel, cfg.DeploymentID})
	}
	return out, nil
}

func parseID(raw string) int64 { value, _ := strconv.ParseInt(raw, 10, 64); return value }

func secureInstanceFiles(slug string) error {
	if !validSlug(slug) {
		return errors.New("invalid instance slug")
	}
	root := Root()
	dir := filepath.Join(root, slug)
	if err := os.Chmod(root, 0710); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0700); err != nil {
		return err
	}
	for _, name := range []string{"instance.env", "metadata.json"} {
		path := filepath.Join(dir, name)
		if err := os.Chmod(path, 0600); err != nil {
			return err
		}
	}
	if serviceUser, err := user.Lookup("xui-backend"); err == nil {
		uid, _ := strconv.Atoi(serviceUser.Uid)
		gid, _ := strconv.Atoi(serviceUser.Gid)
		if err := os.Chown(root, 0, gid); err != nil {
			return err
		}
		if err := os.Chown(dir, uid, gid); err != nil {
			return err
		}
		for _, name := range []string{"instance.env", "metadata.json"} {
			if err := os.Chown(filepath.Join(dir, name), uid, gid); err != nil {
				return err
			}
		}
	}
	return nil
}

func deactivate(ctx context.Context, deploymentID string) error {
	values, err := readEnvFile(backendEnvPath())
	if err != nil {
		return errors.New("cannot read backend database configuration")
	}
	if values["DATABASE_URL"] == "" {
		return errors.New("DATABASE_URL is missing from backend environment file")
	}
	deactivateCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	pool, err := pgxpool.New(deactivateCtx, values["DATABASE_URL"])
	if err != nil {
		return errors.New("cannot configure backend database connection")
	}
	defer pool.Close()
	if err := pool.Ping(deactivateCtx); err != nil {
		return errors.New("cannot reach backend database")
	}
	return (&store.Store{DB: pool}).DeactivateInstance(deactivateCtx, deploymentID)
}

func readEnvFile(path string) (map[string]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read protected configuration %s: %w", path, err)
	}
	out := make(map[string]string)
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		i := strings.IndexByte(line, '=')
		if i <= 0 {
			continue
		}
		key, value := strings.TrimSpace(line[:i]), strings.TrimSpace(line[i+1:])
		if strings.HasPrefix(value, "\"") {
			parsed, err := strconv.Unquote(value)
			if err != nil {
				return nil, fmt.Errorf("invalid value for %s in configuration", key)
			}
			value = parsed
		}
		out[key] = value
	}
	return out, nil
}

func unitName(slug string) string { return "xui-backend-instance-" + slug + ".service" }
func unitPath(slug string) string {
	if p := os.Getenv("XUI_BACKEND_SYSTEMD_DIR"); p != "" {
		return filepath.Join(p, unitName(slug))
	}
	return filepath.Join("/etc/systemd/system", unitName(slug))
}

func installUnit(cfg Config) error {
	content := fmt.Sprintf("[Unit]\nDescription=xui-backend bot instance %s\nAfter=network-online.target xui-backend.service\nWants=network-online.target\n\n[Service]\nType=simple\nExecStart=/usr/local/bin/xui-backend run-instance %s\nRestart=on-failure\nRestartSec=5\nUser=xui-backend\nGroup=xui-backend\nUMask=0077\nNoNewPrivileges=true\nPrivateTmp=true\nProtectSystem=strict\nProtectHome=true\n\n[Install]\nWantedBy=multi-user.target\n", cfg.Slug, cfg.Slug)
	path := unitPath(cfg.Slug)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		return err
	}
	return nil
}

func systemctl(args ...string) error {
	cmd := exec.Command("systemctl", args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemctl %s failed: %s", strings.Join(args, " "), strings.TrimSpace(string(output)))
	}
	return nil
}
