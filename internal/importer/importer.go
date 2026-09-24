// Package importer archives a legacy PostgreSQL deployment without merging
// identities or replaying financial/provisioning work.
package importer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

var validSource = map[string]string{
	"retail-finland": "retail",
	"retail-germany": "retail",
	"reseller-turk1": "reseller",
}

var safeIdent = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

type Options struct {
	SourceInstance string
	PanelID        string
	Apply          bool
}

type TableReport struct {
	Name  string `json:"name"`
	Count int64  `json:"count"`
}

type Collision struct {
	Kind           string `json:"kind"`
	EntityType     string `json:"entity_type"`
	LegacyID       string `json:"legacy_id"`
	OtherSource    string `json:"other_source,omitempty"`
	OtherLegacyID  string `json:"other_legacy_id,omitempty"`
	RemoteIdentity string `json:"remote_identity,omitempty"`
	OtherRemoteID  string `json:"other_remote_identity,omitempty"`
	PanelID        string `json:"panel_id,omitempty"`
}

type Report struct {
	SourceInstance      string            `json:"source_instance"`
	Channel             string            `json:"channel"`
	PanelID             string            `json:"target_panel_id"`
	Applied             bool              `json:"applied"`
	Tables              []TableReport     `json:"tables"`
	FinancialSums       map[string]string `json:"financial_sums_exact_source_units"`
	TargetCounts        map[string]int64  `json:"target_archive_counts,omitempty"`
	TargetFinancialSums map[string]string `json:"target_archive_financial_sums_exact_source_units,omitempty"`
	IDCollisions        []Collision       `json:"source_id_collisions"`
	RemoteCollisions    []Collision       `json:"remote_identity_collisions"`
	OpenObligations     map[string]int64  `json:"open_obligations"`
	ArchivedRows        int64             `json:"archived_rows"`
	SkippedRows         int64             `json:"already_archived_rows"`
}

type sourceTable struct {
	schema    string
	name      string
	cols      []string
	pk        []string
	financial []string
}

type querier interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

func Run(ctx context.Context, source, target *pgxpool.Pool, opt Options) (Report, error) {
	channel, ok := validSource[opt.SourceInstance]
	if !ok {
		return Report{}, fmt.Errorf("unsupported source instance %q", opt.SourceInstance)
	}
	if opt.PanelID == "" {
		return Report{}, errors.New("target panel id is required")
	}
	sourceSnapshot, err := source.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return Report{}, fmt.Errorf("begin read-only source snapshot: %w", err)
	}
	defer sourceSnapshot.Rollback(context.Background())
	report := Report{SourceInstance: opt.SourceInstance, Channel: channel, PanelID: opt.PanelID,
		FinancialSums: map[string]string{}, TargetCounts: map[string]int64{}, TargetFinancialSums: map[string]string{}, OpenObligations: map[string]int64{}}
	tables, err := inspectTables(ctx, sourceSnapshot)
	if err != nil {
		return report, err
	}
	for _, table := range tables {
		count, err := rowCount(ctx, sourceSnapshot, table.schema, table.name)
		if err != nil {
			return report, err
		}
		report.Tables = append(report.Tables, TableReport{Name: table.name, Count: count})
		for _, col := range table.financial {
			sum, err := financialSum(ctx, sourceSnapshot, table.schema, table.name, col)
			if err != nil {
				return report, err
			}
			report.FinancialSums[table.name+"."+col] = sum
		}
	}
	if err := remoteCollisionReport(ctx, sourceSnapshot, target, opt, &report); err != nil {
		return report, err
	}
	if err := idCollisionReport(ctx, sourceSnapshot, target, tables, &report); err != nil {
		return report, err
	}
	if err := obligationReport(ctx, sourceSnapshot, tables, &report); err != nil {
		return report, err
	}
	if !opt.Apply {
		return report, nil
	}
	if err := archive(ctx, sourceSnapshot, target, opt, channel, tables, &report); err != nil {
		return report, err
	}
	report.Applied = true
	return report, nil
}

func inspectTables(ctx context.Context, pool querier) ([]sourceTable, error) {
	var schema string
	if err := pool.QueryRow(ctx, `SELECT current_schema()`).Scan(&schema); err != nil {
		return nil, err
	}
	if !safeIdent.MatchString(schema) {
		return nil, fmt.Errorf("unsafe current schema: %q", schema)
	}
	rows, err := pool.Query(ctx, `SELECT table_name FROM information_schema.tables WHERE table_schema=$1 AND table_type='BASE TABLE' ORDER BY table_name`, schema)
	if err != nil {
		return nil, err
	}
	var names []string
	for rows.Next() {
		var name string
		if err = rows.Scan(&name); err != nil {
			rows.Close()
			return nil, err
		}
		names = append(names, name)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	result := make([]sourceTable, 0, len(names))
	for _, name := range names {
		if !safeIdent.MatchString(name) {
			return nil, fmt.Errorf("unsafe table name from catalog: %q", name)
		}
		t := sourceTable{schema: schema, name: name}
		colRows, err := pool.Query(ctx, `SELECT column_name, data_type, COALESCE(c.numeric_precision,0) FROM information_schema.columns c WHERE table_schema=$1 AND table_name=$2 ORDER BY ordinal_position`, schema, name)
		if err != nil {
			return nil, err
		}
		for colRows.Next() {
			var col, typ string
			var precision int
			if err = colRows.Scan(&col, &typ, &precision); err != nil {
				colRows.Close()
				return nil, err
			}
			t.cols = append(t.cols, col)
			if financialColumn(col) && (typ == "smallint" || typ == "integer" || typ == "bigint" || typ == "numeric" || typ == "decimal") {
				t.financial = append(t.financial, col)
			}
		}
		if err = colRows.Err(); err != nil {
			colRows.Close()
			return nil, err
		}
		colRows.Close()
		pkRows, err := pool.Query(ctx, `SELECT a.attname FROM pg_index i JOIN pg_class tbl ON tbl.oid=i.indrelid JOIN pg_namespace ns ON ns.oid=tbl.relnamespace JOIN unnest(i.indkey) WITH ORDINALITY AS k(attnum,ord) ON true JOIN pg_attribute a ON a.attrelid=tbl.oid AND a.attnum=k.attnum WHERE ns.nspname=$1 AND tbl.relname=$2 AND i.indisprimary ORDER BY k.ord`, schema, name)
		if err != nil {
			return nil, err
		}
		for pkRows.Next() {
			var col string
			if err = pkRows.Scan(&col); err != nil {
				pkRows.Close()
				return nil, err
			}
			t.pk = append(t.pk, col)
		}
		if err = pkRows.Err(); err != nil {
			pkRows.Close()
			return nil, err
		}
		pkRows.Close()
		result = append(result, t)
	}
	return result, nil
}

func financialColumn(name string) bool {
	n := strings.ToLower(name)
	for _, part := range []string{"amount", "balance", "price", "cost", "wallet", "refund", "total"} {
		if strings.Contains(n, part) {
			return true
		}
	}
	return false
}

func rowCount(ctx context.Context, p querier, schema, table string) (int64, error) {
	var n int64
	err := p.QueryRow(ctx, `SELECT count(*) FROM `+quote(schema)+`.`+quote(table)).Scan(&n)
	return n, err
}
func financialSum(ctx context.Context, p querier, schema, table, col string) (string, error) {
	var s string
	err := p.QueryRow(ctx, `SELECT COALESCE(sum(`+quote(col)+`::numeric),0)::text FROM `+quote(schema)+`.`+quote(table)).Scan(&s)
	return s, err
}
func quote(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

func idCollisionReport(ctx context.Context, source querier, target *pgxpool.Pool, tables []sourceTable, r *Report) error {
	for _, t := range tables {
		otherRows, e := target.Query(ctx, `SELECT legacy_id,source_instance FROM legacy_id_map WHERE entity_type=$1 AND source_instance<>$2`, t.name, r.SourceInstance)
		if e != nil {
			if missingRelation(e) {
				return nil
			}
			return e
		}
		known := map[string][]string{}
		for otherRows.Next() {
			var id, instance string
			if e = otherRows.Scan(&id, &instance); e != nil {
				otherRows.Close()
				return e
			}
			known[id] = append(known[id], instance)
		}
		if e = otherRows.Err(); e != nil {
			otherRows.Close()
			return e
		}
		otherRows.Close()
		order := t.pk
		orderBy := make([]string, 0, len(order))
		for _, col := range order {
			orderBy = append(orderBy, quote(col))
		}
		query := `SELECT to_jsonb(src) FROM ` + quote(t.schema) + `.` + quote(t.name) + ` src`
		if len(orderBy) > 0 {
			query += ` ORDER BY ` + strings.Join(orderBy, ",")
		}
		rows, err := source.Query(ctx, query)
		if err != nil {
			return err
		}
		occurrences := map[string]int{}
		for rows.Next() {
			var payload []byte
			if err = rows.Scan(&payload); err != nil {
				rows.Close()
				return err
			}
			obj := map[string]any{}
			dec := json.NewDecoder(strings.NewReader(string(payload)))
			dec.UseNumber()
			if err = dec.Decode(&obj); err != nil {
				rows.Close()
				return err
			}
			id, err := legacyKey(obj, t.pk)
			if err != nil {
				rows.Close()
				return err
			}
			if len(t.pk) == 0 {
				sum := sha256.Sum256(payload)
				h := hex.EncodeToString(sum[:])
				n := occurrences[h]
				occurrences[h] = n + 1
				id = h + ":" + fmt.Sprint(n)
			}
			for _, other := range known[id] {
				r.IDCollisions = append(r.IDCollisions, Collision{Kind: "same_source_id", EntityType: t.name, LegacyID: id, OtherSource: other})
			}
		}
		if err = rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
	}
	sort.Slice(r.IDCollisions, func(i, j int) bool {
		a, b := r.IDCollisions[i], r.IDCollisions[j]
		if a.EntityType != b.EntityType {
			return a.EntityType < b.EntityType
		}
		return a.LegacyID < b.LegacyID
	})
	return nil
}

func missingRelation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "42P01"
}

func remoteCollisionReport(ctx context.Context, source querier, target *pgxpool.Pool, opt Options, r *Report) error {
	var exists bool
	err := target.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM legacy_panel_assignments WHERE source_instance=$1)`, opt.SourceInstance).Scan(&exists)
	if err != nil {
		if missingRelation(err) {
			return nil
		}
		return err
	}
	var schema string
	if err := source.QueryRow(ctx, `SELECT current_schema()`).Scan(&schema); err != nil {
		return err
	}
	if rows, err := source.Query(ctx, `SELECT to_regclass($1||'.subscriptions') IS NOT NULL`, schema); err == nil {
		var yes bool
		if rows.Next() {
			_ = rows.Scan(&yes)
		}
		rows.Close()
		if !yes {
			return nil
		}
	} else {
		return err
	}
	cols := map[string]bool{}
	cr, err := source.Query(ctx, `SELECT column_name FROM information_schema.columns WHERE table_schema=$1 AND table_name='subscriptions'`, schema)
	if err != nil {
		return err
	}
	for cr.Next() {
		var c string
		if err = cr.Scan(&c); err != nil {
			cr.Close()
			return err
		}
		cols[c] = true
	}
	cr.Close()
	keys := []string{}
	for _, k := range []string{"client_email", "client_uuid", "sub_id"} {
		if cols[k] {
			keys = append(keys, k)
		}
	}
	if len(keys) == 0 {
		return nil
	}
	query := `SELECT ` + quote(firstPKOrID(cols)) + `::text, to_jsonb(s) FROM ` + quote(schema) + `.subscriptions s`
	rows, err := source.Query(ctx, query)
	if err != nil {
		return err
	}
	defer rows.Close()
	type remote struct{ source, id, panel, kind, value string }
	var src []remote
	for rows.Next() {
		var id string
		var raw []byte
		if err = rows.Scan(&id, &raw); err != nil {
			return err
		}
		var m map[string]any
		if err = json.Unmarshal(raw, &m); err != nil {
			return err
		}
		for _, k := range keys {
			v, _ := m[k].(string)
			if v != "" {
				src = append(src, remote{opt.SourceInstance, id, opt.PanelID, k, v})
			}
		}
	}
	if err = rows.Err(); err != nil {
		return err
	}
	others, err := target.Query(ctx, `SELECT a.source_instance, r.legacy_id, a.panel_id, key, value FROM legacy_panel_assignments a JOIN legacy_records r ON r.source_instance=a.source_instance AND r.entity_type='subscriptions' CROSS JOIN LATERAL jsonb_each_text(r.payload) e(key,value) WHERE a.source_instance<>$1 AND a.panel_id=$2 AND key IN ('client_email','client_uuid','sub_id')`, opt.SourceInstance, opt.PanelID)
	if err != nil {
		if missingRelation(err) {
			return nil
		}
		return err
	}
	defer others.Close()
	type old struct{ source, id, panel, kind, value string }
	var prev []old
	for others.Next() {
		var o old
		if err = others.Scan(&o.source, &o.id, &o.panel, &o.kind, &o.value); err != nil {
			return err
		}
		prev = append(prev, o)
	}
	if err = others.Err(); err != nil {
		return err
	}
	for _, a := range src {
		for _, b := range prev {
			if a.kind == b.kind && a.value == b.value {
				r.RemoteCollisions = append(r.RemoteCollisions, Collision{Kind: "panel_remote_identity", EntityType: "subscriptions", LegacyID: a.id, OtherSource: b.source, OtherLegacyID: b.id, RemoteIdentity: a.kind + "=" + a.value, OtherRemoteID: b.kind + "=" + b.value, PanelID: opt.PanelID})
			}
		}
	}
	return nil
}

func firstPKOrID(cols map[string]bool) string {
	if cols["id"] {
		return "id"
	}
	return "ctid"
}

func obligationReport(ctx context.Context, source querier, tables []sourceTable, r *Report) error {
	names := map[string]bool{}
	for _, t := range tables {
		names[t.name] = true
	}
	for name, kind := range map[string]string{"purchase_requests": "pending_payment", "payment_intents": "pending_payment", "topup_requests": "pending_topup", "refund_requests": "pending_refund", "reconciliation_records": "reconciliation"} {
		if !names[name] {
			continue
		}
		var count int64
		var schema string
		if err := source.QueryRow(ctx, `SELECT current_schema()`).Scan(&schema); err != nil {
			return err
		}
		query := `SELECT count(*) FROM ` + quote(schema) + `.` + quote(name)
		cols, err := columnSet(ctx, source, schema, name)
		if err != nil {
			return err
		}
		if cols["status"] {
			query += ` WHERE lower(COALESCE(status::text,'')) NOT IN ('approved','completed','complete','rejected','cancelled','canceled','resolved','failed','expired','closed')`
		}
		if err := source.QueryRow(ctx, query).Scan(&count); err != nil {
			return err
		}
		if count > 0 {
			r.OpenObligations[kind] += count
		}
	}
	return nil
}
func columnSet(ctx context.Context, p querier, schema, t string) (map[string]bool, error) {
	rows, e := p.Query(ctx, `SELECT column_name FROM information_schema.columns WHERE table_schema=$1 AND table_name=$2`, schema, t)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	m := map[string]bool{}
	for rows.Next() {
		var c string
		if e = rows.Scan(&c); e != nil {
			return nil, e
		}
		m[c] = true
	}
	return m, rows.Err()
}

func archive(ctx context.Context, source querier, target *pgxpool.Pool, opt Options, channel string, tables []sourceTable, r *Report) (retErr error) {
	conn, err := target.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	lockKey := "legacy-import:" + opt.SourceInstance
	if _, err = conn.Exec(ctx, `SELECT pg_advisory_lock(hashtextextended($1,0))`, lockKey); err != nil {
		return err
	}
	defer func() {
		if _, unlockErr := conn.Exec(context.Background(), `SELECT pg_advisory_unlock(hashtextextended($1,0))`, lockKey); retErr == nil && unlockErr != nil {
			retErr = unlockErr
		}
	}()
	tx, err := conn.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() {
		if tx != nil {
			_ = tx.Rollback(context.Background())
		}
	}()
	fingerprint := sourceFingerprint(r.Tables, r.FinancialSums)
	var priorFingerprint string
	err = tx.QueryRow(ctx, `SELECT source_fingerprint FROM legacy_import_batches WHERE source_instance=$1`, opt.SourceInstance).Scan(&priorFingerprint)
	if err == nil && priorFingerprint != fingerprint {
		return errors.New("legacy source counts or financial totals changed since the batch baseline; stop and review the partially archived source before proceeding")
	}
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if errors.Is(err, pgx.ErrNoRows) {
		_, err = tx.Exec(ctx, `INSERT INTO legacy_import_batches(source_instance,channel,source_fingerprint,status,source_counts,source_financial_totals) VALUES($1,$2,$3,'started',$4::jsonb,$5::jsonb)`, opt.SourceInstance, channel, fingerprint, jsonMap(r.Tables), mapJSON(r.FinancialSums))
	} else {
		_, err = tx.Exec(ctx, `UPDATE legacy_import_batches SET updated_at=now() WHERE source_instance=$1`, opt.SourceInstance)
	}
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO legacy_panel_assignments(source_instance,panel_id) VALUES($1,$2) ON CONFLICT(source_instance) DO UPDATE SET panel_id=EXCLUDED.panel_id WHERE legacy_panel_assignments.panel_id=EXCLUDED.panel_id`, opt.SourceInstance, opt.PanelID)
	if err != nil {
		return fmt.Errorf("panel assignment changed; create a reviewed new import plan: %w", err)
	}
	var panel string
	if err = tx.QueryRow(ctx, `SELECT panel_id FROM legacy_panel_assignments WHERE source_instance=$1`, opt.SourceInstance).Scan(&panel); err != nil {
		return err
	}
	if panel != opt.PanelID {
		return errors.New("source instance already assigned to a different target panel")
	}
	if err = tx.Commit(ctx); err != nil {
		return err
	}
	tx = nil
	for _, t := range tables {
		if err = archiveTable(ctx, source, conn, opt, t, r); err != nil {
			return fmt.Errorf("archive %s: %w", t.name, err)
		}
	}
	tx, err = conn.Begin(ctx)
	if err != nil {
		return err
	}
	for _, t := range tables {
		var count int64
		if err = tx.QueryRow(ctx, `SELECT count(*) FROM legacy_records WHERE source_instance=$1 AND entity_type=$2`, opt.SourceInstance, t.name).Scan(&count); err != nil {
			return err
		}
		r.TargetCounts[t.name] = count
		var sourceCount int64
		for _, item := range r.Tables {
			if item.Name == t.name {
				sourceCount = item.Count
				break
			}
		}
		if count != sourceCount {
			return fmt.Errorf("archive count mismatch for %s: source=%d target=%d", t.name, sourceCount, count)
		}
		for _, col := range t.financial {
			var sum string
			query := `SELECT COALESCE(sum((payload->>$3)::numeric),0)::text FROM legacy_records WHERE source_instance=$1 AND entity_type=$2`
			if err = tx.QueryRow(ctx, query, opt.SourceInstance, t.name, col).Scan(&sum); err != nil {
				return fmt.Errorf("archive financial total %s.%s: %w", t.name, col, err)
			}
			key := t.name + "." + col
			r.TargetFinancialSums[key] = sum
			if !sameNumber(r.FinancialSums[key], sum) {
				return fmt.Errorf("archive financial total mismatch for %s: source=%s target=%s", key, r.FinancialSums[key], sum)
			}
		}
	}
	_, err = tx.Exec(ctx, `UPDATE legacy_import_batches SET status='complete',source_counts=$2::jsonb,source_financial_totals=$3::jsonb,target_counts=$4::jsonb,target_financial_totals=$5::jsonb,collision_report=$6::jsonb,updated_at=now() WHERE source_instance=$1`, opt.SourceInstance, jsonMap(r.Tables), mapJSON(r.FinancialSums), mapJSON(r.TargetCounts), mapJSON(r.TargetFinancialSums), mapJSON(append(r.IDCollisions, r.RemoteCollisions...)))
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO migration_audit(source_instance,action,details) VALUES($1,'raw_archive_import',$2::jsonb)`, opt.SourceInstance, mapJSON(map[string]any{"source_counts": r.Tables, "financial_sums": r.FinancialSums, "id_collisions": r.IDCollisions, "remote_collisions": r.RemoteCollisions, "obligations": r.OpenObligations}))
	if err != nil {
		return err
	}
	if err = tx.Commit(ctx); err != nil {
		return err
	}
	return nil
}

func archiveTable(ctx context.Context, source querier, conn *pgxpool.Conn, opt Options, t sourceTable, r *Report) error {
	order := t.pk
	orderBy := make([]string, 0, len(order))
	for _, c := range order {
		orderBy = append(orderBy, quote(c))
	}
	orderSQL := ""
	if len(orderBy) > 0 {
		orderSQL = ` ORDER BY ` + strings.Join(orderBy, ",")
	}
	rows, err := source.Query(ctx, `SELECT to_jsonb(src) FROM `+quote(t.schema)+`.`+quote(t.name)+` src`+orderSQL)
	if err != nil {
		return err
	}
	defer rows.Close()
	occurrences := map[string]int{}
	var tx pgx.Tx
	chunkRows := 0
	defer func() {
		if tx != nil {
			_ = tx.Rollback(context.Background())
		}
	}()
	for rows.Next() {
		if tx == nil {
			var err error
			tx, err = conn.Begin(ctx)
			if err != nil {
				return err
			}
		}
		var payload []byte
		if err := rows.Scan(&payload); err != nil {
			return err
		}
		obj := map[string]any{}
		dec := json.NewDecoder(strings.NewReader(string(payload)))
		dec.UseNumber()
		if err := dec.Decode(&obj); err != nil {
			return err
		}
		id, err := legacyKey(obj, t.pk)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(payload)
		hash := hex.EncodeToString(sum[:])
		if len(t.pk) == 0 {
			n := occurrences[hash]
			occurrences[hash] = n + 1
			id = hash + ":" + fmt.Sprint(n)
		}
		targetID := opt.SourceInstance + ":" + t.name + ":" + id
		command, err := tx.Exec(ctx, `INSERT INTO legacy_id_map(source_instance,entity_type,legacy_id,target_entity_type,target_id,content_hash) VALUES($1,$2,$3,'legacy_record',$4,$5) ON CONFLICT(source_instance,entity_type,legacy_id) DO NOTHING`, opt.SourceInstance, t.name, id, targetID, hash)
		if err != nil {
			return err
		}
		if command.RowsAffected() == 0 {
			var old string
			if err = tx.QueryRow(ctx, `SELECT content_hash FROM legacy_id_map WHERE source_instance=$1 AND entity_type=$2 AND legacy_id=$3`, opt.SourceInstance, t.name, id).Scan(&old); err != nil {
				return err
			}
			if old != hash {
				return fmt.Errorf("source row changed after prior import: %s/%s", t.name, id)
			}
			r.SkippedRows++
			continue
		}
		if _, err = tx.Exec(ctx, `INSERT INTO legacy_records(source_instance,entity_type,legacy_id,payload,content_hash) VALUES($1,$2,$3,$4::jsonb,$5)`, opt.SourceInstance, t.name, id, string(payload), hash); err != nil {
			return err
		}
		r.ArchivedRows++
		if kind := obligationKind(t.name, obj); kind != "" {
			if _, err = tx.Exec(ctx, `INSERT INTO legacy_obligations(source_instance,entity_type,legacy_id,kind,payload) VALUES($1,$2,$3,$4,$5::jsonb) ON CONFLICT DO NOTHING`, opt.SourceInstance, t.name, id, kind, string(payload)); err != nil {
				return err
			}
		}
		chunkRows++
		if chunkRows >= 500 {
			if err = tx.Commit(ctx); err != nil {
				return err
			}
			tx = nil
			chunkRows = 0
		}
	}
	if err = rows.Err(); err != nil {
		return err
	}
	if tx != nil {
		if err = tx.Commit(ctx); err != nil {
			return err
		}
		tx = nil
	}
	return nil
}

func legacyKey(row map[string]any, pk []string) (string, error) {
	if len(pk) == 0 {
		return "", nil
	}
	key := map[string]any{}
	for _, c := range pk {
		v, ok := row[c]
		if !ok {
			return "", fmt.Errorf("primary key column %s missing", c)
		}
		key[c] = v
	}
	if len(pk) == 1 {
		return fmt.Sprint(key[pk[0]]), nil
	}
	b, e := json.Marshal(key)
	return string(b), e
}
func obligationKind(table string, row map[string]any) string {
	status := strings.ToLower(fmt.Sprint(row["status"]))
	switch table {
	case "purchase_requests", "payment_intents":
		if !terminal(status) {
			return "pending_payment"
		}
	case "topup_requests":
		if !terminal(status) {
			return "pending_topup"
		}
	case "refund_requests":
		if !terminal(status) {
			return "pending_refund"
		}
	case "reconciliation_records":
		if !terminal(status) {
			return "reconciliation"
		}
	}
	return ""
}
func terminal(s string) bool {
	switch s {
	case "approved", "completed", "complete", "rejected", "cancelled", "canceled", "resolved", "failed", "expired", "closed":
		return true
	}
	return false
}
func jsonMap(v any) string {
	b, e := json.Marshal(v)
	if e != nil {
		return "{}"
	}
	return string(b)
}
func mapJSON(v any) string { return jsonMap(v) }
func sameNumber(a, b string) bool {
	left, ok := new(big.Rat).SetString(a)
	if !ok {
		return a == b
	}
	right, ok := new(big.Rat).SetString(b)
	return ok && left.Cmp(right) == 0
}
func sourceFingerprint(counts []TableReport, totals map[string]string) string {
	data, _ := json.Marshal(struct {
		Counts []TableReport     `json:"counts"`
		Totals map[string]string `json:"totals"`
	}{counts, totals})
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

// SchemaFingerprint returns a deterministic public table/column inventory for runbooks.
func SchemaFingerprint(ctx context.Context, p *pgxpool.Pool) (string, error) {
	ts, e := inspectTables(ctx, p)
	if e != nil {
		return "", e
	}
	items := []string{}
	for _, t := range ts {
		items = append(items, t.name+":"+strings.Join(t.cols, ","))
	}
	sort.Strings(items)
	b, _ := json.Marshal(items)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:]), nil
}

// Ensure the local pgx types used by the archive scanner remain explicit at compile time.
var _ pgx.Tx
