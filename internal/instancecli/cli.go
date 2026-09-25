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
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"example.com/xui-commerce/backend/internal/backup"
	"example.com/xui-commerce/backend/internal/globalbackup"
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

type transferState struct {
	DeploymentID string `json:"deployment_id"`
	WasActive    bool   `json:"was_active"`
	WasEnabled   bool   `json:"was_enabled"`
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

// localBackendURL derives the bot-facing loopback URL from the exact address
// configured for the backend service, preventing an accidental stale port.
func localBackendURL(listenAddr string) (string, bool) {
	host, port, err := net.SplitHostPort(strings.TrimSpace(listenAddr))
	if err != nil || port == "" {
		return "", false
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsUnspecified() {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port), true
}

func Menu(ctx context.Context) error {
	m := menu{in: bufio.NewReader(os.Stdin), out: os.Stdout}
	for {
		fmt.Fprintln(m.out, "\nxui-backend")
		fmt.Fprintln(m.out, "1) Add bot instance")
		fmt.Fprintln(m.out, "2) List/manage instances")
		fmt.Fprintln(m.out, "3) Global backup or restore")
		fmt.Fprintln(m.out, "4) Complete instance move backup or restore")
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
			err = m.instanceBackup(ctx)
		default:
			fmt.Fprintln(m.out, "Choose one of the displayed options.")
		}
		if err != nil {
			fmt.Fprintln(m.out, "Error:", err)
		}
	}
}

func Command(args []string) error {
	if len(args) >= 3 && args[0] == "transfer" {
		switch args[1] {
		case "resume":
			if len(args) != 4 || args[3] != "--destination-stopped" {
				return errors.New("usage: xui-backend transfer resume <source-slug> --destination-stopped")
			}
			return resumeTransfer(context.Background(), args[2])
		case "inspect":
			if len(args) != 3 {
				return errors.New("usage: xui-backend transfer inspect <instance-slug>")
			}
			return inspectTransfer(context.Background(), args[2])
		case "release-safe":
			if len(args) != 3 {
				return errors.New("usage: xui-backend transfer release-safe <instance-slug>")
			}
			return releaseSafeTransferWork(context.Background(), args[2])
		}
	}
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
			return globalbackup.Create(context.Background(), globalbackup.CreateOptions{DatabaseURL: values["DATABASE_URL"], BackendEnvPath: backendEnvPath(), InstanceRoot: Root(), UnitRoot: filepath.Dir(unitPath("probe")), Destination: args[2]})
		case "global-move":
			if len(args) != 4 || args[3] != "--stop-source" {
				return errors.New("usage: xui-backend backup global-move <archive-path> --stop-source")
			}
			values, err := readEnvFile(backendEnvPath())
			if err != nil {
				return err
			}
			return globalbackup.Create(context.Background(), globalbackup.CreateOptions{DatabaseURL: values["DATABASE_URL"], BackendEnvPath: backendEnvPath(), InstanceRoot: Root(), UnitRoot: filepath.Dir(unitPath("probe")), Destination: args[2], QuiesceForMove: true})
		case "instance":
			if len(args) != 4 {
				return errors.New("usage: xui-backend backup instance <instance-slug> <archive-path>")
			}
			return snapshotInstance(context.Background(), args[2], args[3])
		case "instance-config":
			if len(args) != 4 {
				return errors.New("usage: xui-backend backup instance-config <slug> <archive-path>")
			}
			return backup.CreateInstanceConfig(Root(), args[2], args[3])
		}
	}
	if len(args) >= 2 && args[0] == "restore" {
		if len(args) >= 4 && args[1] == "instance" {
			if len(args) == 5 && args[4] == "--dry-run" {
				return restoreInstance(context.Background(), args[2], args[3], true)
			}
			if len(args) == 5 && args[4] == "--source-stopped" {
				return restoreInstance(context.Background(), args[2], args[3], false)
			}
			return errors.New("usage: xui-backend restore instance <archive> <new-instance-slug> [--dry-run|--source-stopped]")
		}
		if args[1] == "global-recover" {
			if len(args) != 4 {
				return errors.New("usage: xui-backend restore global-recover <archive> <stage>")
			}
			values, err := readEnvFile(backendEnvPath())
			if err != nil {
				return err
			}
			return globalbackup.Recover(context.Background(), globalbackup.RecoverOptions{DatabaseURL: values["DATABASE_URL"], Archive: args[2], Stage: args[3], BackendEnvPath: backendEnvPath(), InstanceRoot: Root(), UnitRoot: filepath.Dir(unitPath("probe"))})
		}
		if args[1] == "global" {
			if len(args) < 3 {
				return errors.New("usage: xui-backend restore global <archive> [--dry-run|--source-stopped] [--activate]")
			}
			dry, sourceStopped, activate := false, false, false
			for _, option := range args[3:] {
				switch option {
				case "--dry-run":
					dry = true
				case "--source-stopped":
					sourceStopped = true
				case "--activate":
					activate = true
				default:
					return errors.New("unknown global restore option")
				}
			}
			if dry && (sourceStopped || activate) {
				return errors.New("--dry-run cannot be combined with activation options")
			}
			if !dry && !sourceStopped {
				return errors.New("global restore requires --source-stopped after you have stopped the source services")
			}
			values, err := readEnvFile(backendEnvPath())
			if err != nil {
				return err
			}
			opts := globalbackup.RestoreOptions{DatabaseURL: values["DATABASE_URL"], Archive: args[2], BackendEnvPath: backendEnvPath(), InstanceRoot: Root(), UnitRoot: filepath.Dir(unitPath("probe")), DryRun: dry, Activate: activate}
			if dry {
				plan, err := globalbackup.Preflight(context.Background(), opts)
				if err != nil {
					return err
				}
				fmt.Printf("Global restore preflight: %s; backend address %s; instances %s; database-only/bootstrap deployments %s\n", plan.DatabaseMode, plan.ListenAddr, strings.Join(plan.Instances, ", "), formatIDs(plan.DatabaseOnlyDeployments))
				return nil
			}
			plan, err := globalbackup.Restore(context.Background(), opts)
			if err != nil {
				return err
			}
			fmt.Printf("Global restore complete; backend address %s; instances %s; database-only/bootstrap deployments %s. Restored work items and notifications are quarantined for review.\n", plan.ListenAddr, strings.Join(plan.Instances, ", "), formatIDs(plan.DatabaseOnlyDeployments))
			if !activate {
				fmt.Println("Services remain inactive. After reviewing DB/config and queued work, run systemctl daemon-reload, then start xui-backend.service and the instance services with systemctl.")
			}
			return nil
		}
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
	return errors.New("usage: xui-backend [backup global <path>|backup global-move <path> --stop-source|backup instance <slug> <path>|backup instance-config <slug> <path>|restore global <archive> [--dry-run|--source-stopped] [--activate]|restore global-recover <archive> <stage>|restore instance <archive> <new-slug> [--dry-run|--source-stopped]|transfer inspect <slug>|transfer release-safe <slug>|transfer resume <slug> --destination-stopped]")
}

func formatIDs(ids []string) string {
	if len(ids) == 0 {
		return "none"
	}
	return strings.Join(ids, ", ")
}

func resumeTransfer(parent context.Context, slug string) error {
	cfg, err := Read(slug)
	if err != nil {
		return err
	}
	state, err := readTransferState(slug)
	if err != nil || state.DeploymentID != cfg.DeploymentID {
		return errors.New("source transfer state is missing or does not match this deployment")
	}
	unitStatus := serviceStatus(slug)
	if unitStatus != "inactive" && unitStatus != "failed" {
		return errors.New("destination-stopped confirmation requires the source instance service to be inactive")
	}
	values, err := readEnvFile(backendEnvPath())
	if err != nil {
		return err
	}
	if isLocalBackend(cfg.BackendURL) {
		if err = systemctl("start", "xui-backend.service"); err != nil {
			return err
		}
	}
	if err = checkBackendHealth(parent, cfg.BackendURL); err != nil {
		return err
	}
	pool, err := pgxpool.New(parent, values["DATABASE_URL"])
	if err != nil {
		return errors.New("cannot configure backend database connection")
	}
	defer pool.Close()
	if _, err = pool.Exec(parent, `UPDATE deployments SET transfer_frozen=false WHERE id=$1`, cfg.DeploymentID); err != nil {
		return errors.New("could not resume source deployment")
	}
	if err = checkBackendIdentity(parent, cfg); err != nil {
		_, _ = pool.Exec(context.Background(), `UPDATE deployments SET transfer_frozen=true WHERE id=$1`, cfg.DeploymentID)
		return errors.New("source credential/admin check failed; deployment remains frozen")
	}
	if state.WasEnabled {
		if err = systemctl("enable", unitName(slug)); err != nil {
			_, _ = pool.Exec(context.Background(), `UPDATE deployments SET transfer_frozen=true WHERE id=$1`, cfg.DeploymentID)
			return err
		}
	}
	if state.WasActive {
		if err = systemctl("start", unitName(slug)); err != nil {
			_, _ = pool.Exec(context.Background(), `UPDATE deployments SET transfer_frozen=true WHERE id=$1`, cfg.DeploymentID)
			return err
		}
	}
	_ = os.Remove(transferStatePath(slug))
	fmt.Println("Source instance resumed after explicit confirmation that the target is stopped.")
	return nil
}

func transferPool(parent context.Context, slug string) (*pgxpool.Pool, string, error) {
	cfg, err := Read(slug)
	if err != nil {
		return nil, "", err
	}
	values, err := readEnvFile(backendEnvPath())
	if err != nil {
		return nil, "", err
	}
	pool, err := pgxpool.New(parent, values["DATABASE_URL"])
	if err != nil {
		return nil, "", errors.New("cannot configure backend database connection")
	}
	return pool, cfg.DeploymentID, nil
}

func inspectTransfer(parent context.Context, slug string) error {
	pool, deployment, err := transferPool(parent, slug)
	if err != nil {
		return err
	}
	defer pool.Close()
	rows, err := pool.Query(parent, `SELECT id,status,phase,attempts FROM work_items WHERE deployment_id=$1 AND restore_quarantined ORDER BY id`, deployment)
	if err != nil {
		return err
	}
	fmt.Printf("Held work for %s:\n", slug)
	for rows.Next() {
		var id int64
		var status, phase string
		var attempts int
		if err = rows.Scan(&id, &status, &phase, &attempts); err != nil {
			return err
		}
		fmt.Printf("  work_item id=%d status=%s phase=%s attempts=%d\n", id, status, phase, attempts)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	rows, err = pool.Query(parent, `SELECT id,status,attempts FROM outbox WHERE deployment_id=$1 AND restore_quarantined ORDER BY id`, deployment)
	if err != nil {
		return err
	}
	defer rows.Close()
	fmt.Printf("Held notifications for %s:\n", slug)
	for rows.Next() {
		var id int64
		var status string
		var attempts int
		if err = rows.Scan(&id, &status, &attempts); err != nil {
			return err
		}
		fmt.Printf("  outbox id=%d status=%s attempts=%d\n", id, status, attempts)
	}
	return rows.Err()
}

func releaseSafeTransferWork(parent context.Context, slug string) error {
	pool, deployment, err := transferPool(parent, slug)
	if err != nil {
		return err
	}
	defer pool.Close()
	return releaseSafeDeployment(parent, pool, deployment)
}

func releaseSafeDeployment(ctx context.Context, pool *pgxpool.Pool, deployment string) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return errors.New("could not begin safe queue release")
	}
	defer tx.Rollback(ctx)
	workTag, err := tx.Exec(ctx, `UPDATE work_items SET restore_quarantined=false WHERE deployment_id=$1 AND restore_quarantined AND status='pending' AND phase='ready' AND attempts=0`, deployment)
	if err != nil {
		return errors.New("could not release safe pending work")
	}
	outboxTag, err := tx.Exec(ctx, `UPDATE outbox SET restore_quarantined=false WHERE deployment_id=$1 AND restore_quarantined AND status='pending' AND attempts=0`, deployment)
	if err != nil {
		return errors.New("could not release safe pending notifications")
	}
	if err = tx.Commit(ctx); err != nil {
		return errors.New("safe queue release did not commit")
	}
	fmt.Printf("Released %d never-attempted work items and %d never-attempted notifications; uncertain items remain quarantined.\n", workTag.RowsAffected(), outboxTag.RowsAffected())
	return nil
}

func unfreezeRestoredDeployment(ctx context.Context, pool *pgxpool.Pool, deployment, fingerprint string) (restoredUnfreezeState, error) {
	tag, err := pool.Exec(ctx, `UPDATE deployments SET transfer_frozen=false WHERE id=$1 AND restore_fingerprint=$2 AND transfer_frozen=true`, deployment, fingerprint)
	if err == nil && tag.RowsAffected() == 1 {
		return restoredUnfreezeUnfrozen, nil
	}
	verifyCtx, verifyCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer verifyCancel()
	var stillFrozen bool
	verifyErr := pool.QueryRow(verifyCtx, `SELECT transfer_frozen FROM deployments WHERE id=$1 AND restore_fingerprint=$2`, deployment, fingerprint).Scan(&stillFrozen)
	if verifyErr != nil {
		return restoredUnfreezeUnknown, fmt.Errorf("restore activation state is uncertain; final transfer-freeze state could not be read for deployment %s", deployment)
	}
	if !stillFrozen {
		if err != nil {
			return restoredUnfreezeUnfrozen, fmt.Errorf("final transfer-freeze update reported an error after activation became visible; activation may already be visible: %w", err)
		}
		return restoredUnfreezeUnfrozen, errors.New("final transfer-freeze update did not change the restored deployment, but activation is already visible")
	}
	if err != nil {
		return restoredUnfreezeFrozen, fmt.Errorf("transfer freeze update failed; deployment remains frozen: %w", err)
	}
	return restoredUnfreezeFrozen, errors.New("transfer freeze update did not change the restored deployment; deployment remains frozen")
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
	backendValues, err := readEnvFile(backendEnvPath())
	if err != nil {
		return err
	}
	if backendValues["DATABASE_URL"] == "" {
		return errors.New("DATABASE_URL is missing from backend environment file")
	}
	localURL, localConfigured := localBackendURL(backendValues["BACKEND_LISTEN_ADDR"])
	prompt := "Backend URL (remote URL required): "
	if localConfigured {
		prompt = "Backend URL (press Enter to use configured local backend " + localURL + ", or enter a remote URL): "
	}
	backendURL, err := m.ask(prompt)
	if err != nil {
		return err
	}
	if backendURL == "" && localConfigured {
		backendURL = localURL
	}
	if !validBackendTransport(backendURL) {
		return errors.New("backend URL must use HTTPS remotely or HTTP only on loopback, without credentials, query, or fragment")
	}
	if localConfigured && isLocalBackend(backendURL) && strings.TrimRight(backendURL, "/") != localURL {
		return errors.New("local backend URL must use the configured listen port " + localURL)
	}
	if err := checkBackendHealth(ctx, backendURL); err != nil {
		return err
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
	if err := checkBackendIdentity(ctx, cfg); err != nil {
		return compensateRegistration(cfg, errors.New("backend URL or generated credential did not authenticate the registered deployment"))
	}
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
	action, err := m.ask("1) Create global backup  2) Create move backup and stop source services  3) Dry-run global restore  4) Restore global system: ")
	if err != nil {
		return err
	}
	path, err := m.ask("Archive path: ")
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
	create := func(move bool) error {
		if move {
			confirm, e := m.ask("This stops active source bot and backend services and leaves them stopped after a successful archive. Type MOVE to continue: ")
			if e != nil {
				return e
			}
			if confirm != "MOVE" {
				return nil
			}
		}
		return globalbackup.Create(ctx, globalbackup.CreateOptions{DatabaseURL: values["DATABASE_URL"], BackendEnvPath: backendEnvPath(), InstanceRoot: Root(), UnitRoot: filepath.Dir(unitPath("probe")), Destination: path, QuiesceForMove: move})
	}
	if action == "1" {
		return create(false)
	}
	if action == "2" {
		return create(true)
	}
	if action != "3" && action != "4" {
		return errors.New("invalid selection")
	}
	if action == "4" {
		confirm, e := m.ask("Confirm the source server's backend and all bot services are stopped. Type SOURCE_STOPPED: ")
		if e != nil {
			return e
		}
		if confirm != "SOURCE_STOPPED" {
			return nil
		}
	}
	dry := action == "3"
	opts := globalbackup.RestoreOptions{DatabaseURL: values["DATABASE_URL"], Archive: path, BackendEnvPath: backendEnvPath(), InstanceRoot: Root(), UnitRoot: filepath.Dir(unitPath("probe")), DryRun: dry}
	if dry {
		plan, e := globalbackup.Preflight(ctx, opts)
		if e != nil {
			return e
		}
		fmt.Fprintf(m.out, "Preflight passed: target database %s; backend will listen at %s; instances: %s\n", plan.DatabaseMode, plan.ListenAddr, strings.Join(plan.Instances, ", "))
		return nil
	}
	activate, err := m.ask("After database and config validation, start the restored backend and source-active bots now? (y/N): ")
	if err != nil {
		return err
	}
	opts.Activate = strings.EqualFold(activate, "y") || strings.EqualFold(activate, "yes")
	if _, err := globalbackup.Preflight(ctx, opts); err != nil {
		return err
	}
	plan, err := globalbackup.Restore(ctx, opts)
	if err != nil {
		return err
	}
	fmt.Fprintf(m.out, "Global system restored; backend address %s; instances: %s. Pending panel work and Telegram notifications remain quarantined until reviewed.\n", plan.ListenAddr, strings.Join(plan.Instances, ", "))
	if !opts.Activate {
		fmt.Fprintln(m.out, "Services remain inactive. Review the restored database and configs, then start xui-backend.service and the instance services.")
	}
	return nil
}

func (m menu) instanceBackup(ctx context.Context) error {
	action, err := m.ask("1) Back up complete instance and commercial data  2) Move/restore complete instance: ")
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
		return snapshotInstance(ctx, slug, path)
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
	confirm, err := m.ask("This imports all commercial history and will activate the bot. Confirm the source bot and workers are stopped. Type SOURCE-STOPPED: ")
	if err != nil {
		return err
	}
	if confirm != "SOURCE-STOPPED" {
		return nil
	}
	return restoreInstance(ctx, path, slug, false)
}

func snapshotInstance(ctx context.Context, slug, archive string) (retErr error) {
	if !validSlug(slug) {
		return errors.New("invalid instance slug")
	}
	cfg, err := Read(slug)
	if err != nil {
		return err
	}
	probe := exec.Command("systemctl", "is-active", unitName(slug))
	out, _ := probe.CombinedOutput()
	state := strings.TrimSpace(string(out))
	wasActive := state == "active"
	if !wasActive && state != "inactive" && state != "failed" {
		return errors.New("could not verify the source instance service state")
	}
	wasEnabled, stateKnown := serviceIsEnabled(slug)
	if !stateKnown {
		return errors.New("could not verify the source instance boot-enabled state")
	}
	if wasActive {
		if err = systemctl("stop", unitName(slug)); err != nil {
			return err
		}
	}
	values, err := readEnvFile(backendEnvPath())
	if err != nil {
		if wasActive {
			_ = systemctl("start", unitName(slug))
		}
		return err
	}
	pool, err := pgxpool.New(ctx, values["DATABASE_URL"])
	if err != nil {
		if wasActive {
			_ = systemctl("start", unitName(slug))
		}
		return errors.New("cannot configure backend database connection")
	}
	defer pool.Close()
	var wasFrozen bool
	if err = pool.QueryRow(ctx, `SELECT transfer_frozen FROM deployments WHERE id=$1`, cfg.DeploymentID).Scan(&wasFrozen); err != nil {
		if wasActive {
			_ = systemctl("start", unitName(slug))
		}
		return errors.New("could not inspect source transfer state")
	}
	if _, err = pool.Exec(ctx, `UPDATE deployments SET transfer_frozen=true WHERE id=$1`, cfg.DeploymentID); err != nil {
		if wasActive {
			_ = systemctl("start", unitName(slug))
		}
		return errors.New("could not freeze source deployment workers")
	}
	resume := true
	defer func() {
		if resume {
			if !wasFrozen {
				_, _ = pool.Exec(context.Background(), `UPDATE deployments SET transfer_frozen=false WHERE id=$1`, cfg.DeploymentID)
			}
			if wasActive && !wasFrozen {
				_ = systemctl("start", unitName(slug))
			}
			if wasEnabled {
				_ = systemctl("enable", unitName(slug))
			}
			_ = os.Remove(transferStatePath(slug))
		}
	}()
	stateFile := transferState{DeploymentID: cfg.DeploymentID, WasActive: wasActive, WasEnabled: wasEnabled}
	stateData, err := json.Marshal(stateFile)
	if err != nil {
		return errors.New("could not record source transfer rollback state")
	}
	if err = os.WriteFile(transferStatePath(slug), stateData, 0600); err != nil {
		return errors.New("could not persist source transfer rollback state")
	}
	transferCtx, transferCancel := context.WithTimeout(ctx, 5*time.Minute)
	transferLease, err := (&store.Store{DB: pool}).AcquireDeploymentTransferLease(transferCtx, cfg.DeploymentID)
	transferCancel()
	if err != nil {
		return errors.New("could not drain in-flight deployment API requests")
	}
	defer func() {
		releaseCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if releaseErr := transferLease.Release(releaseCtx); releaseErr != nil {
			retErr = errors.Join(retErr, errors.New("could not release deployment transfer lock"))
		}
	}()
	deadline := time.Now().Add(90 * time.Second)
	for {
		var busy int
		err = pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM work_items WHERE deployment_id=$1 AND status='running' AND lease_until>now())+(SELECT count(*) FROM outbox WHERE deployment_id=$1 AND status='sending' AND lease_until>now())`, cfg.DeploymentID).Scan(&busy)
		if err != nil {
			return errors.New("could not verify source worker drain")
		}
		if busy == 0 {
			break
		}
		if time.Now().After(deadline) {
			return errors.New("source workers did not drain; source service and worker freeze were rolled back")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	callCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	report, err := backup.CreateInstance(callCtx, pool, Root(), slug, archive)
	if err != nil {
		return err
	}
	if wasEnabled {
		if err = systemctl("disable", unitName(slug)); err != nil {
			return errors.New("backup was created but source bot boot startup could not be disabled")
		}
	}
	resume = false
	fmt.Printf("Complete backup created for deployment %s (%d table groups). Source bot remains stopped and backend workers frozen for cutover. Uncertain work: %d; uncertain outbox sends: %d. To roll back after stopping the target, use `xui-backend transfer resume %s --destination-stopped`.\n", report.Deployment, len(report.Counts), report.UncertainWorkItems, report.UncertainOutbox, slug)
	return nil
}

func restoreInstance(parent context.Context, archive, slug string, dryRun bool) error {
	if !validSlug(slug) {
		return errors.New("invalid target instance slug")
	}
	manifest, archived, err := backup.InspectInstance(archive)
	if err != nil {
		return err
	}
	instanceDir := filepath.Join(Root(), slug)
	localExists := false
	if _, err = os.Stat(instanceDir); err == nil {
		existing, readErr := Read(slug)
		if readErr != nil || existing.DeploymentID != archived.Deployment {
			return errors.New("target instance directory exists but does not match this archive")
		}
		localExists = true
		if serviceIsActive(slug) {
			return errors.New("target instance service is active; stop it before resuming restore")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if _, err = os.Stat(unitPath(slug)); err == nil && !localExists {
		return errors.New("target instance service unit exists without matching local instance configuration")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	values, err := readEnvFile(backendEnvPath())
	if err != nil {
		return err
	}
	if values["DATABASE_URL"] == "" {
		return errors.New("DATABASE_URL is missing from backend environment file")
	}
	targetURL, ok := localBackendURL(values["BACKEND_LISTEN_ADDR"])
	if !ok {
		return errors.New("target backend listen address is missing or invalid")
	}
	if err = checkBackendHealth(parent, targetURL); err != nil {
		return err
	}
	pool, err := pgxpool.New(parent, values["DATABASE_URL"])
	if err != nil {
		return errors.New("cannot configure backend database connection")
	}
	defer pool.Close()
	if err = pool.Ping(parent); err != nil {
		return errors.New("cannot reach target backend database")
	}
	ctx, cancel := context.WithTimeout(parent, 5*time.Minute)
	defer cancel()
	report, err := backup.RestoreInstance(ctx, pool, archive, slug, values["BACKEND_PANEL_SECRETS_KEY"], true)
	if err != nil {
		return fmt.Errorf("instance restore preflight failed: %w", err)
	}
	if dryRun {
		fmt.Printf("Instance restore preflight passed for deployment %s (%d table groups); target backend %s; panel identity collisions checked and required client/inbound readbacks matched.\n", report.Deployment, len(report.Counts), targetURL)
		return nil
	}
	stage, err := backup.StagePortableInstanceConfig(archive, restoreStageRoot(), slug, targetURL, false)
	if err != nil {
		return err
	}
	defer os.RemoveAll(filepath.Dir(stage))
	configValues, err := readEnvFile(filepath.Join(stage, "instance.env"))
	if err != nil {
		return errors.New("could not read staged instance configuration")
	}
	cfg := Config{Slug: slug, Channel: configValues["CHANNEL"], DisplayName: configValues["DISPLAY_NAME"], DeploymentID: configValues["BACKEND_DEPLOYMENT_ID"], BackendURL: targetURL, BackendToken: configValues["BACKEND_TOKEN"], BotToken: configValues["TELEGRAM_BOT_TOKEN"], AdminID: parseID(configValues["ADMIN_TELEGRAM_ID"])}
	if cfg.DeploymentID != report.Deployment || !validBackendURL(cfg.BackendURL) || !validTelegramToken(cfg.BotToken) || cfg.BackendToken == "" || cfg.AdminID <= 0 {
		return errors.New("archived runtime identity failed validation")
	}
	if localExists {
		existing, _ := Read(slug)
		if existing.BackendToken != cfg.BackendToken || existing.BotToken != cfg.BotToken || existing.AdminID != cfg.AdminID || existing.Channel != cfg.Channel {
			return errors.New("existing local instance credentials do not match the restore archive")
		}
	}
	if unitData, readErr := os.ReadFile(unitPath(slug)); readErr == nil && string(unitData) != unitDefinition(cfg) {
		return errors.New("existing target service unit differs from the expected instance unit")
	} else if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
		return readErr
	}
	if _, err = backup.RestoreInstance(ctx, pool, archive, slug, values["BACKEND_PANEL_SECRETS_KEY"], false); err != nil {
		return fmt.Errorf("instance import or inactive-restore check failed: %w", err)
	}
	if err = save(cfg); err != nil {
		return fmt.Errorf("database imported but frozen; local instance config could not be installed: %w", err)
	}
	if err = installUnit(cfg); err != nil {
		return fmt.Errorf("database imported but frozen; service unit could not be installed: %w", err)
	}
	if err = systemctl("daemon-reload"); err != nil {
		return err
	}
	backendStore := &store.Store{DB: pool}
	if err = activateRestoredInstance(ctx, restoredActivationInput{
		DeploymentID: cfg.DeploymentID,
		Fingerprint:  manifest.DataSHA256,
		Slug:         slug,
		BackendToken: cfg.BackendToken,
		AdminID:      cfg.AdminID,
		BackendURL:   targetURL,
	}, restoredActivationHooks{
		AcquireTransferLease: func(leaseCtx context.Context, deployment string) (restoredActivationLease, error) {
			return backendStore.AcquireDeploymentTransferLease(leaseCtx, deployment)
		},
		Refreeze: func(freezeCtx context.Context, deployment, fingerprint string) error {
			return backendStore.RefreezeRestoredDeployment(freezeCtx, deployment, fingerprint)
		},
		Unfreeze: func(unfreezeCtx context.Context, deployment, fingerprint string) (restoredUnfreezeState, error) {
			return unfreezeRestoredDeployment(unfreezeCtx, pool, deployment, fingerprint)
		},
		ReleaseSafe: func(releaseCtx context.Context, deployment string) error {
			return releaseSafeDeployment(releaseCtx, pool, deployment)
		},
		ValidateHealth: checkBackendHealth,
		StartBot: func(instanceSlug string) error {
			return systemctl("enable", "--now", unitName(instanceSlug))
		},
		StopBot: func(instanceSlug string) {
			_ = systemctl("stop", unitName(instanceSlug))
			_ = systemctl("disable", unitName(instanceSlug))
		},
		BotActive: serviceIsActive,
		ReportWarning: func(message string) {
			fmt.Fprintf(os.Stderr, "Warning: %s\n", message)
		},
	}); err != nil {
		return err
	}
	fmt.Printf("Instance %s restored and active against %s. Uncertain work items and notifications remain quarantined for review.\n", slug, targetURL)
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

func validBackendTransport(raw string) bool {
	if !validBackendURL(raw) {
		return false
	}
	u, err := url.ParseRequestURI(raw)
	return err == nil && (u.Scheme == "https" || isLocalBackend(raw))
}

func checkBackendHealth(parent context.Context, base string) error {
	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
	defer cancel()
	client := &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+"/healthz", nil)
	if err != nil {
		return errors.New("backend health check could not be constructed")
	}
	resp, err := client.Do(req)
	if err != nil {
		return errors.New("backend URL did not respond to health check")
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != http.StatusOK {
		return errors.New("backend URL health check returned a non-success status")
	}
	return nil
}

func checkBackendIdentity(parent context.Context, cfg Config) error {
	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
	defer cancel()
	client := &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(cfg.BackendURL, "/")+"/v1/admin/config", nil)
	if err != nil {
		return errors.New("backend identity check could not be constructed")
	}
	req.Header.Set("Authorization", "Bearer "+cfg.BackendToken)
	req.Header.Set("X-Actor-Telegram-ID", strconv.FormatInt(cfg.AdminID, 10))
	resp, err := client.Do(req)
	if err != nil {
		return errors.New("registered backend credential did not authenticate")
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != http.StatusOK {
		return errors.New("registered backend credential or administrator identity was rejected")
	}
	return nil
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
	content := unitDefinition(cfg)
	path := unitPath(cfg.Slug)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		return err
	}
	return nil
}

func unitDefinition(cfg Config) string {
	return fmt.Sprintf("[Unit]\nDescription=xui-backend bot instance %s\nAfter=network-online.target xui-backend.service\nWants=network-online.target\n\n[Service]\nType=simple\nExecStart=/usr/local/bin/xui-backend run-instance %s\nRestart=on-failure\nRestartSec=5\nUser=xui-backend\nGroup=xui-backend\nUMask=0077\nNoNewPrivileges=true\nPrivateTmp=true\nProtectSystem=strict\nProtectHome=true\n\n[Install]\nWantedBy=multi-user.target\n", cfg.Slug, cfg.Slug)
}

func systemctl(args ...string) error {
	cmd := exec.Command("systemctl", args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemctl %s failed: %s", strings.Join(args, " "), strings.TrimSpace(string(output)))
	}
	return nil
}

func serviceIsActive(slug string) bool {
	return serviceStatus(slug) == "active"
}

func serviceStatus(slug string) string {
	cmd := exec.Command("systemctl", "is-active", unitName(slug))
	output, _ := cmd.CombinedOutput()
	status := strings.TrimSpace(string(output))
	if status == "" {
		return "unknown"
	}
	return status
}

func serviceIsEnabled(slug string) (bool, bool) {
	cmd := exec.Command("systemctl", "is-enabled", unitName(slug))
	output, _ := cmd.CombinedOutput()
	switch strings.TrimSpace(string(output)) {
	case "enabled":
		return true, true
	case "disabled":
		return false, true
	default:
		return false, false
	}
}

func transferStatePath(slug string) string {
	return filepath.Join(Root(), slug, "transfer-state.json")
}

func readTransferState(slug string) (transferState, error) {
	var state transferState
	data, err := os.ReadFile(transferStatePath(slug))
	if err != nil {
		return state, err
	}
	if err = json.Unmarshal(data, &state); err != nil || state.DeploymentID == "" {
		return transferState{}, errors.New("invalid source transfer state")
	}
	return state, nil
}
