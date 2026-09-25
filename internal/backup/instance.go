package backup

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"example.com/xui-commerce/backend/internal/secrets"
	"example.com/xui-commerce/backend/internal/xui"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// InstanceReport is safe to display: it contains identifiers and row counts,
// never configuration values or encrypted credential bytes.
type InstanceReport struct {
	Deployment         string         `json:"deployment"`
	Counts             map[string]int `json:"row_counts"`
	UncertainWorkItems int            `json:"uncertain_work_items"`
	UncertainOutbox    int            `json:"uncertain_outbox"`
	Blockers           []string       `json:"blockers,omitempty"`
}

type instanceArchive struct {
	Format     int                          `json:"format"`
	Deployment string                       `json:"deployment"`
	Schema     []string                     `json:"schema_migrations"`
	Tables     map[string][]json.RawMessage `json:"tables"`
}

// Table order is parent-first. IDs and operation keys are retained verbatim.
var instanceTables = []string{
	"client_services", "panels", "deployments", "commercial_accounts", "actors", "plans",
	"purchase_quotes", "wallet_ledger", "subscriptions", "payment_intents", "orders",
	"payment_settlements", "work_items", "trial_usage", "trial_claims", "outbox",
	"topup_requests", "wallet_credit_approvals", "refund_requests", "refund_approvals",
	"plan_access", "reseller_access_requests", "admin_configuration_audit",
	"backend_client_credentials", "legacy_import_batches", "legacy_id_map", "legacy_records",
	"legacy_obligations", "migration_audit", "legacy_panel_assignments",
}

// CreateInstance creates a protected logical snapshot of one deployment. The
// source service must be stopped while this runs to make the archive a cutover
// snapshot rather than a live, potentially inconsistent copy.
func CreateInstance(ctx context.Context, db *pgxpool.Pool, instanceRoot, slug, destination string) (InstanceReport, error) {
	if !validSlug(slug) {
		return InstanceReport{}, errors.New("invalid instance slug")
	}
	configs, err := readConfigDir(filepath.Join(instanceRoot, slug))
	if err != nil {
		return InstanceReport{}, err
	}
	// The slug maps to the durable deployment ID in instance.env. Parse only
	// that setting; the config is archived byte-for-byte and never logged.
	deployment := envValue(string(configs["instance.env"]), "BACKEND_DEPLOYMENT_ID")
	if deployment == "" {
		return InstanceReport{}, errors.New("instance config has no backend deployment ID")
	}
	archive := instanceArchive{Format: 2, Deployment: deployment, Tables: map[string][]json.RawMessage{}}
	report := InstanceReport{Deployment: deployment, Counts: map[string]int{}}
	tx, err := db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return report, errors.New("could not begin consistent instance snapshot")
	}
	defer tx.Rollback(ctx)
	if err = tx.QueryRow(ctx, `SELECT COALESCE(array_agg(version ORDER BY version), ARRAY[]::text[]) FROM schema_migrations`).Scan(&archive.Schema); err != nil {
		return report, errors.New("could not read schema version")
	}
	var exists bool
	var frozen bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM deployments WHERE id=$1), COALESCE((SELECT transfer_frozen FROM deployments WHERE id=$1),false)`, deployment).Scan(&exists, &frozen); err != nil || !exists {
		return report, errors.New("configured deployment does not exist")
	}
	if !frozen {
		return report, errors.New("source deployment must be transfer-frozen before snapshot")
	}
	var outside int
	if err = tx.QueryRow(ctx, `SELECT count(*) FROM subscriptions s JOIN commercial_accounts a ON a.id=s.subscriber_account_id WHERE s.deployment_id=$1 AND a.home_deployment_id<>$1`, deployment).Scan(&outside); err != nil {
		return report, errors.New("could not inspect cross-deployment account references")
	}
	if outside != 0 {
		report.Blockers = append(report.Blockers, "subscriptions reference accounts owned by another deployment")
		return report, errors.New("instance has cross-deployment account references")
	}
	for _, table := range instanceTables {
		query, args, e := instanceSelect(table, deployment)
		if e != nil {
			return report, e
		}
		rows, e := tx.Query(ctx, query, args...)
		if e != nil {
			return report, fmt.Errorf("could not snapshot %s", table)
		}
		for rows.Next() {
			var raw []byte
			if e = rows.Scan(&raw); e != nil {
				rows.Close()
				return report, fmt.Errorf("could not read %s row", table)
			}
			archive.Tables[table] = append(archive.Tables[table], append(json.RawMessage(nil), raw...))
		}
		e = rows.Err()
		rows.Close()
		if e != nil {
			return report, fmt.Errorf("could not snapshot %s", table)
		}
		report.Counts[table] = len(archive.Tables[table])
		if _, ok := archive.Tables[table]; !ok {
			archive.Tables[table] = []json.RawMessage{}
		}
	}
	for _, raw := range archive.Tables["work_items"] {
		var row struct {
			Status   string `json:"status"`
			Phase    string `json:"phase"`
			Attempts int    `json:"attempts"`
		}
		_ = json.Unmarshal(raw, &row)
		if row.Status == "running" || row.Status == "manual_review" || row.Phase == "create_attempted" || row.Phase == "manual_review" || row.Attempts > 0 {
			report.UncertainWorkItems++
		}
	}
	for _, raw := range archive.Tables["outbox"] {
		var row struct {
			Status   string `json:"status"`
			Attempts int    `json:"attempts"`
		}
		_ = json.Unmarshal(raw, &row)
		if row.Status == "sending" || (row.Attempts > 0 && row.Status != "sent") {
			report.UncertainOutbox++
		}
	}
	data, err := json.Marshal(archive)
	if err != nil {
		return report, err
	}
	files := map[string][]byte{"instance.env": configs["instance.env"]}
	if b, ok := configs["metadata.json"]; ok {
		files["metadata.json"] = b
	}
	files["database.json"] = data
	backendData, err := os.ReadFile(filepath.Join(filepath.Dir(instanceRoot), "backend.env"))
	if err != nil {
		return report, errors.New("could not read protected backend encryption key")
	}
	key := envValue(string(backendData), "BACKEND_PANEL_SECRETS_KEY")
	if key == "" {
		return report, errors.New("backend encryption key is missing")
	}
	files["recovery.key"] = []byte(key + "\n")
	sum := sha256.Sum256(data)
	m := Manifest{Format: 2, Scope: "instance", Instance: slug, CreatedAt: nowUTC(), Database: true, DataSHA256: hex.EncodeToString(sum[:]), RowCounts: report.Counts, ConfigFiles: keys(files)}
	if err = writeArchive(destination, m, nil, prefixFiles(slug, files)); err != nil {
		return report, err
	}
	return report, nil
}

// InspectInstance validates the archive and reports source row counts without
// exposing secrets. It makes no database changes.
func InspectInstance(path string) (Manifest, InstanceReport, error) {
	m, err := Validate(path)
	if err != nil {
		return m, InstanceReport{}, err
	}
	if m.Scope != "instance" {
		return m, InstanceReport{}, errors.New("archive is not an instance backup")
	}
	files, err := readArchiveFiles(path)
	if err != nil {
		return m, InstanceReport{}, err
	}
	var a instanceArchive
	if err = json.Unmarshal(files[m.Instance+"/database.json"], &a); err != nil || a.Format != 2 || a.Deployment == "" {
		return m, InstanceReport{}, errors.New("invalid instance database archive")
	}
	if envValue(string(files[m.Instance+"/instance.env"]), "BACKEND_DEPLOYMENT_ID") != a.Deployment {
		return m, InstanceReport{}, errors.New("instance configuration and database deployment IDs differ")
	}
	known := map[string]bool{}
	for _, table := range instanceTables {
		known[table] = true
		if _, ok := a.Tables[table]; !ok {
			return m, InstanceReport{}, fmt.Errorf("instance archive is missing table group %s", table)
		}
	}
	for table := range a.Tables {
		if !known[table] {
			return m, InstanceReport{}, fmt.Errorf("instance archive contains unknown table group %s", table)
		}
	}
	deploymentMatches := 0
	for _, row := range a.Tables["deployments"] {
		var record struct {
			ID string `json:"id"`
		}
		if json.Unmarshal(row, &record) == nil && record.ID == a.Deployment {
			deploymentMatches++
		}
	}
	if deploymentMatches != 1 {
		return m, InstanceReport{}, errors.New("instance archive deployment row is missing or duplicated")
	}
	r := InstanceReport{Deployment: a.Deployment, Counts: map[string]int{}}
	for t, rows := range a.Tables {
		r.Counts[t] = len(rows)
	}
	if len(r.Counts) != len(m.RowCounts) {
		return m, InstanceReport{}, errors.New("instance archive manifest row counts differ")
	}
	for table, count := range r.Counts {
		if m.RowCounts[table] != count {
			return m, InstanceReport{}, errors.New("instance archive manifest row counts differ")
		}
	}
	return m, r, nil
}

// StagePortableInstanceConfig rewrites only local runtime identity and backend
// address, retaining the encrypted archive's scoped bearer and Telegram token.
// The encryption recovery key is intentionally never copied into this directory.
func StagePortableInstanceConfig(path, stagingParent, newSlug, backendURL string, dryRun bool) (string, error) {
	m, err := Validate(path)
	if err != nil {
		return "", err
	}
	if m.Scope != "instance" {
		return "", errors.New("archive is not a complete instance backup")
	}
	if !validSlug(newSlug) {
		return "", errors.New("invalid target instance slug")
	}
	if strings.TrimSpace(backendURL) == "" {
		return "", errors.New("target backend URL is required")
	}
	if dryRun {
		return filepath.Join(stagingParent, "instance-review-<new>", newSlug), nil
	}
	files, err := readArchiveFiles(path)
	if err != nil {
		return "", err
	}
	data := files[m.Instance+"/instance.env"]
	lines := strings.Split(string(data), "\n")
	for i, line := range lines {
		key, _, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		switch key {
		case "INSTANCE_ID":
			lines[i] = key + "=" + strconv.Quote(newSlug)
		case "BACKEND_URL":
			lines[i] = key + "=" + strconv.Quote(strings.TrimRight(backendURL, "/"))
		}
	}
	if err = os.MkdirAll(stagingParent, 0700); err != nil {
		return "", err
	}
	stage, err := os.MkdirTemp(stagingParent, "instance-review-")
	if err != nil {
		return "", err
	}
	if err = os.Chmod(stage, 0700); err != nil {
		_ = os.RemoveAll(stage)
		return "", err
	}
	target := filepath.Join(stage, newSlug)
	if err = os.Mkdir(target, 0700); err != nil {
		_ = os.RemoveAll(stage)
		return "", err
	}
	if err = os.WriteFile(filepath.Join(target, "instance.env"), []byte(strings.Join(lines, "\n")), 0600); err != nil {
		_ = os.RemoveAll(stage)
		return "", err
	}
	if raw := files[m.Instance+"/metadata.json"]; len(raw) > 0 {
		var v map[string]any
		if json.Unmarshal(raw, &v) != nil {
			_ = os.RemoveAll(stage)
			return "", errors.New("invalid instance metadata")
		}
		v["slug"] = newSlug
		v["backend_url"] = strings.TrimRight(backendURL, "/")
		b, e := json.MarshalIndent(v, "", "  ")
		if e != nil {
			_ = os.RemoveAll(stage)
			return "", e
		}
		if e = os.WriteFile(filepath.Join(target, "metadata.json"), b, 0600); e != nil {
			_ = os.RemoveAll(stage)
			return "", e
		}
	}
	return target, nil
}

// RestoreInstance inserts the complete instance into an existing migrated
// backend. It preserves all source IDs and refuses any deployment or primary
// key collision. Dry-run performs all checks in a rolled-back serializable
// transaction. Workers must remain stopped until remote panel state is read
// back and queued unknown work is reconciled.
func RestoreInstance(ctx context.Context, db *pgxpool.Pool, path, newSlug, targetKey string, dryRun bool) (InstanceReport, error) {
	if !validSlug(newSlug) {
		return InstanceReport{}, errors.New("invalid target instance slug")
	}
	m, r, err := InspectInstance(path)
	if err != nil {
		return r, err
	}
	files, err := readArchiveFiles(path)
	if err != nil {
		return r, err
	}
	var a instanceArchive
	if err = json.Unmarshal(files[m.Instance+"/database.json"], &a); err != nil {
		return r, errors.New("invalid instance database archive")
	}
	sourceKey, err := secrets.ParseKey(strings.TrimSpace(string(files[m.Instance+"/recovery.key"])))
	if err != nil {
		return r, errors.New("archive encryption key is invalid")
	}
	destinationKey, err := secrets.ParseKey(strings.TrimSpace(targetKey))
	if err != nil {
		return r, errors.New("destination encryption key is invalid")
	}
	if err = rewrapEncryptedRows(&a, sourceKey, destinationKey); err != nil {
		return r, err
	}
	if err = validateRuntimeIdentity(&a, files[m.Instance+"/instance.env"], destinationKey); err != nil {
		return r, err
	}
	setDeploymentFlag(a.Tables["deployments"], "transfer_frozen", true)
	setDeploymentString(a.Tables["deployments"], "restore_fingerprint", m.DataSHA256)
	if err = validateRemoteClients(ctx, &a, destinationKey); err != nil {
		return r, err
	}
	quarantineRows(a.Tables["work_items"])
	quarantineRows(a.Tables["outbox"])
	var schema []string
	if err = db.QueryRow(ctx, `SELECT COALESCE(array_agg(version ORDER BY version), ARRAY[]::text[]) FROM schema_migrations`).Scan(&schema); err != nil {
		return r, errors.New("could not read destination schema version")
	}
	if strings.Join(schema, "\n") != strings.Join(a.Schema, "\n") {
		return r, errors.New("source and destination schema versions differ")
	}
	tx, err := db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return r, err
	}
	defer tx.Rollback(ctx)
	var existingFingerprint string
	var existingFrozen bool
	err = tx.QueryRow(ctx, `SELECT restore_fingerprint,transfer_frozen FROM deployments WHERE id=$1`, a.Deployment).Scan(&existingFingerprint, &existingFrozen)
	if err == nil {
		if existingFingerprint == m.DataSHA256 && existingFrozen {
			return r, nil
		}
		return r, errors.New("destination already contains this deployment without a matching inactive restore marker")
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return r, err
	}
	for _, row := range a.Tables["panels"] {
		var want struct {
			ID  string `json:"id"`
			URL string `json:"base_url"`
		}
		if json.Unmarshal(row, &want) != nil || want.ID == "" || want.URL == "" {
			return r, errors.New("invalid panel record in instance archive")
		}
		var otherID string
		err = tx.QueryRow(ctx, `SELECT id FROM panels WHERE base_url=$1 AND id<>$2 LIMIT 1`, want.URL, want.ID).Scan(&otherID)
		if err == nil {
			return r, errors.New("target already represents this panel endpoint under a different panel ID")
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return r, err
		}
	}
	for _, table := range instanceTables {
		for _, row := range a.Tables[table] {
			q := fmt.Sprintf(`INSERT INTO %s SELECT * FROM jsonb_populate_record(NULL::%s,$1::jsonb)`, table, table)
			if table == "client_services" {
				q = `INSERT INTO client_services SELECT * FROM jsonb_populate_record(NULL::client_services,$1::jsonb) ON CONFLICT(id) DO NOTHING`
			} else if table == "panels" {
				q = `INSERT INTO panels SELECT * FROM jsonb_populate_record(NULL::panels,$1::jsonb) ON CONFLICT(id) DO NOTHING`
			}
			if _, err = tx.Exec(ctx, q, []byte(row)); err != nil {
				return r, fmt.Errorf("destination collision or schema mismatch in %s", table)
			}
			if table == "client_services" {
				var want struct {
					ID          string `json:"id"`
					DisplayName string `json:"display_name"`
				}
				if json.Unmarshal(row, &want) != nil {
					return r, errors.New("invalid client service row")
				}
				var got string
				if err = tx.QueryRow(ctx, `SELECT display_name FROM client_services WHERE id=$1`, want.ID).Scan(&got); err != nil || got != want.DisplayName {
					return r, errors.New("shared client service identity conflicts with destination")
				}
			} else if table == "panels" {
				if err = verifySharedPanel(ctx, tx, row, destinationKey); err != nil {
					return r, err
				}
			}
		}
	}
	// Fix every owned BIGSERIAL sequence after preserving source identifiers.
	if !dryRun {
		for _, table := range []string{"commercial_accounts", "actors", "plans", "purchase_quotes", "wallet_ledger", "subscriptions", "payment_intents", "orders", "payment_settlements", "work_items", "outbox", "topup_requests", "wallet_credit_approvals", "refund_requests", "refund_approvals", "reseller_access_requests", "admin_configuration_audit", "migration_audit"} {
			q := fmt.Sprintf(`SELECT setval(pg_get_serial_sequence('%s','id'), GREATEST(COALESCE((SELECT max(id) FROM %s),1),1), true)`, table, table)
			if _, err = tx.Exec(ctx, q); err != nil {
				return r, fmt.Errorf("could not advance %s identity sequence", table)
			}
		}
	}
	if dryRun {
		return r, nil
	}
	if err = tx.Commit(ctx); err != nil {
		return r, errors.New("instance restore transaction failed")
	}
	return r, nil
}

func verifySharedPanel(ctx context.Context, tx pgx.Tx, row json.RawMessage, key []byte) error {
	var want struct {
		ID      string `json:"id"`
		URL     string `json:"base_url"`
		Enabled bool   `json:"enabled"`
		Token   string `json:"encrypted_api_token"`
	}
	if json.Unmarshal(row, &want) != nil || want.ID == "" || !strings.HasPrefix(want.Token, `\x`) {
		return errors.New("invalid shared panel identity in archive")
	}
	wantCiphertext, err := hex.DecodeString(strings.TrimPrefix(want.Token, `\x`))
	if err != nil {
		return errors.New("invalid shared panel credential in archive")
	}
	wantSecret, err := secrets.Open(key, wantCiphertext)
	if err != nil {
		return errors.New("shared panel archive credential could not be decrypted")
	}
	var url string
	var enabled bool
	var ciphertext []byte
	if err = tx.QueryRow(ctx, `SELECT base_url,enabled,encrypted_api_token FROM panels WHERE id=$1`, want.ID).Scan(&url, &enabled, &ciphertext); err != nil {
		return errors.New("shared panel row could not be verified in destination")
	}
	if url != want.URL || enabled != want.Enabled || len(ciphertext) == 0 {
		return errors.New("shared panel configuration conflicts with destination")
	}
	actualSecret, err := secrets.Open(key, ciphertext)
	if err != nil || !bytes.Equal(actualSecret, wantSecret) {
		return errors.New("shared panel credential conflicts with destination")
	}
	return nil
}

func validateRuntimeIdentity(a *instanceArchive, config []byte, key []byte) error {
	deployment := envValue(string(config), "BACKEND_DEPLOYMENT_ID")
	token := envValue(string(config), "BACKEND_TOKEN")
	adminRaw := envValue(string(config), "ADMIN_TELEGRAM_ID")
	adminID, err := strconv.ParseInt(adminRaw, 10, 64)
	if err != nil || adminID <= 0 || deployment != a.Deployment || token == "" {
		return errors.New("archived bot runtime identity is incomplete")
	}
	digest := sha256.Sum256([]byte(token))
	tokenHash := hex.EncodeToString(digest[:])
	credentialFound := false
	for _, row := range a.Tables["backend_client_credentials"] {
		var c struct {
			Hash       string `json:"token_hash"`
			Deployment string `json:"deployment_id"`
			Enabled    bool   `json:"enabled"`
		}
		if json.Unmarshal(row, &c) != nil {
			return errors.New("invalid backend credential row")
		}
		if strings.TrimPrefix(c.Hash, `\x`) == tokenHash && c.Deployment == deployment && c.Enabled {
			credentialFound = true
		}
	}
	if !credentialFound {
		return errors.New("archived scoped backend credential does not match runtime configuration")
	}
	adminFound := false
	for _, row := range a.Tables["actors"] {
		var actor struct {
			Deployment string `json:"deployment_id"`
			TelegramID int64  `json:"telegram_id"`
			Role       string `json:"role"`
			Approval   string `json:"approval_status"`
			Enabled    bool   `json:"enabled"`
		}
		if json.Unmarshal(row, &actor) == nil && actor.Deployment == deployment && actor.TelegramID == adminID && actor.Role == "admin" && actor.Approval == "approved" && actor.Enabled {
			adminFound = true
		}
	}
	if !adminFound {
		return errors.New("archived administrator identity is not enabled and approved")
	}
	telegramToken := envValue(string(config), "TELEGRAM_BOT_TOKEN")
	encryptedFound := false
	for _, row := range a.Tables["deployments"] {
		var d struct {
			ID        string `json:"id"`
			AdminID   *int64 `json:"admin_telegram_id"`
			Encrypted string `json:"encrypted_telegram_token"`
		}
		if json.Unmarshal(row, &d) != nil || d.ID != deployment {
			continue
		}
		if d.AdminID == nil || *d.AdminID != adminID || !strings.HasPrefix(d.Encrypted, `\x`) {
			return errors.New("deployment runtime identity does not match config")
		}
		ciphertext, e := hex.DecodeString(strings.TrimPrefix(d.Encrypted, `\x`))
		if e != nil {
			return errors.New("invalid encrypted Telegram credential")
		}
		plain, e := secrets.Open(key, ciphertext)
		if e != nil || string(plain) != telegramToken {
			return errors.New("encrypted Telegram credential does not match runtime config")
		}
		encryptedFound = true
	}
	if !encryptedFound {
		return errors.New("deployment Telegram credential is missing")
	}
	return nil
}

func setDeploymentFlag(rows []json.RawMessage, key string, value bool) {
	for i, row := range rows {
		var fields map[string]json.RawMessage
		if json.Unmarshal(row, &fields) != nil {
			continue
		}
		b, _ := json.Marshal(value)
		fields[key] = b
		if updated, err := json.Marshal(fields); err == nil {
			rows[i] = updated
		}
	}
}

func setDeploymentString(rows []json.RawMessage, key, value string) {
	for i, row := range rows {
		var fields map[string]json.RawMessage
		if json.Unmarshal(row, &fields) != nil {
			continue
		}
		b, _ := json.Marshal(value)
		fields[key] = b
		if updated, err := json.Marshal(fields); err == nil {
			rows[i] = updated
		}
	}
}

func quarantineRows(rows []json.RawMessage) {
	for i, row := range rows {
		var fields map[string]json.RawMessage
		if json.Unmarshal(row, &fields) != nil {
			continue
		}
		fields["restore_quarantined"] = json.RawMessage("true")
		if b, err := json.Marshal(fields); err == nil {
			rows[i] = b
		}
	}
}

func rewrapEncryptedRows(a *instanceArchive, from, to []byte) error {
	for table, column := range map[string]string{"panels": "encrypted_api_token", "deployments": "encrypted_telegram_token"} {
		for i, row := range a.Tables[table] {
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(row, &fields); err != nil {
				return errors.New("invalid encrypted credential row")
			}
			raw := fields[column]
			if len(raw) == 0 || string(raw) == "null" {
				continue
			}
			var encoded string
			if err := json.Unmarshal(raw, &encoded); err != nil || !strings.HasPrefix(encoded, `\x`) {
				return errors.New("invalid encrypted credential encoding")
			}
			ciphertext, err := hex.DecodeString(strings.TrimPrefix(encoded, `\x`))
			if err != nil {
				return errors.New("invalid encrypted credential encoding")
			}
			plain, err := secrets.Open(from, ciphertext)
			if err != nil {
				return errors.New("archive credential could not be decrypted")
			}
			resealed, err := secrets.Seal(to, plain)
			if err != nil {
				return errors.New("credential re-encryption failed")
			}
			b, _ := json.Marshal(`\x` + hex.EncodeToString(resealed))
			fields[column] = b
			updated, err := json.Marshal(fields)
			if err != nil {
				return errors.New("credential row encoding failed")
			}
			a.Tables[table][i] = updated
		}
	}
	return nil
}

func validateRemoteClients(ctx context.Context, a *instanceArchive, key []byte) error {
	type panel struct {
		ID    string `json:"id"`
		URL   string `json:"base_url"`
		Token string `json:"encrypted_api_token"`
	}
	panels := map[string]*xui.Client{}
	type panelScan struct {
		clients  []xui.RemoteClient
		inbounds []xui.InboundAttachment
		options  map[int]bool
	}
	scans := map[string]panelScan{}
	for _, raw := range a.Tables["panels"] {
		var p panel
		if json.Unmarshal(raw, &p) != nil {
			return errors.New("invalid panel record in instance archive")
		}
		if p.Token == "" || p.Token == "null" {
			return errors.New("instance panel has no encrypted API credential")
		}
		ciphertext, err := hex.DecodeString(strings.TrimPrefix(p.Token, `\x`))
		if err != nil {
			return errors.New("invalid encrypted panel credential")
		}
		plain, err := secrets.Open(key, ciphertext)
		if err != nil {
			return errors.New("destination panel credential could not be decrypted")
		}
		c, err := xui.New(p.URL, string(plain), 12*time.Second)
		if err != nil {
			return errors.New("panel configuration is invalid")
		}
		if err = c.CheckWriteReadiness(ctx); err != nil {
			return errors.New("target panel is unreachable or incompatible with the backend write contract")
		}
		listed, err := c.ListClients(ctx)
		if err != nil {
			return errors.New("full remote client list failed; collision check is incomplete")
		}
		inbounds, err := c.ListInboundAttachments(ctx)
		if err != nil {
			return errors.New("remote inbound attachment list failed; collision check is incomplete")
		}
		ids, err := c.ListInboundOptions(ctx)
		if err != nil {
			return errors.New("remote inbound options failed; collision check is incomplete")
		}
		opts := map[int]bool{}
		for _, id := range ids {
			opts[id] = true
		}
		scans[p.ID] = panelScan{clients: listed, inbounds: inbounds, options: opts}
		panels[p.ID] = c
	}
	type subscriptionIdentity struct {
		PanelID  string `json:"panel_id"`
		Email    string `json:"client_email"`
		UUID     string `json:"client_uuid"`
		SubID    string `json:"sub_id"`
		Status   string `json:"status"`
		Inbounds []int  `json:"inbound_ids"`
		ID       int64  `json:"id"`
	}
	var subscriptions []subscriptionIdentity
	type pendingAddIdentity struct {
		Email    string `json:"email"`
		UUID     string `json:"client_uuid"`
		SubID    string `json:"sub_id"`
		PanelID  string `json:"panel_id"`
		Inbounds []int  `json:"inbound_ids"`
	}
	safePendingAdd := map[int64]pendingAddIdentity{}
	for _, raw := range a.Tables["work_items"] {
		var work struct {
			SubscriptionID int64              `json:"subscription_id"`
			Kind           string             `json:"kind"`
			Status         string             `json:"status"`
			Phase          string             `json:"phase"`
			Desired        pendingAddIdentity `json:"desired_state"`
		}
		if json.Unmarshal(raw, &work) != nil {
			return errors.New("invalid work item in instance archive")
		}
		if work.SubscriptionID != 0 && work.Kind == "provision_add" && work.Status == "pending" && work.Phase == "ready" {
			safePendingAdd[work.SubscriptionID] = work.Desired
		}
	}
	for _, raw := range a.Tables["subscriptions"] {
		var sub subscriptionIdentity
		if json.Unmarshal(raw, &sub) != nil || sub.PanelID == "" || sub.Email == "" || sub.UUID == "" || sub.SubID == "" {
			return errors.New("invalid subscription identity in instance archive")
		}
		subscriptions = append(subscriptions, sub)
	}
	// Check every archived identity, including historical deleted/cancelled
	// rows, before considering whether it still needs remote readback.
	for _, sub := range subscriptions {
		client := panels[sub.PanelID]
		scan, ok := scans[sub.PanelID]
		if client == nil || !ok {
			return errors.New("subscription references a panel absent from the archived panel closure")
		}
		var listed *xui.RemoteClient
		for i := range scan.clients {
			other := &scan.clients[i]
			if remoteIdentityConflicts(other.Email, other.UUID, other.SubID, sub.Email, sub.UUID, sub.SubID) {
				return errors.New("remote panel has a global email, UUID, or subscription ID collision")
			}
			if other.Email == sub.Email && other.UUID == sub.UUID && other.SubID == sub.SubID {
				listed = other
			}
		}
		attached := false
		for _, in := range scan.inbounds {
			for i := range in.Clients {
				other := &in.Clients[i]
				if remoteIdentityConflicts(other.Email, other.UUID, other.SubID, sub.Email, sub.UUID, sub.SubID) {
					return errors.New("remote inbound list has a global email, UUID, or subscription ID collision")
				}
				if other.Email == sub.Email && other.UUID == sub.UUID && other.SubID == sub.SubID {
					attached = true
				}
			}
		}
		if attached && listed == nil {
			return errors.New("remote inbound list contains an identity missing from the full client list")
		}
		if listed == nil {
			pending, hasPending := safePendingAdd[sub.ID]
			provenNoWrite := hasPending && pending.Email == sub.Email && pending.UUID == sub.UUID && pending.SubID == sub.SubID && pending.PanelID == sub.PanelID && sameIDs(pending.Inbounds, sub.Inbounds)
			if sub.Status == "deleted" || sub.Status == "cancelled" || (sub.Status == "provisioning" && provenNoWrite) {
				continue
			}
			return errors.New("remote panel client is missing from full client list without a durable no-write proof")
		}
		remote, err := client.GetClient(ctx, sub.Email)
		if err != nil {
			return errors.New("remote panel readback failed for an instance subscription")
		}
		if remote.Email != sub.Email || xui.UUIDOf(remote) != sub.UUID || remote.SubID != sub.SubID {
			return errors.New("remote panel identity collision: email, UUID, or subscription ID differs")
		}
		if sub.Status == "deleted" || sub.Status == "cancelled" {
			continue
		}
		if !sameIDs(remote.InboundIDs, sub.Inbounds) || !sameIDs(listed.InboundIDs, sub.Inbounds) {
			return errors.New("remote panel client inbound attachment differs from archived subscription")
		}
		for _, id := range sub.Inbounds {
			if !scan.options[id] {
				return errors.New("archived subscription references an inbound missing from target panel")
			}
			found := false
			for _, in := range scan.inbounds {
				if in.ID == id {
					for _, cl := range in.Clients {
						if cl.Email == sub.Email && cl.UUID == sub.UUID && cl.SubID == sub.SubID {
							found = true
						}
					}
				}
			}
			if !found {
				return errors.New("remote inbound attachment readback did not match archived subscription")
			}
		}
	}
	return nil
}

func remoteIdentityConflicts(email, uuid, subID, wantEmail, wantUUID, wantSubID string) bool {
	return (email == wantEmail || uuid == wantUUID || subID == wantSubID) &&
		(email != wantEmail || uuid != wantUUID || subID != wantSubID)
}

func sameIDs(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	aa := append([]int(nil), a...)
	bb := append([]int(nil), b...)
	sort.Ints(aa)
	sort.Ints(bb)
	for i := range aa {
		if aa[i] != bb[i] {
			return false
		}
	}
	return true
}

func instanceSelect(t, d string) (string, []any, error) {
	filters := map[string]string{
		"client_services": `id IN (SELECT client_service_id FROM deployments WHERE id=$1)`,
		"panels":          `id IN (SELECT default_panel_id FROM deployments WHERE id=$1 UNION SELECT panel_id FROM plans WHERE deployment_id=$1 UNION SELECT panel_id FROM subscriptions WHERE deployment_id=$1 UNION SELECT panel_id FROM work_items WHERE deployment_id=$1 UNION SELECT panel_id FROM legacy_panel_assignments WHERE source_instance=(SELECT legacy_source_instance FROM deployments WHERE id=$1))`,
		"deployments":     `id=$1`, "commercial_accounts": `home_deployment_id=$1`, "actors": `deployment_id=$1`, "plans": `deployment_id=$1`,
		"purchase_quotes": `deployment_id=$1`, "wallet_ledger": `deployment_id=$1`, "subscriptions": `deployment_id=$1`, "payment_intents": `deployment_id=$1`, "orders": `deployment_id=$1`, "payment_settlements": `deployment_id=$1`, "work_items": `deployment_id=$1`, "trial_usage": `deployment_id=$1`, "trial_claims": `deployment_id=$1`, "outbox": `deployment_id=$1`, "topup_requests": `deployment_id=$1`, "wallet_credit_approvals": `deployment_id=$1`, "refund_requests": `deployment_id=$1`, "refund_approvals": `deployment_id=$1`, "plan_access": `deployment_id=$1`, "reseller_access_requests": `deployment_id=$1`, "admin_configuration_audit": `deployment_id=$1`, "backend_client_credentials": `deployment_id=$1`,
		"legacy_import_batches": `source_instance=(SELECT legacy_source_instance FROM deployments WHERE id=$1)`, "legacy_id_map": `source_instance=(SELECT legacy_source_instance FROM deployments WHERE id=$1)`, "legacy_records": `source_instance=(SELECT legacy_source_instance FROM deployments WHERE id=$1)`, "legacy_obligations": `source_instance=(SELECT legacy_source_instance FROM deployments WHERE id=$1)`, "migration_audit": `source_instance=(SELECT legacy_source_instance FROM deployments WHERE id=$1)`, "legacy_panel_assignments": `source_instance=(SELECT legacy_source_instance FROM deployments WHERE id=$1)`,
	}
	f, ok := filters[t]
	if !ok {
		return "", nil, fmt.Errorf("unsupported instance table %q", t)
	}
	return fmt.Sprintf(`SELECT to_jsonb(x) FROM %s x WHERE %s ORDER BY to_jsonb(x)::text`, t, f), []any{d}, nil
}

func envValue(b, key string) string {
	for _, line := range strings.Split(b, "\n") {
		i := strings.IndexByte(line, '=')
		if i > 0 && strings.TrimSpace(line[:i]) == key {
			v := strings.TrimSpace(line[i+1:])
			if strings.HasPrefix(v, "\"") {
				if s, e := strconvUnquote(v); e == nil {
					return s
				}
			}
			return v
		}
	}
	return ""
}
func strconvUnquote(s string) (string, error) {
	var v string
	err := json.Unmarshal([]byte(s), &v)
	return v, err
}
func keys(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
func prefixFiles(slug string, files map[string][]byte) map[string][]byte {
	o := map[string][]byte{}
	for k, v := range files {
		o[slug+"/"+k] = v
	}
	return o
}
func nowUTC() time.Time { return time.Now().UTC() }
func readArchiveFiles(path string) (map[string][]byte, error) {
	z, e := zip.OpenReader(path)
	if e != nil {
		return nil, e
	}
	defer z.Close()
	o := map[string][]byte{}
	for _, f := range z.File {
		if f.Name == manifestName {
			continue
		}
		b, e := readZip(f)
		if e != nil {
			return nil, e
		}
		o[f.Name] = b
	}
	return o, nil
}
