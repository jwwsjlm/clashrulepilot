package main

import (
	"context"
	"log"
	"os/signal"
	"syscall"

	"clashrulepilot/internal/app"
	"clashrulepilot/internal/config"
	"clashrulepilot/internal/runtimeuser"
)

// clashrulepilot-sync performs one upstream synchronization and exits.
// It is intended for GitHub Actions and operator-run maintenance jobs.
func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatal(err)
	}
	cfg.SyncUpstream = true
	cfg.UpstreamIndex = false
	if _, err := runtimeuser.Prepare(cfg.DataDir); err != nil {
		log.Fatalf("prepare data directory: %v", err)
	}
	service, err := app.New(cfg)
	if err != nil {
		log.Fatal(err)
	}
	defer service.Close()
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := service.Bootstrap(ctx); err != nil {
		log.Fatal(err)
	}
	result, err := service.SyncWithSource(ctx, "github-actions")
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("sync complete private_commit=%s changed=%d index_changed=%t", result.Commit, result.Changed, result.IndexChanged)
}
