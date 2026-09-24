package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"example.com/xui-commerce/backend/internal/importer"
	"github.com/jackc/pgx/v5/pgxpool"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "import-legacy:", err)
		os.Exit(1)
	}
}

func run() error {
	var sourceInstance, panelID string
	var apply bool
	flag.StringVar(&sourceInstance, "source-instance", "", "retail-finland, retail-germany, or reseller-turk1")
	flag.StringVar(&panelID, "target-panel-id", "", "reviewed target panel id; required for remote identity collision analysis")
	flag.BoolVar(&apply, "apply", false, "archive source rows to backend; default is dry-run")
	flag.Parse()
	legacyURL, targetURL := os.Getenv("LEGACY_DATABASE_URL"), os.Getenv("DATABASE_URL")
	if legacyURL == "" || targetURL == "" {
		return fmt.Errorf("set LEGACY_DATABASE_URL (read-only) and DATABASE_URL (backend)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	legacyConfig, err := pgxpool.ParseConfig(legacyURL)
	if err != nil {
		return fmt.Errorf("parse legacy database URL: %w", err)
	}
	if legacyConfig.ConnConfig.RuntimeParams == nil {
		legacyConfig.ConnConfig.RuntimeParams = map[string]string{}
	}
	legacyConfig.ConnConfig.RuntimeParams["default_transaction_read_only"] = "on"
	legacy, err := pgxpool.NewWithConfig(ctx, legacyConfig)
	if err != nil {
		return err
	}
	defer legacy.Close()
	target, err := pgxpool.New(ctx, targetURL)
	if err != nil {
		return err
	}
	defer target.Close()
	if err = legacy.Ping(ctx); err != nil {
		return fmt.Errorf("legacy database unavailable: %w", err)
	}
	if err = target.Ping(ctx); err != nil {
		return fmt.Errorf("backend database unavailable: %w", err)
	}
	report, err := importer.Run(ctx, legacy, target, importer.Options{SourceInstance: sourceInstance, PanelID: panelID, Apply: apply})
	if err != nil {
		return err
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(report)
}
