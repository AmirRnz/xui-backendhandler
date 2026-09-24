package main

import (
	"log/slog"
	"os"

	"example.com/xui-commerce/backend/internal/backendapp"
)

func main() {
	command := "serve"
	if len(os.Args) > 1 {
		command = os.Args[1]
	}
	if err := backendapp.Run(command); err != nil {
		slog.Error("backend stopped", "error", err)
		os.Exit(1)
	}
}
