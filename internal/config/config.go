package config

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

type ClientCredential struct {
	DeploymentID string `json:"deployment_id"`
	Token        string `json:"token"`
}
type Config struct {
	DatabaseURL       string
	ListenAddr        string
	ClientCredentials []ClientCredential
	PanelTokens       map[string]string
	TelegramTokens    map[string]string
	WorkerInterval    time.Duration
}

func Load() (Config, error) {
	c := Config{DatabaseURL: strings.TrimSpace(os.Getenv("DATABASE_URL")), ListenAddr: strings.TrimSpace(os.Getenv("BACKEND_LISTEN_ADDR")), WorkerInterval: 5 * time.Second}
	if c.DatabaseURL == "" {
		return c, fmt.Errorf("DATABASE_URL is required")
	}
	if c.ListenAddr == "" {
		c.ListenAddr = "127.0.0.1:8088"
	}
	if raw := os.Getenv("BACKEND_CLIENTS_JSON"); raw != "" {
		if err := json.Unmarshal([]byte(raw), &c.ClientCredentials); err != nil {
			return c, fmt.Errorf("parse BACKEND_CLIENTS_JSON: %w", err)
		}
	}
	if raw := os.Getenv("XUI_PANEL_TOKENS_JSON"); raw != "" {
		if err := json.Unmarshal([]byte(raw), &c.PanelTokens); err != nil {
			return c, fmt.Errorf("parse XUI_PANEL_TOKENS_JSON: %w", err)
		}
	}
	if raw := os.Getenv("TELEGRAM_BOT_TOKENS_JSON"); raw != "" {
		if err := json.Unmarshal([]byte(raw), &c.TelegramTokens); err != nil {
			return c, fmt.Errorf("parse TELEGRAM_BOT_TOKENS_JSON: %w", err)
		}
	}
	if raw := os.Getenv("BACKEND_WORKER_INTERVAL"); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil || d < time.Second || d > time.Minute {
			return c, fmt.Errorf("BACKEND_WORKER_INTERVAL must be between 1s and 1m")
		}
		c.WorkerInterval = d
	}
	seen := map[string]bool{}
	for _, credential := range c.ClientCredentials {
		if credential.DeploymentID == "" || len(credential.Token) < 24 {
			return c, fmt.Errorf("each backend client credential needs a deployment ID and a token of at least 24 characters")
		}
		if seen[credential.DeploymentID] {
			return c, fmt.Errorf("duplicate backend client deployment credential")
		}
		seen[credential.DeploymentID] = true
	}
	return c, nil
}
