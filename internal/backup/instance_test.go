package backup

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"example.com/xui-commerce/backend/internal/secrets"
)

func TestRewrapEncryptedInstanceCredentialsUsesDestinationKey(t *testing.T) {
	from, to := make([]byte, 32), make([]byte, 32)
	from[0] = 1
	to[0] = 2
	sealed, err := secrets.Seal(from, []byte("panel-secret"))
	if err != nil {
		t.Fatal(err)
	}
	row, _ := json.Marshal(map[string]string{"id": "p", "encrypted_api_token": `\x` + hex.EncodeToString(sealed)})
	a := instanceArchive{Tables: map[string][]json.RawMessage{"panels": {row}}}
	if err = rewrapEncryptedRows(&a, from, to); err != nil {
		t.Fatal(err)
	}
	var got map[string]string
	if err = json.Unmarshal(a.Tables["panels"][0], &got); err != nil {
		t.Fatal(err)
	}
	ciphertext, err := hex.DecodeString(got["encrypted_api_token"][2:])
	if err != nil {
		t.Fatal(err)
	}
	plain, err := secrets.Open(to, ciphertext)
	if err != nil || string(plain) != "panel-secret" {
		t.Fatalf("rewrapped credential = %q, %v", plain, err)
	}
	if _, err = secrets.Open(from, ciphertext); err == nil {
		t.Fatal("credential remained encrypted under source key")
	}
}

func TestRuntimeIdentityRequiresMatchingScopedCredentialAndAdmin(t *testing.T) {
	key := make([]byte, 32)
	key[0] = 7
	telegram, err := secrets.Seal(key, []byte("123456789012345678901234567890:secret"))
	if err != nil {
		t.Fatal(err)
	}
	const deployment, token = "instance-a", "scoped-token"
	digest := sha256.Sum256([]byte(token))
	account, _ := json.Marshal(map[string]any{"id": 1, "deployment_id": deployment, "token_hash": `\x` + hex.EncodeToString(digest[:]), "enabled": true})
	actor, _ := json.Marshal(map[string]any{"deployment_id": deployment, "telegram_id": 77, "role": "admin", "approval_status": "approved", "enabled": true})
	id := int64(77)
	dep, _ := json.Marshal(map[string]any{"id": deployment, "admin_telegram_id": id, "encrypted_telegram_token": `\x` + hex.EncodeToString(telegram)})
	a := instanceArchive{Deployment: deployment, Tables: map[string][]json.RawMessage{"backend_client_credentials": {account}, "actors": {actor}, "deployments": {dep}}}
	config := []byte("BACKEND_DEPLOYMENT_ID=\"instance-a\"\nBACKEND_TOKEN=\"scoped-token\"\nADMIN_TELEGRAM_ID=\"77\"\nTELEGRAM_BOT_TOKEN=\"123456789012345678901234567890:secret\"\n")
	if err = validateRuntimeIdentity(&a, config, key); err != nil {
		t.Fatalf("valid runtime identity rejected: %v", err)
	}
	if err = validateRuntimeIdentity(&a, []byte(strings.ReplaceAll(string(config), "scoped-token", "different-token")), key); err == nil {
		t.Fatal("mismatched bearer token passed runtime validation")
	}
}

func TestPortableInstanceStageRewritesLocalURLAndDoesNotStageRecoveryKey(t *testing.T) {
	root := t.TempDir()
	archive := filepath.Join(root, "move.zip")
	database := []byte(`{"format":2,"deployment":"instance-src","schema_migrations":[],"tables":{}}`)
	sum := sha256.Sum256(database)
	files := map[string][]byte{"retail/instance.env": []byte("INSTANCE_ID=\"retail\"\nCHANNEL=\"retail\"\nBACKEND_URL=\"http://127.0.0.1:8090\"\nBACKEND_TOKEN=\"scoped\"\nTELEGRAM_BOT_TOKEN=\"123456789012345678901234567890:secret\"\nBACKEND_DEPLOYMENT_ID=\"instance-src\"\nADMIN_TELEGRAM_ID=\"77\"\n"), "retail/database.json": database, "retail/recovery.key": []byte("base64-key\n")}
	if err := writeArchive(archive, Manifest{Format: 2, Scope: "instance", Instance: "retail", Database: true, DataSHA256: hex.EncodeToString(sum[:]), RowCounts: map[string]int{}, ConfigFiles: []string{"instance.env", "database.json", "recovery.key"}}, nil, files); err != nil {
		t.Fatal(err)
	}
	stage, err := StagePortableInstanceConfig(archive, filepath.Join(root, "stage"), "retail-copy", "http://127.0.0.1:8088", false)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(stage, "instance.env"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `INSTANCE_ID="retail-copy"`) || !strings.Contains(string(data), `BACKEND_URL="http://127.0.0.1:8088"`) || !strings.Contains(string(data), `BACKEND_TOKEN="scoped"`) {
		t.Fatalf("staged runtime config was not safely rebased: %s", data)
	}
	if _, err = os.Stat(filepath.Join(stage, "recovery.key")); !os.IsNotExist(err) {
		t.Fatal("source encryption key was copied into active instance config")
	}
}

func TestInstanceArchiveChecksumRejectsChangedDatabasePayload(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "bad.zip")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	for name, data := range map[string][]byte{manifestName: []byte(`{"format":2,"scope":"instance","instance":"retail","database_included":true,"instance_data_sha256":"wrong"}`), "retail/instance.env": []byte("TOKEN=x"), "retail/recovery.key": []byte("key"), "retail/database.json": []byte(`{"format":2}`)} {
		w, e := zw.Create(name)
		if e != nil {
			t.Fatal(e)
		}
		_, _ = w.Write(data)
	}
	_ = zw.Close()
	_ = f.Close()
	if _, err = Validate(path); err == nil {
		t.Fatal("expected checksum validation failure")
	}
}

func TestRemoteClientPreflightChecksGlobalIdentityAndAttachments(t *testing.T) {
	const uuid, subID, email = "uuid-a", "sub-a", "client@example.test"
	collision := false
	missing := false
	readbackUUID := uuid
	wrongInbound := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/panel/api/server/getPanelUpdateInfo":
			_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "obj": map[string]any{"currentVersion": "3.8.5"}})
		case "/panel/api/inbounds/options":
			_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "obj": []any{map[string]any{"id": 1}}})
		case "/panel/api/clients/list":
			clients := []any{map[string]any{"email": email, "uuid": uuid, "subId": subID, "inboundIds": []int{1}}}
			if missing {
				clients = []any{}
			}
			if collision {
				clients = append(clients, map[string]any{"email": "other@example.test", "uuid": "uuid-b", "subId": subID, "inboundIds": []int{1}})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "obj": clients})
		case "/panel/api/inbounds/list":
			clients := []any{map[string]any{"email": email, "uuid": uuid, "subId": subID}}
			if missing {
				clients = []any{}
			}
			inboundID := 1
			if wrongInbound {
				inboundID = 2
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "obj": []any{map[string]any{"id": inboundID, "clientStats": clients}}})
		case "/panel/api/clients/get/client@example.test":
			_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "obj": map[string]any{"email": email, "uuid": readbackUUID, "subId": subID, "inboundIds": []int{1}}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	key := make([]byte, 32)
	key[0] = 3
	sealed, err := secrets.Seal(key, []byte("api-token"))
	if err != nil {
		t.Fatal(err)
	}
	panel, _ := json.Marshal(map[string]any{"id": "panel-a", "base_url": server.URL, "encrypted_api_token": `\x` + hex.EncodeToString(sealed)})
	sub, _ := json.Marshal(map[string]any{"id": 42, "panel_id": "panel-a", "client_email": email, "client_uuid": uuid, "sub_id": subID, "status": "active", "inbound_ids": []int{1}})
	archive := instanceArchive{Tables: map[string][]json.RawMessage{"panels": {panel}, "subscriptions": {sub}}}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err = validateRemoteClients(ctx, &archive, key); err != nil {
		t.Fatalf("valid panel identities rejected: %v", err)
	}
	// A duplicate subscription ID on a different email must block the move.
	collision = true
	if err = validateRemoteClients(ctx, &archive, key); err == nil {
		t.Fatal("expected global remote subId collision to block restore")
	}
	collision = false
	// Historical rows still participate in collision detection.
	cancelled, _ := json.Marshal(map[string]any{"id": 42, "panel_id": "panel-a", "client_email": email, "client_uuid": uuid, "sub_id": subID, "status": "cancelled", "inbound_ids": []int{1}})
	archive.Tables["subscriptions"] = []json.RawMessage{cancelled}
	collision = true
	if err = validateRemoteClients(ctx, &archive, key); err == nil {
		t.Fatal("expected cancelled identity collision to block restore")
	}
	collision = false
	// Absence is permitted only with durable ready/pending add work, which is
	// the backend's persisted proof that the panel write was definitively absent.
	provisioning, _ := json.Marshal(map[string]any{"id": 42, "panel_id": "panel-a", "client_email": email, "client_uuid": uuid, "sub_id": subID, "status": "provisioning", "inbound_ids": []int{1}})
	work, _ := json.Marshal(map[string]any{"subscription_id": 42, "kind": "provision_add", "status": "pending", "phase": "ready", "attempts": 2, "desired_state": map[string]any{"email": email, "client_uuid": uuid, "sub_id": subID, "panel_id": "panel-a", "inbound_ids": []int{1}}})
	archive.Tables["subscriptions"] = []json.RawMessage{provisioning}
	archive.Tables["work_items"] = []json.RawMessage{work}
	missing = true
	if err = validateRemoteClients(ctx, &archive, key); err != nil {
		t.Fatalf("safe pending no-write subscription rejected: %v", err)
	}
	work, _ = json.Marshal(map[string]any{"subscription_id": 42, "kind": "provision_add", "status": "pending", "phase": "create_attempted", "attempts": 2, "desired_state": map[string]any{"email": email, "client_uuid": uuid, "sub_id": subID, "panel_id": "panel-a", "inbound_ids": []int{1}}})
	archive.Tables["work_items"] = []json.RawMessage{work}
	if err = validateRemoteClients(ctx, &archive, key); err == nil {
		t.Fatal("ambiguous create_attempted work must not accept an absent panel client")
	}
	work, _ = json.Marshal(map[string]any{"subscription_id": 42, "kind": "provision_add", "status": "pending", "phase": "ready", "attempts": 0, "desired_state": map[string]any{"email": email, "client_uuid": uuid, "sub_id": subID, "panel_id": "panel-a", "inbound_ids": []int{1}}})
	archive.Tables["work_items"] = []json.RawMessage{work}
	missing = false
	readbackUUID = "uuid-mismatch"
	if err = validateRemoteClients(ctx, &archive, key); err == nil {
		t.Fatal("mismatched per-email readback must block restore")
	}
	readbackUUID = uuid
	wrongInbound = true
	if err = validateRemoteClients(ctx, &archive, key); err == nil {
		t.Fatal("inbound attachment mismatch must block restore")
	}
}
