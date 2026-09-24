package store

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

type AdminPlan struct {
	ID                      int64  `json:"id"`
	Name                    string `json:"name"`
	Kind                    string `json:"kind"`
	Enabled                 bool   `json:"enabled"`
	IsLimited               bool   `json:"is_limited"`
	Description             string `json:"description"`
	BasePriceToman          int64  `json:"base_price_toman"`
	PricePerExtraIPToman    int64  `json:"price_per_extra_ip_toman"`
	PricePerGBToman         int64  `json:"price_per_gb_toman"`
	PricePerExtraMonthToman int64  `json:"price_per_extra_month_toman"`
	BaseIPLimit             int    `json:"base_ip_limit"`
	MaxIPLimit              int    `json:"max_ip_limit"`
	MinDataGB               int    `json:"min_data_gb"`
	MaxDataBytes            int64  `json:"max_data_bytes"`
	ExpireSeconds           int64  `json:"expire_seconds"`
	TestIPLimit             int    `json:"test_ip_limit"`
	MaxPerDay               int    `json:"max_per_day"`
	Flow                    string `json:"flow"`
	InboundIDs              []int  `json:"inbound_ids"`
	UsageDescription        string `json:"usage_description"`
}

type AdminConfiguration struct {
	DeploymentID        string            `json:"deployment_id"`
	Channel             string            `json:"channel"`
	Plans               []AdminPlan       `json:"plans"`
	PaymentInstructions map[string]string `json:"payment_instructions"`
	Settings            map[string]any    `json:"settings"`
	Panel               map[string]any    `json:"panel"`
}

type ResellerReview struct {
	TelegramID     int64     `json:"telegram_id"`
	ActorID        int64     `json:"actor_id,omitempty"`
	ApprovalStatus string    `json:"approval_status"`
	CreatedAt      time.Time `json:"created_at"`
}

func (s *Store) AdminConfiguration(ctx context.Context, deployment string) (*AdminConfiguration, error) {
	c := &AdminConfiguration{Plans: []AdminPlan{}}
	var configText string
	var card, owner, instructions, panelID, baseURL string
	var tokenConfigured bool
	err := s.DB.QueryRow(ctx, `SELECT d.id,d.channel,d.payment_card_number,d.payment_card_owner,d.payment_instructions,
		COALESCE(d.configuration,'{}'::jsonb)::text,COALESCE(p.id,''),COALESCE(p.base_url,''),p.encrypted_api_token IS NOT NULL
		FROM deployments d LEFT JOIN panels p ON p.id=d.default_panel_id WHERE d.id=$1`, deployment).
		Scan(&c.DeploymentID, &c.Channel, &card, &owner, &instructions, &configText, &panelID, &baseURL, &tokenConfigured)
	if err != nil {
		return nil, err
	}
	if err = json.Unmarshal([]byte(configText), &c.Settings); err != nil {
		return nil, err
	}
	if c.Settings == nil {
		c.Settings = map[string]any{}
	}
	if _, ok := c.Settings["features"]; !ok {
		c.Settings["features"] = map[string]bool{}
	}
	if _, ok := c.Settings["text"]; !ok {
		c.Settings["text"] = map[string]string{}
	}
	var reset, unapproved int
	var approvedRequired bool
	if err = s.DB.QueryRow(ctx, `SELECT retail_trial_reset_days,unapproved_trial_daily_limit,COALESCE((configuration->>'reseller_approved_required')::boolean,false) FROM deployments WHERE id=$1`, deployment).Scan(&reset, &unapproved, &approvedRequired); err != nil {
		return nil, err
	}
	c.Settings["retail_trial_reset_days"] = reset
	c.Settings["unapproved_trial_daily_limit"] = unapproved
	c.Settings["reseller_approved_required"] = approvedRequired
	c.PaymentInstructions = map[string]string{"card_number": card, "card_owner": owner, "instructions": instructions}
	c.Panel = map[string]any{"id": panelID, "base_url": baseURL, "token_configured": tokenConfigured}
	rows, err := s.DB.Query(ctx, `SELECT id,name,kind,enabled,is_limited,description,base_price_toman,price_per_extra_ip_toman,
		price_per_gb_toman,price_per_extra_month_toman,base_ip_limit,max_ip_limit,min_data_gb,max_data_bytes,expire_seconds,
		test_ip_limit,max_per_day,flow,COALESCE(array_to_json(inbound_ids)::text,'[]'),usage_description
		FROM plans WHERE deployment_id=$1 ORDER BY id`, deployment)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var p AdminPlan
		var ids string
		if err = rows.Scan(&p.ID, &p.Name, &p.Kind, &p.Enabled, &p.IsLimited, &p.Description, &p.BasePriceToman, &p.PricePerExtraIPToman,
			&p.PricePerGBToman, &p.PricePerExtraMonthToman, &p.BaseIPLimit, &p.MaxIPLimit, &p.MinDataGB, &p.MaxDataBytes, &p.ExpireSeconds,
			&p.TestIPLimit, &p.MaxPerDay, &p.Flow, &ids, &p.UsageDescription); err != nil {
			return nil, err
		}
		if err = json.Unmarshal([]byte(ids), &p.InboundIDs); err != nil {
			return nil, err
		}
		if p.InboundIDs == nil {
			p.InboundIDs = []int{}
		}
		c.Plans = append(c.Plans, p)
	}
	return c, rows.Err()
}

func validateAdminPlan(p AdminPlan) error {
	if strings.TrimSpace(p.Name) == "" || len(p.Name) > 120 || (p.Kind != "paid" && p.Kind != "test") {
		return fmt.Errorf("%w: plan name and kind are invalid", ErrInvalidAdminConfig)
	}
	if p.BasePriceToman < 0 || p.PricePerExtraIPToman < 0 || p.PricePerGBToman < 0 || p.PricePerExtraMonthToman < 0 || p.BaseIPLimit < 0 || p.MaxIPLimit < p.BaseIPLimit || p.MinDataGB < 0 || p.MaxDataBytes < 0 || p.ExpireSeconds < 0 || p.TestIPLimit < 0 || p.MaxPerDay < 0 || len(p.InboundIDs) > 64 {
		return fmt.Errorf("%w: plan values are outside allowed bounds", ErrInvalidAdminConfig)
	}
	for _, id := range p.InboundIDs {
		if id <= 0 {
			return fmt.Errorf("%w: inbound_ids must contain positive IDs", ErrInvalidAdminConfig)
		}
	}
	return nil
}

func (s *Store) SaveAdminPlan(ctx context.Context, deployment string, actorID int64, p AdminPlan) (int64, error) {
	if err := validateAdminPlan(p); err != nil {
		return 0, err
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	var panelID string
	if err := tx.QueryRow(ctx, `SELECT default_panel_id FROM deployments WHERE id=$1 AND enabled`, deployment).Scan(&panelID); err != nil {
		return 0, err
	}
	if p.ID == 0 {
		err := tx.QueryRow(ctx, `INSERT INTO plans(deployment_id,panel_id,kind,name,enabled,is_limited,description,base_price_toman,price_per_extra_ip_toman,
		price_per_gb_toman,price_per_extra_month_toman,base_ip_limit,max_ip_limit,min_data_gb,max_data_bytes,expire_seconds,test_ip_limit,max_per_day,flow,inbound_ids,usage_description)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21) RETURNING id`, deployment, panelID, p.Kind, strings.TrimSpace(p.Name), p.Enabled, p.IsLimited, p.Description, p.BasePriceToman, p.PricePerExtraIPToman, p.PricePerGBToman, p.PricePerExtraMonthToman, p.BaseIPLimit, p.MaxIPLimit, p.MinDataGB, p.MaxDataBytes, p.ExpireSeconds, p.TestIPLimit, p.MaxPerDay, p.Flow, p.InboundIDs, p.UsageDescription).Scan(&p.ID)
		if err != nil {
			return 0, err
		}
		if err = recordAdminAudit(ctx, tx, deployment, actorID, "plan_created"); err != nil {
			return 0, err
		}
		if err = tx.Commit(ctx); err != nil {
			return 0, err
		}
		return p.ID, nil
	}
	tag, err := tx.Exec(ctx, `UPDATE plans SET name=$3,kind=$4,enabled=$5,is_limited=$6,description=$7,base_price_toman=$8,price_per_extra_ip_toman=$9,
		price_per_gb_toman=$10,price_per_extra_month_toman=$11,base_ip_limit=$12,max_ip_limit=$13,min_data_gb=$14,max_data_bytes=$15,expire_seconds=$16,
		test_ip_limit=$17,max_per_day=$18,flow=$19,inbound_ids=$20,usage_description=$21,updated_at=now() WHERE deployment_id=$1 AND id=$2`, deployment, p.ID, strings.TrimSpace(p.Name), p.Kind, p.Enabled, p.IsLimited, p.Description, p.BasePriceToman, p.PricePerExtraIPToman, p.PricePerGBToman, p.PricePerExtraMonthToman, p.BaseIPLimit, p.MaxIPLimit, p.MinDataGB, p.MaxDataBytes, p.ExpireSeconds, p.TestIPLimit, p.MaxPerDay, p.Flow, p.InboundIDs, p.UsageDescription)
	if err != nil {
		return 0, err
	}
	if tag.RowsAffected() == 0 {
		return 0, pgx.ErrNoRows
	}
	if err = recordAdminAudit(ctx, tx, deployment, actorID, "plan_updated"); err != nil {
		return 0, err
	}
	if err = tx.Commit(ctx); err != nil {
		return 0, err
	}
	return p.ID, nil
}

func (s *Store) SavePaymentInstructions(ctx context.Context, deployment string, actorID int64, card, owner, instructions string) error {
	if len(card) > 80 || len(owner) > 160 || len(instructions) > 4000 {
		return fmt.Errorf("%w: payment instructions exceed allowed length", ErrInvalidAdminConfig)
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	tag, err := tx.Exec(ctx, `UPDATE deployments SET payment_card_number=$2,payment_card_owner=$3,payment_instructions=$4 WHERE id=$1`, deployment, strings.TrimSpace(card), strings.TrimSpace(owner), strings.TrimSpace(instructions))
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	if err = recordAdminAudit(ctx, tx, deployment, actorID, "payment_instructions_updated"); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) PatchAdminSettings(ctx context.Context, deployment string, actorID int64, resetDays, unapprovedLimit *int, approvedRequired *bool, features, text map[string]any) error {
	if resetDays != nil && (*resetDays < -3650 || *resetDays > 36500) {
		return fmt.Errorf("%w: retail_trial_reset_days outside allowed bounds", ErrInvalidAdminConfig)
	}
	if unapprovedLimit != nil && (*unapprovedLimit < 1 || *unapprovedLimit > 10000) {
		return fmt.Errorf("%w: unapproved_trial_daily_limit outside allowed bounds", ErrInvalidAdminConfig)
	}
	if err := validateConfigMap(features, true); err != nil {
		return err
	}
	if err := validateConfigMap(text, false); err != nil {
		return err
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var settings []byte
	if err = tx.QueryRow(ctx, `SELECT configuration FROM deployments WHERE id=$1 FOR UPDATE`, deployment).Scan(&settings); err != nil {
		return err
	}
	var current map[string]any
	if err := json.Unmarshal(settings, &current); err != nil {
		return err
	}
	if current == nil {
		current = map[string]any{}
	}
	if features != nil {
		current["features"] = mergeConfigMap(asMap(current["features"]), features)
	}
	if text != nil {
		current["text"] = mergeConfigMap(asMap(current["text"]), text)
	}
	if approvedRequired != nil {
		current["reseller_approved_required"] = *approvedRequired
	}
	encoded, err := json.Marshal(current)
	if err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE deployments SET retail_trial_reset_days=COALESCE($2,retail_trial_reset_days),unapproved_trial_daily_limit=COALESCE($3,unapproved_trial_daily_limit),configuration=$4::jsonb WHERE id=$1`, deployment, resetDays, unapprovedLimit, encoded); err != nil {
		return err
	}
	if err = recordAdminAudit(ctx, tx, deployment, actorID, "settings_updated"); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func validateConfigMap(values map[string]any, wantBool bool) error {
	if len(values) > 64 {
		return fmt.Errorf("%w: configuration map has too many entries", ErrInvalidAdminConfig)
	}
	for key, value := range values {
		if strings.TrimSpace(key) == "" || len(key) > 80 {
			return fmt.Errorf("%w: configuration key is invalid", ErrInvalidAdminConfig)
		}
		if wantBool {
			if _, ok := value.(bool); !ok {
				return fmt.Errorf("%w: feature %q must be boolean", ErrInvalidAdminConfig, key)
			}
		} else if v, ok := value.(string); !ok || len(v) > 4000 {
			return fmt.Errorf("%w: text %q must be a string no longer than 4000 characters", ErrInvalidAdminConfig, key)
		}
	}
	return nil
}
func asMap(v any) map[string]any {
	m, _ := v.(map[string]any)
	if m == nil {
		return map[string]any{}
	}
	return m
}
func mergeConfigMap(dst, src map[string]any) map[string]any {
	for k, v := range src {
		dst[k] = v
	}
	return dst
}

func (s *Store) SavePanelConfig(ctx context.Context, deployment string, actorID int64, baseURL string, ciphertext []byte) error {
	u, err := url.ParseRequestURI(strings.TrimSpace(baseURL))
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || len(baseURL) > 500 {
		return fmt.Errorf("%w: panel base_url must be an HTTP(S) URL", ErrInvalidAdminConfig)
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	tag, err := tx.Exec(ctx, `UPDATE panels p SET base_url=$2,encrypted_api_token=COALESCE($3,p.encrypted_api_token) FROM deployments d WHERE d.id=$1 AND p.id=d.default_panel_id`, deployment, strings.TrimRight(baseURL, "/"), nullBytes(ciphertext))
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	if err = recordAdminAudit(ctx, tx, deployment, actorID, "panel_config_updated"); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func nullBytes(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return b
}

func (s *Store) EncryptedPanelToken(ctx context.Context, panelID string) ([]byte, error) {
	var token []byte
	err := s.DB.QueryRow(ctx, `SELECT encrypted_api_token FROM panels WHERE id=$1 AND enabled`, panelID).Scan(&token)
	return token, err
}

func (s *Store) FeatureEnabled(ctx context.Context, deployment, key string) (bool, error) {
	var enabled bool
	err := s.DB.QueryRow(ctx, `SELECT COALESCE((configuration->'features'->>$2)::boolean,true) FROM deployments WHERE id=$1`, deployment, key).Scan(&enabled)
	return enabled, err
}

func (s *Store) ResellerApprovedRequired(ctx context.Context, deployment string) (bool, error) {
	var required bool
	err := s.DB.QueryRow(ctx, `SELECT COALESCE((configuration->>'reseller_approved_required')::boolean,false) FROM deployments WHERE id=$1`, deployment).Scan(&required)
	return required, err
}

func (s *Store) PublicFeatures(ctx context.Context, deployment string) (map[string]any, error) {
	var raw []byte
	if err := s.DB.QueryRow(ctx, `SELECT configuration FROM deployments WHERE id=$1`, deployment).Scan(&raw); err != nil {
		return nil, err
	}
	var c map[string]any
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, err
	}
	return map[string]any{"features": asMap(c["features"]), "text": asMap(c["text"])}, nil
}

func (s *Store) PendingResellers(ctx context.Context, deployment string) ([]ResellerReview, error) {
	rows, err := s.DB.Query(ctx, `SELECT a.telegram_id,a.id,a.approval_status,a.created_at FROM actors a JOIN deployments d ON d.id=a.deployment_id
		WHERE a.deployment_id=$1 AND d.channel='reseller' AND a.role='reseller' AND a.approval_status='pending' AND a.enabled
		ORDER BY a.created_at,a.id`, deployment)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]ResellerReview, 0)
	for rows.Next() {
		var v ResellerReview
		if err = rows.Scan(&v.TelegramID, &v.ActorID, &v.ApprovalStatus, &v.CreatedAt); err != nil {
			return nil, err
		}
		result = append(result, v)
	}
	return result, rows.Err()
}

func (s *Store) SetResellerApproval(ctx context.Context, deployment string, adminID, telegramID int64, status string) (*ResellerReview, error) {
	if err := validateResellerDecision("pending", status); err != nil {
		return nil, err
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	v := &ResellerReview{TelegramID: telegramID, ApprovalStatus: status}
	var currentStatus string
	err = tx.QueryRow(ctx, `SELECT a.id,a.approval_status FROM actors a JOIN deployments d ON d.id=a.deployment_id
		WHERE a.deployment_id=$1 AND d.channel='reseller' AND a.telegram_id=$2 AND a.role='reseller' AND a.enabled FOR UPDATE OF a`, deployment, telegramID).Scan(&v.ActorID, &currentStatus)
	if err != nil {
		return nil, err
	}
	if err = validateResellerDecision(currentStatus, status); err != nil {
		return nil, err
	}
	tag, err := tx.Exec(ctx, `UPDATE actors SET approval_status=$3,updated_at=now() WHERE id=$1 AND deployment_id=$2 AND approval_status='pending'`, v.ActorID, deployment, status)
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() != 1 {
		return nil, ErrConflict
	}
	if err = recordAdminAudit(ctx, tx, deployment, adminID, "reseller_"+status); err != nil {
		return nil, err
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, err
	}
	return v, nil
}

func validateResellerDecision(current, target string) error {
	if target != "approved" && target != "rejected" {
		return fmt.Errorf("%w: reseller approval status must be approved or rejected", ErrInvalidAdminConfig)
	}
	if current != "pending" {
		return ErrConflict
	}
	return nil
}

func recordAdminAudit(ctx context.Context, tx pgx.Tx, deployment string, actorID int64, action string) error {
	tag, err := tx.Exec(ctx, `INSERT INTO admin_configuration_audit(deployment_id,actor_id,action)
		SELECT $1,id,$3 FROM actors WHERE id=$2 AND deployment_id=$1 AND deployment_id IN ('retail-finland','reseller-turk1') AND telegram_id=96937669 AND role='admin' AND enabled AND approval_status='approved'`, deployment, actorID, action)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrForbidden
	}
	return nil
}
