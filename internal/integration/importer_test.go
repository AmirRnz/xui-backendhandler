package integration

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"example.com/xui-commerce/backend/internal/importer"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestLegacyArchiveDryRunResumeCollisionAndObligations(t *testing.T) {
	backend, target := testStore(t)
	_ = backend
	ctx := context.Background()
	schema := fmt.Sprintf("legacy_%x", time.Now().UnixNano())
	if _, err := target.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	legacyCfg, err := pgxpool.ParseConfig(os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	if legacyCfg.ConnConfig.RuntimeParams == nil {
		legacyCfg.ConnConfig.RuntimeParams = map[string]string{}
	}
	legacyCfg.ConnConfig.RuntimeParams["search_path"] = schema
	legacy, err := pgxpool.NewWithConfig(ctx, legacyCfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		legacy.Close()
		if _, e := target.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE"); e != nil {
			t.Errorf("drop source schema: %v", e)
		}
	})
	for _, statement := range []string{
		`CREATE TABLE bot_users(id BIGINT PRIMARY KEY, telegram_id BIGINT NOT NULL, wallet_balance BIGINT NOT NULL)`,
		`CREATE TABLE subscriptions(id BIGINT PRIMARY KEY, user_id BIGINT NOT NULL, client_email TEXT NOT NULL, client_uuid TEXT NOT NULL, sub_id TEXT NOT NULL)`,
		`CREATE TABLE purchase_requests(id BIGINT PRIMARY KEY, amount BIGINT NOT NULL, status TEXT NOT NULL)`,
		`CREATE TABLE items(id BIGINT PRIMARY KEY, amount BIGINT NOT NULL)`,
		`INSERT INTO bot_users VALUES(7, 9001, 12500)`,
		`INSERT INTO subscriptions VALUES(7, 7, 'same@example', 'uuid-one', 'sub-one')`,
		`INSERT INTO purchase_requests VALUES(21, 8000, 'pending')`,
		`INSERT INTO items SELECT i, i*100 FROM generate_series(1,600) AS i`,
	} {
		if _, err = legacy.Exec(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	first, err := importer.Run(ctx, legacy, target, importer.Options{SourceInstance: "retail-finland", PanelID: "panel-retail-finland"})
	if err != nil {
		t.Fatal(err)
	}
	if first.Applied || len(first.Tables) != 4 || first.FinancialSums["bot_users.wallet_balance"] != "12500" || first.FinancialSums["items.amount"] != "18030000" || first.OpenObligations["pending_payment"] != 1 {
		t.Fatalf("unexpected dry-run report: %+v", first)
	}
	var batches int
	if err = target.QueryRow(ctx, `SELECT count(*) FROM legacy_import_batches`).Scan(&batches); err != nil {
		t.Fatal(err)
	}
	if batches != 0 {
		t.Fatal("dry run wrote to the backend")
	}
	for _, statement := range []string{
		`CREATE FUNCTION fail_import_item() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.entity_type='items' AND NEW.legacy_id='551' THEN RAISE EXCEPTION 'simulated archive interruption'; END IF; RETURN NEW; END $$`,
		`CREATE TRIGGER fail_import_item BEFORE INSERT ON legacy_records FOR EACH ROW EXECUTE FUNCTION fail_import_item()`,
	} {
		if _, err = target.Exec(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = importer.Run(ctx, legacy, target, importer.Options{SourceInstance: "retail-finland", PanelID: "panel-retail-finland", Apply: true}); err == nil {
		t.Fatal("fault injection did not interrupt the first archive run")
	}
	var checkpointRows int
	if err = target.QueryRow(ctx, `SELECT count(*) FROM legacy_records WHERE source_instance='retail-finland' AND entity_type='items'`).Scan(&checkpointRows); err != nil {
		t.Fatal(err)
	}
	if checkpointRows != 500 {
		t.Fatalf("expected one committed archive chunk, got %d rows", checkpointRows)
	}
	if _, err = target.Exec(ctx, `DROP TRIGGER fail_import_item ON legacy_records`); err != nil {
		t.Fatal(err)
	}
	if _, err = target.Exec(ctx, `DROP FUNCTION fail_import_item()`); err != nil {
		t.Fatal(err)
	}
	first, err = importer.Run(ctx, legacy, target, importer.Options{SourceInstance: "retail-finland", PanelID: "panel-retail-finland", Apply: true})
	if err != nil {
		t.Fatal(err)
	}
	if !first.Applied || first.ArchivedRows != 102 || first.SkippedRows != 501 || first.TargetCounts["items"] != 600 || first.TargetFinancialSums["bot_users.wallet_balance"] != "12500" {
		t.Fatalf("unexpected applied report: %+v", first)
	}
	replay, err := importer.Run(ctx, legacy, target, importer.Options{SourceInstance: "retail-finland", PanelID: "panel-retail-finland", Apply: true})
	if err != nil {
		t.Fatal(err)
	}
	if replay.ArchivedRows != 0 || replay.SkippedRows != 603 {
		t.Fatalf("rerun was not idempotent: %+v", replay)
	}
	secondDry, err := importer.Run(ctx, legacy, target, importer.Options{SourceInstance: "retail-germany", PanelID: "panel-retail-finland"})
	if err != nil {
		t.Fatal(err)
	}
	if secondDry.Applied || len(secondDry.IDCollisions) == 0 || len(secondDry.RemoteCollisions) == 0 {
		t.Fatalf("dry-run failed to expose identity collisions: %+v", secondDry)
	}
	second, err := importer.Run(ctx, legacy, target, importer.Options{SourceInstance: "retail-germany", PanelID: "panel-retail-finland", Apply: true})
	if err != nil {
		t.Fatal(err)
	}
	if !second.Applied || len(second.IDCollisions) == 0 || len(second.RemoteCollisions) == 0 {
		t.Fatalf("same source IDs and panel remote identities were not reported: %+v", second)
	}
	if _, err = importer.Run(ctx, legacy, target, importer.Options{SourceInstance: "reseller-turk1", PanelID: "panel-reseller-turk1", Apply: true}); err != nil {
		t.Fatal(err)
	}
	var sourceRows, obligations int
	if err = target.QueryRow(ctx, `SELECT count(*) FROM legacy_records WHERE source_instance='retail-finland'`).Scan(&sourceRows); err != nil {
		t.Fatal(err)
	}
	if err = target.QueryRow(ctx, `SELECT count(*) FROM legacy_obligations WHERE source_instance='retail-finland' AND status='open'`).Scan(&obligations); err != nil {
		t.Fatal(err)
	}
	if sourceRows != 603 || obligations != 1 {
		t.Fatalf("source identity or pending obligation not retained: records=%d obligations=%d", sourceRows, obligations)
	}
	if _, err = legacy.Exec(ctx, `UPDATE bot_users SET wallet_balance=12501 WHERE id=7`); err != nil {
		t.Fatal(err)
	}
	if _, err = importer.Run(ctx, legacy, target, importer.Options{SourceInstance: "retail-finland", PanelID: "panel-retail-finland", Apply: true}); err == nil {
		t.Fatal("source drift after initial import was overwritten instead of blocked")
	}
}
