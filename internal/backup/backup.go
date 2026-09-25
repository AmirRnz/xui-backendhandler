// Package backup creates operator-managed backups for the xui-backend runtime.
// Global archives contain the complete PostgreSQL database and all instance
// configuration. Complete instance archives contain one deployment's logical
// database closure and runtime configuration; instance-config archives are
// available separately for configuration-only troubleshooting.
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
	"io"
	"net/url"
	"os"
	"os/exec"
	pathpkg "path"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const manifestName = "manifest.json"
const maxArchiveBytes = 2 << 30

type Manifest struct {
	Format      int            `json:"format"`
	Scope       string         `json:"scope"`
	Instance    string         `json:"instance,omitempty"`
	CreatedAt   time.Time      `json:"created_at"`
	Database    bool           `json:"database_included"`
	ConfigOnly  bool           `json:"configuration_only"`
	SQLSHA256   string         `json:"database_sha256,omitempty"`
	DataSHA256  string         `json:"instance_data_sha256,omitempty"`
	RowCounts   map[string]int `json:"instance_row_counts,omitempty"`
	ConfigFiles []string       `json:"configuration_files,omitempty"`
}

// CreateGlobal writes a complete database dump and all registered instance
// configuration into a mode-0600 archive. The caller should store archives in
// a separately protected backup location.
func CreateGlobal(ctx context.Context, databaseURL, instanceRoot, destination string) error {
	return create(ctx, databaseURL, instanceRoot, destination, "global", "")
}

// CreateInstanceConfig writes the selected instance's configuration only.
// It intentionally makes no claim to back up any PostgreSQL records.
func CreateInstanceConfig(instanceRoot, slug, destination string) error {
	if !validSlug(slug) {
		return errors.New("invalid instance slug")
	}
	root, err := filepath.Abs(instanceRoot)
	if err != nil {
		return err
	}
	dir := filepath.Join(root, slug)
	if err := ensureInside(root, dir); err != nil {
		return err
	}
	files, err := readConfigDir(dir)
	if err != nil {
		return err
	}
	return writeArchive(destination, Manifest{Format: 1, Scope: "instance-config", Instance: slug, CreatedAt: time.Now().UTC(), ConfigOnly: true, ConfigFiles: []string{"instance.env", "metadata.json"}}, nil, map[string][]byte{slug + "/instance.env": files["instance.env"], maybeMetadata(slug, files): files["metadata.json"]})
}

func maybeMetadata(slug string, files map[string][]byte) string {
	if _, ok := files["metadata.json"]; ok {
		return slug + "/metadata.json"
	}
	return ""
}

// Validate checks archive format, paths, checksums, and restore scope without
// invoking PostgreSQL or writing files.
func Validate(path string) (Manifest, error) {
	zr, err := zip.OpenReader(path)
	if err != nil {
		return Manifest{}, err
	}
	defer zr.Close()
	if len(zr.File) > 10000 {
		return Manifest{}, errors.New("backup contains too many files")
	}
	var m Manifest
	seen := map[string]bool{}
	var sql []byte
	var instanceData []byte
	for _, f := range zr.File {
		if f.UncompressedSize64 > maxArchiveBytes {
			return m, errors.New("backup entry exceeds size limit")
		}
		clean := pathpkg.Clean(f.Name)
		if strings.HasPrefix(f.Name, "/") || clean == ".." || strings.HasPrefix(clean, "../") || strings.Contains(f.Name, "\\") || (len(f.Name) > 2 && f.Name[1] == ':') {
			return m, errors.New("backup contains an unsafe path")
		}
		if seen[f.Name] {
			return m, errors.New("backup contains duplicate paths")
		}
		seen[f.Name] = true
		if f.Name == manifestName {
			b, e := readZip(f)
			if e != nil {
				return m, e
			}
			if e = json.Unmarshal(b, &m); e != nil {
				return m, errors.New("invalid backup manifest")
			}
		} else if f.Name == "database.sql" {
			sql, err = readZip(f)
			if err != nil {
				return m, err
			}
		} else if strings.HasSuffix(f.Name, "/database.json") {
			instanceData, err = readZip(f)
			if err != nil {
				return m, err
			}
		} else {
			if _, err = readZip(f); err != nil {
				return m, err
			}
		}
	}
	if (m.Format != 1 && m.Format != 2) || (m.Scope != "global" && m.Scope != "instance-config" && m.Scope != "instance") {
		return m, errors.New("unsupported backup format")
	}
	if m.Scope == "global" {
		if !m.Database || m.ConfigOnly || len(sql) == 0 {
			return m, errors.New("global backup is incomplete")
		}
		sum := sha256.Sum256(sql)
		if hex.EncodeToString(sum[:]) != m.SQLSHA256 {
			return m, errors.New("database dump checksum mismatch")
		}
	}
	if m.Scope == "instance-config" {
		if !m.ConfigOnly || m.Database || !validSlug(m.Instance) || !seen[m.Instance+"/instance.env"] {
			return m, errors.New("instance configuration backup is incomplete")
		}
		for name := range seen {
			if name != manifestName && name != m.Instance+"/instance.env" && name != m.Instance+"/metadata.json" {
				return m, errors.New("instance configuration backup contains unexpected files")
			}
		}
	}
	if m.Scope == "instance" {
		if m.Format != 2 || !m.Database || !validSlug(m.Instance) || !seen[m.Instance+"/instance.env"] || !seen[m.Instance+"/database.json"] || !seen[m.Instance+"/recovery.key"] {
			return m, errors.New("instance backup is incomplete")
		}
		for name := range seen {
			if name == manifestName {
				continue
			}
			parts := strings.Split(name, "/")
			if len(parts) != 2 || parts[0] != m.Instance || (parts[1] != "instance.env" && parts[1] != "metadata.json" && parts[1] != "database.json" && parts[1] != "recovery.key") {
				return m, errors.New("instance backup contains unexpected files")
			}
		}
		if len(instanceData) == 0 {
			return m, errors.New("instance data archive is missing")
		}
		sum := sha256.Sum256(instanceData)
		if hex.EncodeToString(sum[:]) != m.DataSHA256 {
			return m, errors.New("instance data checksum mismatch")
		}
	}
	if m.Scope == "global" {
		for name := range seen {
			if name == manifestName || name == "database.sql" || name == "backend.env" {
				continue
			}
			parts := strings.Split(name, "/")
			if len(parts) != 2 || !validSlug(parts[0]) || (parts[1] != "instance.env" && parts[1] != "metadata.json") {
				return m, errors.New("global backup contains unexpected configuration paths")
			}
		}
	}
	return m, nil
}

// StageGlobalConfig extracts the configuration bundled with a global archive
// into a new, inactive review directory. It never overwrites existing files or
// registers or starts instances. Operators must review and rebind backend.env
// before moving selected files into the live configuration root.
func StageGlobalConfig(archive, stagingParent string, dryRun bool) (string, error) {
	m, err := Validate(archive)
	if err != nil {
		return "", err
	}
	if m.Scope != "global" {
		return "", errors.New("archive is not a global backup")
	}
	if dryRun {
		return filepath.Join(stagingParent, "restore-review-<new>"), nil
	}
	if err = os.MkdirAll(stagingParent, 0700); err != nil {
		return "", err
	}
	dir, err := os.MkdirTemp(stagingParent, "restore-review-")
	if err != nil {
		return "", err
	}
	if err = os.Chmod(dir, 0700); err != nil {
		_ = os.RemoveAll(dir)
		return "", err
	}
	ok := false
	defer func() {
		if !ok {
			_ = os.RemoveAll(dir)
		}
	}()
	zr, err := zip.OpenReader(archive)
	if err != nil {
		return "", err
	}
	defer zr.Close()
	for _, f := range zr.File {
		if f.Name == manifestName || f.Name == "database.sql" {
			continue
		}
		b, e := readZip(f)
		if e != nil {
			return "", e
		}
		rel := filepath.FromSlash(f.Name)
		if e = ensureInside(dir, filepath.Join(dir, rel)); e != nil {
			return "", e
		}
		target := filepath.Join(dir, rel)
		if f.Name != "backend.env" {
			if e = os.MkdirAll(filepath.Dir(target), 0700); e != nil {
				return "", e
			}
		}
		if e = os.WriteFile(target, b, 0600); e != nil {
			return "", e
		}
	}
	ok = true
	return dir, nil
}

// StageInstanceConfig validates and extracts an instance config archive into
// an inactive review directory outside the runnable instance registry. It
// never creates a runnable service or overwrites an existing staging dir.
func StageInstanceConfig(path, stagingParent, newSlug string, dryRun bool) (string, error) {
	m, err := Validate(path)
	if err != nil {
		return "", err
	}
	if m.Scope != "instance-config" {
		return "", errors.New("archive is not an instance configuration backup")
	}
	if !validSlug(newSlug) {
		return "", errors.New("invalid target instance slug")
	}
	zr, err := zip.OpenReader(path)
	if err != nil {
		return "", err
	}
	defer zr.Close()
	if dryRun {
		return filepath.Join(stagingParent, "instance-config-review-<new>", newSlug), nil
	}
	if err = os.MkdirAll(stagingParent, 0700); err != nil {
		return "", err
	}
	stageRoot, err := os.MkdirTemp(stagingParent, "instance-config-review-")
	if err != nil {
		return "", err
	}
	if err = os.Chmod(stageRoot, 0700); err != nil {
		_ = os.RemoveAll(stageRoot)
		return "", err
	}
	target := filepath.Join(stageRoot, newSlug)
	if err = os.Mkdir(target, 0700); err != nil {
		_ = os.RemoveAll(stageRoot)
		return "", err
	}
	ok := false
	defer func() {
		if !ok {
			_ = os.RemoveAll(stageRoot)
		}
	}()
	for _, f := range zr.File {
		if !strings.HasPrefix(f.Name, m.Instance+"/") {
			continue
		}
		rel := strings.TrimPrefix(f.Name, m.Instance+"/")
		if rel != "instance.env" && rel != "metadata.json" {
			return "", errors.New("unexpected file in instance backup")
		}
		b, e := readZip(f)
		if e != nil {
			return "", e
		}
		if e = os.WriteFile(filepath.Join(target, rel), b, 0600); e != nil {
			return "", e
		}
	}
	ok = true
	return target, nil
}

// RestoreGlobal validates the dump and restores only into an empty database.
// Dry-run verifies the archive and database connectivity, without mutation.
func RestoreGlobal(ctx context.Context, databaseURL, archive string, dryRun bool) error {
	m, err := Validate(archive)
	if err != nil {
		return err
	}
	if m.Scope != "global" {
		return errors.New("archive is not a global backup")
	}
	conn, env, err := pgEnvironment(databaseURL)
	if err != nil {
		return err
	}
	check := exec.CommandContext(ctx, "psql", conn, "-X", "-Atqc", "SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE c.relkind='r' AND n.nspname NOT IN ('pg_catalog','information_schema') AND n.nspname NOT LIKE 'pg_toast%'")
	check.Env = append(os.Environ(), env...)
	out, err := check.Output()
	if err != nil {
		return errors.New("could not inspect restore database")
	}
	if strings.TrimSpace(string(out)) != "0" {
		return errors.New("restore target database is not empty")
	}
	if dryRun {
		return nil
	}
	zr, err := zip.OpenReader(archive)
	if err != nil {
		return err
	}
	defer zr.Close()
	var dump []byte
	for _, f := range zr.File {
		if f.Name == "database.sql" {
			dump, err = readZip(f)
			break
		}
	}
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, "psql", conn, "-X", "-v", "ON_ERROR_STOP=1", "--single-transaction")
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdin = bytes.NewReader(dump)
	if err = cmd.Run(); err != nil {
		return errors.New("database restore failed; inspect PostgreSQL logs without printing credentials")
	}
	return nil
}

func create(ctx context.Context, dbURL, root, destination, scope, slug string) error {
	if scope != "global" {
		return errors.New("unsupported backup scope")
	}
	info, err := os.Stat(root)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return errors.New("instance registry is not a directory")
	}
	configs, err := readAllConfigs(root)
	if err != nil {
		return err
	}
	conn, env, err := pgEnvironment(dbURL)
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, "pg_dump", conn, "--no-owner", "--no-privileges", "--format=plain")
	cmd.Env = append(os.Environ(), env...)
	dump, err := cmd.Output()
	if err != nil {
		return errors.New("database backup failed")
	}
	sum := sha256.Sum256(dump)
	m := Manifest{Format: 1, Scope: "global", CreatedAt: time.Now().UTC(), Database: true, ConfigFiles: configs.names, SQLSHA256: hex.EncodeToString(sum[:])}
	return writeArchive(destination, m, dump, configs.data)
}

type configBundle struct {
	names []string
	data  map[string][]byte
}

func readAllConfigs(root string) (configBundle, error) {
	b := configBundle{data: map[string][]byte{}}
	// The backend environment carries the database URL and backend credentials,
	// so a complete global recovery archive must retain it with the instance
	// configuration. It remains protected by the archive's owner-only mode.
	backendEnv := filepath.Join(filepath.Dir(root), "backend.env")
	if info, err := os.Lstat(backendEnv); err == nil {
		if !info.Mode().IsRegular() || (runtime.GOOS != "windows" && info.Mode().Perm()&0007 != 0) {
			return b, errors.New("backend.env must be a regular file with no world permissions")
		}
		contents, err := os.ReadFile(backendEnv)
		if err != nil {
			return b, err
		}
		b.data["backend.env"] = contents
		b.names = append(b.names, "backend.env")
	} else if !os.IsNotExist(err) {
		return b, err
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return b, err
	}
	for _, e := range entries {
		if !e.IsDir() || !validSlug(e.Name()) {
			continue
		}
		files, err := readConfigDir(filepath.Join(root, e.Name()))
		if err != nil {
			return b, fmt.Errorf("instance %s: %w", e.Name(), err)
		}
		for name, content := range files {
			path := e.Name() + "/" + name
			b.data[path] = content
			b.names = append(b.names, path)
		}
	}
	return b, nil
}
func readConfigDir(dir string) (map[string][]byte, error) {
	out := map[string][]byte{}
	for _, name := range []string{"instance.env", "metadata.json"} {
		p := filepath.Join(dir, name)
		info, statErr := os.Lstat(p)
		if os.IsNotExist(statErr) && name == "metadata.json" {
			continue
		}
		if statErr != nil {
			return nil, statErr
		}
		if !info.Mode().IsRegular() || (runtime.GOOS != "windows" && info.Mode().Perm()&0007 != 0) {
			return nil, fmt.Errorf("%s must be a regular file with no world permissions", name)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}
		out[name] = b
	}
	if len(out) == 0 || out["instance.env"] == nil {
		return nil, errors.New("missing instance.env")
	}
	return out, nil
}
func writeArchive(destination string, m Manifest, dump []byte, files map[string][]byte) error {
	if _, err := os.Lstat(destination); err == nil {
		return errors.New("backup destination already exists")
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(destination), ".xui-backup-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err = tmp.Chmod(0600); err != nil {
		tmp.Close()
		return err
	}
	zw := zip.NewWriter(tmp)
	if dump != nil {
		if err = writeZip(zw, "database.sql", dump); err != nil {
			zw.Close()
			tmp.Close()
			return err
		}
	}
	for name, b := range files {
		if name == "" {
			continue
		}
		if err = writeZip(zw, name, b); err != nil {
			zw.Close()
			tmp.Close()
			return err
		}
	}
	mb, err := json.MarshalIndent(m, "", "  ")
	if err == nil {
		err = writeZip(zw, manifestName, mb)
	}
	if err == nil {
		err = zw.Close()
	} else {
		_ = zw.Close()
	}
	if err != nil {
		tmp.Close()
		return err
	}
	if err = tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, destination)
}
func writeZip(zw *zip.Writer, name string, b []byte) error {
	if name == "" {
		return nil
	}
	h := &zip.FileHeader{Name: name, Method: zip.Deflate}
	h.SetMode(0600)
	w, e := zw.CreateHeader(h)
	if e != nil {
		return e
	}
	_, e = w.Write(b)
	return e
}
func readZip(f *zip.File) ([]byte, error) {
	r, e := f.Open()
	if e != nil {
		return nil, e
	}
	defer r.Close()
	b, err := io.ReadAll(io.LimitReader(r, maxArchiveBytes+1))
	if err == nil && len(b) > maxArchiveBytes {
		return nil, errors.New("backup entry exceeds size limit")
	}
	return b, err
}
func validSlug(s string) bool {
	if s == "" || len(s) > 48 {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
			return false
		}
	}
	return true
}
func ensureInside(root, path string) error {
	r, e := filepath.Rel(root, path)
	if e != nil || r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) {
		return errors.New("path escapes configured root")
	}
	return nil
}
func pgEnvironment(raw string) (string, []string, error) {
	u, e := url.Parse(raw)
	if e != nil || u.Scheme != "postgres" && u.Scheme != "postgresql" {
		return "", nil, errors.New("invalid PostgreSQL URL")
	}
	db := strings.TrimPrefix(u.Path, "/")
	if db == "" {
		return "", nil, errors.New("database name is required")
	}
	host := u.Hostname()
	if host == "" {
		host = "localhost"
	}
	port := u.Port()
	if port == "" {
		port = "5432"
	}
	env := []string{"PGHOST=" + host, "PGPORT=" + port, "PGDATABASE=" + db}
	if u.User != nil {
		env = append(env, "PGUSER="+u.User.Username())
		if p, ok := u.User.Password(); ok {
			env = append(env, "PGPASSWORD="+p)
		}
	}
	if ssl := u.Query().Get("sslmode"); ssl != "" {
		env = append(env, "PGSSLMODE="+ssl)
	}
	return db, env, nil
}
