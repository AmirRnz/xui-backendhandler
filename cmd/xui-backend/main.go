package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"example.com/xui-commerce/backend/internal/backendapp"
	"example.com/xui-commerce/backend/internal/botruntime/enduser"
	"example.com/xui-commerce/backend/internal/botruntime/reseller"
	"example.com/xui-commerce/backend/internal/instancecli"
)

func main() {
	if err := run(); err != nil {
		slog.Error("xui-backend command failed", "error", err)
		os.Exit(1)
	}
}

func run() error {
	args := os.Args[1:]
	if len(args) > 0 && (args[0] == "serve" || args[0] == "migrate") {
		if len(args) != 1 {
			return fmt.Errorf("usage: xui-backend [serve|migrate]")
		}
		return backendapp.Run(args[0])
	}
	if len(args) > 0 && args[0] == "run-instance" {
		if len(args) != 2 {
			return fmt.Errorf("usage: xui-backend run-instance <instance-slug>")
		}
		cfg, err := instancecli.Read(args[1])
		if err != nil {
			return err
		}
		if err := instancecli.ApplyEnvironment(cfg); err != nil {
			return err
		}
		switch cfg.Channel {
		case "retail":
			return enduser.Run()
		case "reseller":
			return reseller.Run()
		default:
			return fmt.Errorf("instance %q has an unsupported channel", cfg.Slug)
		}
	}
	if len(args) != 0 {
		return instancecli.Command(args)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return instancecli.Menu(ctx)
}
