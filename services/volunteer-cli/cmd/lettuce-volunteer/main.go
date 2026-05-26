package main

import (
	"log/slog"
	"os"

	"github.com/lettuce-compute/volunteer-cli/internal/cli"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	})))

	if err := cli.Execute(); err != nil {
		os.Exit(1)
	}
}
