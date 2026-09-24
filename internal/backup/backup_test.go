package backup

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestInstanceConfigBackupRestoreIsConfigOnlyAndOwnerOnly(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "instances", "retail-finland")
	if err := os.MkdirAll(source, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "instance.env"), []byte("TOKEN=secret\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "metadata.json"), []byte(`{"kind":"retail"}`), 0600); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(root, "retail.zip")
	if err := CreateInstanceConfig(filepath.Join(root, "instances"), "retail-finland", archive); err != nil {
		t.Fatal(err)
	}
	manifest, err := Validate(archive)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Scope != "instance-config" || manifest.Database || !manifest.ConfigOnly {
		t.Fatalf("unexpected manifest: %+v", manifest)
	}
	staging := filepath.Join(root, "review")
	preview, err := StageInstanceConfig(archive, staging, "retail-copy", true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(staging); !os.IsNotExist(err) {
		t.Fatalf("dry run wrote files: %v", err)
	}
	if preview == "" {
		t.Fatal("dry run did not report a review path")
	}
	staged, err := StageInstanceConfig(archive, staging, "retail-copy", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "instances", "retail-copy")); !os.IsNotExist(err) {
		t.Fatalf("restore created a runnable instance: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(staged, "instance.env"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "TOKEN=secret\n" {
		t.Fatalf("restored config mismatch")
	}
	second, err := StageInstanceConfig(archive, staging, "retail-copy", false)
	if err != nil {
		t.Fatal(err)
	}
	if second == staged {
		t.Fatal("staging reused an existing directory")
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(filepath.Join(staged, "instance.env"))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0600 {
			t.Fatalf("staged file permissions = %o, want 600", info.Mode().Perm())
		}
	}
}

func TestGlobalConfigStagesToNewInactiveDirectory(t *testing.T) {
	root := t.TempDir()
	archive := filepath.Join(root, "global.zip")
	dump := []byte("-- database dump\n")
	sum := sha256.Sum256(dump)
	manifest := Manifest{Format: 1, Scope: "global", CreatedAt: time.Now().UTC(), Database: true, SQLSHA256: hex.EncodeToString(sum[:])}
	files := map[string][]byte{"backend.env": []byte("DATABASE_URL=postgres://target/db\n"), "retail/instance.env": []byte("TOKEN=secret\n"), "retail/metadata.json": []byte(`{"slug":"retail"}`)}
	if err := writeArchive(archive, manifest, dump, files); err != nil {
		t.Fatal(err)
	}
	parent := filepath.Join(root, "review")
	preview, err := StageGlobalConfig(archive, parent, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(parent); !os.IsNotExist(err) {
		t.Fatalf("dry run created staging directory: %v", err)
	}
	if preview == "" {
		t.Fatal("dry run did not report staging path")
	}
	staged, err := StageGlobalConfig(archive, parent, false)
	if err != nil {
		t.Fatal(err)
	}
	for rel, want := range map[string]string{"backend.env": "DATABASE_URL=postgres://target/db\n", "retail/instance.env": "TOKEN=secret\n", "retail/metadata.json": "{\"slug\":\"retail\"}"} {
		got, err := os.ReadFile(filepath.Join(staged, rel))
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != want {
			t.Fatalf("%s mismatch", rel)
		}
	}
	if _, err := os.Stat(filepath.Join(staged, "database.sql")); !os.IsNotExist(err) {
		t.Fatalf("staging copied database dump: %v", err)
	}
	if _, err := StageGlobalConfig(archive, parent, false); err != nil {
		t.Fatal(err)
	}
}

func TestValidateRejectsUnsafePathAndInvalidScope(t *testing.T) {
	for name, manifest := range map[string]Manifest{
		"unsafe path":   {Format: 1, Scope: "instance-config", Instance: "retail", ConfigOnly: true},
		"invalid scope": {Format: 1, Scope: "tenant-db"},
	} {
		t.Run(name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "bad.zip")
			f, err := os.Create(p)
			if err != nil {
				t.Fatal(err)
			}
			zw := zip.NewWriter(f)
			mb, _ := json.Marshal(manifest)
			mw, _ := zw.Create(manifestName)
			_, _ = mw.Write(mb)
			if name == "unsafe path" {
				w, _ := zw.Create("../escape")
				_, _ = w.Write([]byte("x"))
			}
			_ = zw.Close()
			_ = f.Close()
			if _, err := Validate(p); err == nil {
				t.Fatal("expected validation failure")
			}
		})
	}
}

func TestValidateMalformedEntryReturnsErrorWithoutPanic(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, "malformed.zip")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	manifest := Manifest{Format: 1, Scope: "instance-config", Instance: "retail", ConfigOnly: true}
	mb, _ := json.Marshal(manifest)
	w, err := zw.Create(manifestName)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = w.Write(mb)
	w, err = zw.Create("retail/instance.env")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = w.Write([]byte("TOKEN=x"))
	w, err = zw.Create("retail/extra")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = w.Write([]byte("malformed payload"))
	if err = zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err = f.Close(); err != nil {
		t.Fatal(err)
	}
	archive, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	patchZipMethod(t, archive, "retail/extra", 0xffff)
	if err = os.WriteFile(p, archive, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = Validate(p); err == nil {
		t.Fatal("expected malformed compression method error")
	}
}

func patchZipMethod(t *testing.T, data []byte, filename string, method uint16) {
	t.Helper()
	foundLocal, foundCentral := false, false
	for i := 0; i+30 <= len(data); i++ {
		if binary.LittleEndian.Uint32(data[i:i+4]) == 0x04034b50 {
			nameLen := int(binary.LittleEndian.Uint16(data[i+26 : i+28]))
			extraLen := int(binary.LittleEndian.Uint16(data[i+28 : i+30]))
			end := i + 30 + nameLen + extraLen
			if end <= len(data) && string(data[i+30:i+30+nameLen]) == filename {
				binary.LittleEndian.PutUint16(data[i+8:i+10], method)
				foundLocal = true
			}
		} else if i+46 <= len(data) && binary.LittleEndian.Uint32(data[i:i+4]) == 0x02014b50 {
			nameLen := int(binary.LittleEndian.Uint16(data[i+28 : i+30]))
			extraLen := int(binary.LittleEndian.Uint16(data[i+30 : i+32]))
			commentLen := int(binary.LittleEndian.Uint16(data[i+32 : i+34]))
			end := i + 46 + nameLen + extraLen + commentLen
			if end <= len(data) && string(data[i+46:i+46+nameLen]) == filename {
				binary.LittleEndian.PutUint16(data[i+10:i+12], method)
				foundCentral = true
			}
		}
	}
	if !foundLocal || !foundCentral {
		t.Fatal("could not locate ZIP entry headers")
	}
}
