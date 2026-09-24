package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"example.com/xui-commerce/backend/internal/panelurl"
	"example.com/xui-commerce/backend/internal/secrets"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type RegisterInstanceInput struct {
	DeploymentID    string
	Channel         string
	DisplayName     string
	PanelID         string
	PanelURL        string
	PanelToken      string
	TelegramToken   string
	AdminTelegramID int64
}

type RegisterInstanceResult struct {
	DeploymentID string
	BackendToken string
}

var instanceIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{1,62}$`)

// RegisterInstance creates a scoped deployment and its bootstrap administrator.
// The returned bearer token is shown once; only its digest is stored in PostgreSQL.
func (s *Store) RegisterInstance(ctx context.Context, in RegisterInstanceInput, secretKey []byte) (RegisterInstanceResult, error) {
	in.DeploymentID = strings.TrimSpace(in.DeploymentID)
	in.Channel = strings.TrimSpace(in.Channel)
	in.DisplayName = strings.TrimSpace(in.DisplayName)
	in.PanelID = strings.TrimSpace(in.PanelID)
	in.PanelURL = strings.TrimSpace(in.PanelURL)
	in.PanelToken = strings.TrimSpace(in.PanelToken)
	in.TelegramToken = strings.TrimSpace(in.TelegramToken)
	if !instanceIDPattern.MatchString(in.DeploymentID) || (in.Channel != "retail" && in.Channel != "reseller") {
		return RegisterInstanceResult{}, fmt.Errorf("deployment ID or channel is invalid")
	}
	if in.DisplayName == "" || len(in.DisplayName) > 120 || in.AdminTelegramID <= 0 {
		return RegisterInstanceResult{}, fmt.Errorf("display name and positive admin Telegram ID are required")
	}
	if in.PanelID == "" {
		in.PanelID = "panel-" + in.DeploymentID
	}
	if !instanceIDPattern.MatchString(in.PanelID) || in.PanelToken == "" || len(in.PanelToken) > 4096 || in.TelegramToken == "" || len(in.TelegramToken) > 512 {
		return RegisterInstanceResult{}, fmt.Errorf("valid panel ID, panel API token, and Telegram token are required")
	}
	if len(secretKey) != 32 {
		return RegisterInstanceResult{}, fmt.Errorf("backend secret encryption key must be 32 bytes")
	}
	if err := panelurl.Validate(in.PanelURL); err != nil {
		return RegisterInstanceResult{}, fmt.Errorf("invalid panel URL: %w", err)
	}
	panelSecret, err := secrets.Seal(secretKey, []byte(in.PanelToken))
	if err != nil {
		return RegisterInstanceResult{}, fmt.Errorf("encrypt panel token: %w", err)
	}
	telegramSecret, err := secrets.Seal(secretKey, []byte(in.TelegramToken))
	if err != nil {
		return RegisterInstanceResult{}, fmt.Errorf("encrypt Telegram token: %w", err)
	}
	secret := make([]byte, 32)
	if _, err = rand.Read(secret); err != nil {
		return RegisterInstanceResult{}, fmt.Errorf("generate backend credential: %w", err)
	}
	bearer := base64.RawURLEncoding.EncodeToString(secret)
	digest := sha256.Sum256([]byte(bearer))
	serviceID := in.Channel + "-bot"

	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return RegisterInstanceResult{}, err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `INSERT INTO client_services(id,display_name) VALUES($1,$2) ON CONFLICT(id) DO NOTHING`, serviceID, in.DisplayName); err != nil {
		return RegisterInstanceResult{}, err
	}
	var oldChannel, oldPanel string
	var deploymentActive, botActive bool
	existingErr := tx.QueryRow(ctx, `SELECT channel,COALESCE(default_panel_id,''),enabled,bot_instance_active FROM deployments WHERE id=$1 FOR UPDATE`, in.DeploymentID).Scan(&oldChannel, &oldPanel, &deploymentActive, &botActive)
	switch {
	case existingErr == nil:
		if botActive || !deploymentActive || oldChannel != in.Channel || oldPanel != in.PanelID {
			return RegisterInstanceResult{}, fmt.Errorf("deployment ID is already in use or cannot be safely reactivated: %w", ErrConflict)
		}
		tag, updateErr := tx.Exec(ctx, `UPDATE panels SET base_url=$2,encrypted_api_token=$3,enabled=true WHERE id=$1`, in.PanelID, strings.TrimRight(in.PanelURL, "/"), panelSecret)
		if updateErr != nil {
			return RegisterInstanceResult{}, updateErr
		}
		if tag.RowsAffected() == 0 {
			return RegisterInstanceResult{}, fmt.Errorf("retained panel is missing: %w", ErrConflict)
		}
		if _, err = tx.Exec(ctx, `UPDATE deployments SET encrypted_telegram_token=$2,admin_telegram_id=$3,bot_instance_active=true,telegram_notifications_enabled=true WHERE id=$1`, in.DeploymentID, telegramSecret, in.AdminTelegramID); err != nil {
			return RegisterInstanceResult{}, err
		}
		if _, err = tx.Exec(ctx, `UPDATE actors SET role='customer',updated_at=now() WHERE deployment_id=$1 AND role='admin' AND telegram_id<>$2`, in.DeploymentID, in.AdminTelegramID); err != nil {
			return RegisterInstanceResult{}, err
		}
	case errors.Is(existingErr, pgx.ErrNoRows):
		if _, err = tx.Exec(ctx, `INSERT INTO panels(id,base_url,encrypted_api_token) VALUES($1,$2,$3)`, in.PanelID, strings.TrimRight(in.PanelURL, "/"), panelSecret); err != nil {
			if isUniqueViolation(err) {
				return RegisterInstanceResult{}, fmt.Errorf("panel ID already exists: %w", ErrConflict)
			}
			return RegisterInstanceResult{}, err
		}
		if _, err = tx.Exec(ctx, `INSERT INTO deployments(id,client_service_id,channel,legacy_source_instance,default_panel_id,encrypted_telegram_token,admin_telegram_id) VALUES($1,$2,$3,$1,$4,$5,$6)`, in.DeploymentID, serviceID, in.Channel, in.PanelID, telegramSecret, in.AdminTelegramID); err != nil {
			if isUniqueViolation(err) {
				return RegisterInstanceResult{}, fmt.Errorf("deployment ID already exists: %w", ErrConflict)
			}
			return RegisterInstanceResult{}, err
		}
	default:
		return RegisterInstanceResult{}, existingErr
	}
	accountKind := "retail_customer"
	if in.Channel == "reseller" {
		accountKind = "reseller"
	}
	accountKey := "telegram-admin:" + fmt.Sprint(in.AdminTelegramID) + ":" + in.DeploymentID
	var accountID int64
	err = tx.QueryRow(ctx, `INSERT INTO commercial_accounts(home_deployment_id,kind,status,system_key) VALUES($1,$2,'active',$3) ON CONFLICT(system_key) WHERE system_key IS NOT NULL DO UPDATE SET status='active',updated_at=now() RETURNING id`, in.DeploymentID, accountKind, accountKey).Scan(&accountID)
	if err != nil {
		return RegisterInstanceResult{}, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO actors(deployment_id,account_id,telegram_id,identity_provider,external_subject,role,approval_status,enabled) VALUES($1,$2,$3,'telegram',$4,'admin','approved',true) ON CONFLICT(deployment_id,telegram_id) DO UPDATE SET account_id=EXCLUDED.account_id,identity_provider='telegram',external_subject=EXCLUDED.external_subject,role='admin',approval_status='approved',enabled=true,updated_at=now()`, in.DeploymentID, accountID, in.AdminTelegramID, fmt.Sprint(in.AdminTelegramID)); err != nil {
		return RegisterInstanceResult{}, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO backend_client_credentials(token_hash,deployment_id) VALUES($1,$2)`, digest[:], in.DeploymentID); err != nil {
		return RegisterInstanceResult{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return RegisterInstanceResult{}, err
	}
	return RegisterInstanceResult{DeploymentID: in.DeploymentID, BackendToken: bearer}, nil
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

func (s *Store) AuthenticateClient(ctx context.Context, token string) (string, error) {
	digest := sha256.Sum256([]byte(strings.TrimSpace(token)))
	var deployment string
	err := s.DB.QueryRow(ctx, `SELECT c.deployment_id FROM backend_client_credentials c JOIN deployments d ON d.id=c.deployment_id WHERE c.token_hash=$1 AND c.enabled AND d.enabled AND d.bot_instance_active`, digest[:]).Scan(&deployment)
	return deployment, err
}

func (s *Store) IsDeploymentEnabled(ctx context.Context, deployment string) (bool, error) {
	var enabled bool
	err := s.DB.QueryRow(ctx, `SELECT enabled AND bot_instance_active FROM deployments WHERE id=$1`, deployment).Scan(&enabled)
	return enabled, err
}

func (s *Store) IsDesignatedAdmin(ctx context.Context, deployment string, telegramID int64) (bool, error) {
	var authorized bool
	err := s.DB.QueryRow(ctx, `SELECT COALESCE(admin_telegram_id=$2,false) FROM deployments WHERE id=$1`, deployment, telegramID).Scan(&authorized)
	return authorized, err
}

// DeactivateInstance revokes bot API credentials but leaves the deployment and
// actors active so durable purchases, provisioning, notifications, and manual
// reconciliation continue to work after the Telegram process is removed.
func (s *Store) DeactivateInstance(ctx context.Context, deployment string) error {
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var exists bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM deployments WHERE id=$1)`, deployment).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return ErrNotFound
	}
	if _, err = tx.Exec(ctx, `UPDATE backend_client_credentials SET enabled=false WHERE deployment_id=$1`, deployment); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE deployments SET bot_instance_active=false,telegram_notifications_enabled=false,encrypted_telegram_token=NULL WHERE id=$1`, deployment); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) EncryptedTelegramToken(ctx context.Context, deployment string) ([]byte, error) {
	var token []byte
	err := s.DB.QueryRow(ctx, `SELECT encrypted_telegram_token FROM deployments WHERE id=$1 AND enabled`, deployment).Scan(&token)
	return token, err
}
