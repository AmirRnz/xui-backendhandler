package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"example.com/xui-commerce/backend/internal/api"
	"example.com/xui-commerce/backend/internal/config"
	"example.com/xui-commerce/backend/internal/notify"
	"example.com/xui-commerce/backend/internal/store"
	"example.com/xui-commerce/backend/internal/worker"
	"github.com/jackc/pgx/v5/pgxpool"
)

func main() {
	if err := run(); err != nil {
		slog.Error("backend stopped", "error", err)
		os.Exit(1)
	}
}

func run() error {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	command := "serve"
	if len(os.Args) > 1 {
		command = os.Args[1]
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("configure database pool: %w", err)
	}
	defer pool.Close()
	if err = pool.Ping(ctx); err != nil {
		return fmt.Errorf("database connection failed: %w", err)
	}
	repo := &store.Store{DB: pool}
	if command == "migrate" {
		if err = repo.Migrate(ctx); err != nil {
			return err
		}
		logger.Info("database migrations applied")
		return nil
	}
	if command != "serve" {
		return fmt.Errorf("usage: backend [migrate|serve]")
	}
	if len(cfg.ClientCredentials) == 0 {
		return fmt.Errorf("BACKEND_CLIENTS_JSON must configure at least one scoped bot identity")
	}
	rootCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	server := &http.Server{Addr: cfg.ListenAddr, Handler: api.New(repo, cfg, logger), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 20 * time.Second, IdleTimeout: 60 * time.Second}
	provision := &worker.Runner{Store: repo, Config: cfg, Logger: logger}
	dispatcher := &notify.Dispatcher{Store: repo, Config: cfg, Logger: logger}
	go provision.Run(rootCtx)
	go dispatcher.Run(rootCtx)
	serveErr := make(chan error, 1)
	go func() { logger.Info("backend listening", "addr", cfg.ListenAddr); serveErr <- server.ListenAndServe() }()
	select {
	case <-rootCtx.Done():
		shutdownCtx, done := context.WithTimeout(context.Background(), 10*time.Second)
		defer done()
		if err := server.Shutdown(shutdownCtx); err != nil {
			return err
		}
		return nil
	case err := <-serveErr:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("HTTP server failed: %w", err)
	}
}
