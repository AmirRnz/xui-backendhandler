package integration

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"example.com/xui-commerce/backend/internal/backup"
	"example.com/xui-commerce/backend/internal/secrets"
	"example.com/xui-commerce/backend/internal/store"
)

func TestCompleteInstanceBackupRestoresScopedCommerceAndQuarantinesWork(t *testing.T) {
	source, sourcePool := testStore(t)
	_, targetPool := testStore(t)
	ctx := context.Background()
	panelServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/panel/api/server/getPanelUpdateInfo":
			_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "obj": map[string]any{"currentVersion": "3.8.5"}})
		case "/panel/api/inbounds/options":
			_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "obj": []any{}})
		case "/panel/api/clients/list", "/panel/api/inbounds/list":
			_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "obj": []any{}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer panelServer.Close()
	sourceKey := make([]byte, 32)
	sourceKey[0] = 0x42
	targetKey := make([]byte, 32)
	targetKey[0] = 0x21
	sharedPanelToken, err := secrets.Seal(targetKey, []byte("panel-api-secret"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = targetPool.Exec(ctx, `INSERT INTO panels(id,base_url,enabled,encrypted_api_token) VALUES($1,$2,true,$3)`, "panel-retail-move-test", panelServer.URL, sharedPanelToken); err != nil {
		t.Fatal(err)
	}
	registered, err := source.RegisterInstance(ctx, store.RegisterInstanceInput{DeploymentID: "retail-move-test", Channel: "retail", DisplayName: "Move Test", PanelID: "panel-retail-move-test", PanelURL: panelServer.URL, PanelToken: "panel-api-secret", TelegramToken: "123456789012345678901234567890:secret", AdminTelegramID: 77123}, sourceKey)
	if err != nil {
		t.Fatal(err)
	}
	other, err := source.RegisterInstance(ctx, store.RegisterInstanceInput{DeploymentID: "retail-other-test", Channel: "retail", DisplayName: "Other Test", PanelID: "panel-retail-other-test", PanelURL: panelServer.URL, PanelToken: "other-panel-secret", TelegramToken: "123456789012345678901234567890:other", AdminTelegramID: 88123}, sourceKey)
	if err != nil {
		t.Fatal(err)
	}
	_ = other
	var accountID, actorID, planID int64
	if err = sourcePool.QueryRow(ctx, `SELECT a.id,x.id FROM commercial_accounts a JOIN actors x ON x.account_id=a.id WHERE a.home_deployment_id=$1`, registered.DeploymentID).Scan(&accountID, &actorID); err != nil {
		t.Fatal(err)
	}
	if err = sourcePool.QueryRow(ctx, `INSERT INTO plans(deployment_id,panel_id,kind,name,base_price_toman) VALUES($1,$2,'paid','Historical plan',100000) RETURNING id`, registered.DeploymentID, "panel-retail-move-test").Scan(&planID); err != nil {
		t.Fatal(err)
	}
	var quoteID int64
	if err = sourcePool.QueryRow(ctx, `INSERT INTO purchase_quotes(deployment_id,account_id,actor_id,quote_key,plan_id,plan_snapshot,terms,months,ip_limit,data_gb,final_price_toman) VALUES($1,$2,$3,'immutable-q',$4,'{}','{}',1,1,10,100000) RETURNING id`, registered.DeploymentID, accountID, actorID, planID).Scan(&quoteID); err != nil {
		t.Fatal(err)
	}
	if _, err = sourcePool.Exec(ctx, `INSERT INTO wallet_ledger(deployment_id,account_id,actor_id,amount_toman,balance_after_toman,entry_type,description,operation_key,input_hash) VALUES($1,$2,$3,250000,250000,'credit','historical credit','credit-1','hash-1')`, registered.DeploymentID, accountID, actorID); err != nil {
		t.Fatal(err)
	}
	var intentID int64
	if err = sourcePool.QueryRow(ctx, `INSERT INTO payment_intents(deployment_id,account_id,actor_id,quote_id,intent_key,input_hash,amount_toman,terms,status) VALUES($1,$2,$3,$4,'intent-1','hash-2',100000,'{}','awaiting_receipt') RETURNING id`, registered.DeploymentID, accountID, actorID, quoteID).Scan(&intentID); err != nil {
		t.Fatal(err)
	}
	if _, err = sourcePool.Exec(ctx, `INSERT INTO orders(deployment_id,account_id,actor_id,quote_id,operation_key,input_hash,payment_method,status,payment_intent_id) VALUES($1,$2,$3,$4,'order-1','hash-3','direct','awaiting_payment',$5)`, registered.DeploymentID, accountID, actorID, quoteID, intentID); err != nil {
		t.Fatal(err)
	}
	if _, err = sourcePool.Exec(ctx, `INSERT INTO outbox(deployment_id,actor_id,dedupe_key,topic,payload,status,restore_quarantined) VALUES($1,$2,'notice-1','payment_pending','{}','pending',false)`, registered.DeploymentID, actorID); err != nil {
		t.Fatal(err)
	}
	if _, err = sourcePool.Exec(ctx, `INSERT INTO work_items(deployment_id,account_id,actor_id,panel_id,operation_key,input_hash,kind,desired_state,phase,status) VALUES($1,$2,$3,$4,'uncertain-1','hash-4','provision_add','{}','create_attempted','manual_review')`, registered.DeploymentID, accountID, actorID, "panel-retail-move-test"); err != nil {
		t.Fatal(err)
	}
	if _, err = sourcePool.Exec(ctx, `UPDATE deployments SET transfer_frozen=true WHERE id=$1`, registered.DeploymentID); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	instanceRoot := filepath.Join(root, "instances")
	instanceDir := filepath.Join(instanceRoot, "retail-move")
	if err = os.MkdirAll(instanceDir, 0700); err != nil {
		t.Fatal(err)
	}
	config := fmt.Sprintf("INSTANCE_ID=%q\nCHANNEL=%q\nDISPLAY_NAME=%q\nBACKEND_DEPLOYMENT_ID=%q\nBACKEND_URL=%q\nBACKEND_TOKEN=%q\nTELEGRAM_BOT_TOKEN=%q\nADMIN_TELEGRAM_ID=%q\n", "retail-move", "retail", "Move Test", registered.DeploymentID, "http://127.0.0.1:8088", registered.BackendToken, "123456789012345678901234567890:secret", strconv.FormatInt(77123, 10))
	if err = os.WriteFile(filepath.Join(instanceDir, "instance.env"), []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	backendEnv := fmt.Sprintf("BACKEND_PANEL_SECRETS_KEY=%q\n", base64.StdEncoding.EncodeToString(sourceKey))
	if err = os.WriteFile(filepath.Join(root, "backend.env"), []byte(backendEnv), 0600); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(root, "retail.zip")
	report, err := backup.CreateInstance(ctx, sourcePool, instanceRoot, "retail-move", archive)
	if err != nil {
		t.Fatal(err)
	}
	if report.Counts["orders"] != 1 || report.Counts["wallet_ledger"] != 1 || report.Counts["payment_intents"] != 1 || report.UncertainWorkItems != 1 {
		t.Fatalf("snapshot omitted commercial or uncertain rows: %+v", report)
	}
	_, collisionPool := testStore(t)
	if _, err = collisionPool.Exec(ctx, `INSERT INTO panels(id,base_url,enabled,encrypted_api_token) VALUES($1,'https://other-panel.example.test',true,$2)`, "panel-retail-move-test", sharedPanelToken); err != nil {
		t.Fatal(err)
	}
	if _, err = backup.RestoreInstance(ctx, collisionPool, archive, "retail-copy", base64.StdEncoding.EncodeToString(targetKey), true); err == nil {
		t.Fatal("destination panel ID with different endpoint must block restore")
	}
	if _, err = backup.RestoreInstance(ctx, targetPool, archive, "retail-copy", base64.StdEncoding.EncodeToString(targetKey), true); err != nil {
		t.Fatalf("collision dry-run failed: %v", err)
	}
	if _, err = backup.RestoreInstance(ctx, targetPool, archive, "retail-copy", base64.StdEncoding.EncodeToString(targetKey), false); err != nil {
		t.Fatalf("restore failed: %v", err)
	}
	var gotOrders, gotLedger, gotIntents, gotWork, quarantined int
	if err = targetPool.QueryRow(ctx, `SELECT (SELECT count(*) FROM orders WHERE deployment_id=$1),(SELECT count(*) FROM wallet_ledger WHERE deployment_id=$1),(SELECT count(*) FROM payment_intents WHERE deployment_id=$1),(SELECT count(*) FROM work_items WHERE deployment_id=$1),(SELECT count(*) FROM work_items WHERE deployment_id=$1 AND restore_quarantined)`, registered.DeploymentID).Scan(&gotOrders, &gotLedger, &gotIntents, &gotWork, &quarantined); err != nil {
		t.Fatal(err)
	}
	if gotOrders != 1 || gotLedger != 1 || gotIntents != 1 || gotWork != 1 || quarantined != 1 {
		t.Fatalf("restore lost history or queue quarantine: %d %d %d %d %d", gotOrders, gotLedger, gotIntents, gotWork, quarantined)
	}
	var frozen bool
	if err = targetPool.QueryRow(ctx, `SELECT transfer_frozen FROM deployments WHERE id=$1`, registered.DeploymentID).Scan(&frozen); err != nil || !frozen {
		t.Fatal("restored deployment must remain frozen until local activation completes")
	}
	var otherPresent bool
	if err = targetPool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM deployments WHERE id=$1)`, other.DeploymentID).Scan(&otherPresent); err != nil {
		t.Fatal(err)
	}
	if otherPresent {
		t.Fatal("instance backup leaked another deployment")
	}
	var nextActor int64
	if err = targetPool.QueryRow(ctx, `SELECT nextval(pg_get_serial_sequence('actors','id'))`).Scan(&nextActor); err != nil {
		t.Fatal(err)
	}
	var maxActor int64
	if err = targetPool.QueryRow(ctx, `SELECT max(id) FROM actors`).Scan(&maxActor); err != nil {
		t.Fatal(err)
	}
	if nextActor <= maxActor {
		t.Fatalf("actor sequence was not advanced: next=%d max=%d", nextActor, maxActor)
	}
	if _, err = backup.RestoreInstance(ctx, targetPool, archive, "retail-copy", base64.StdEncoding.EncodeToString(targetKey), false); err != nil {
		t.Fatalf("matching frozen import should resume idempotently: %v", err)
	}
	if _, err = targetPool.Exec(ctx, `UPDATE deployments SET transfer_frozen=false WHERE id=$1`, registered.DeploymentID); err != nil {
		t.Fatal(err)
	}
	if _, err = backup.RestoreInstance(ctx, targetPool, archive, "retail-copy", base64.StdEncoding.EncodeToString(targetKey), false); err == nil {
		t.Fatal("restore must reject an already activated deployment")
	}
}
