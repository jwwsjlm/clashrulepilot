package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"clashrulepilot/internal/app"
	"clashrulepilot/internal/bot"
	"clashrulepilot/internal/config"
	"clashrulepilot/internal/runtimeuser"
	"github.com/robfig/cron/v3"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatal(err)
	}
	runtimeInfo, err := runtimeuser.Prepare(cfg.DataDir)
	if err != nil && cfg.DataDir == "/data" {
		legacyErr := err
		const fallbackDataDir = "/app/data"
		runtimeInfo, err = runtimeuser.Prepare(fallbackDataDir)
		if err == nil {
			log.Printf("legacy DATA_DIR=/data unavailable (%v); automatically using %s", legacyErr, fallbackDataDir)
			cfg.DataDir = fallbackDataDir
		} else {
			err = fmt.Errorf("legacy /data failed: %v; fallback %s failed: %w", legacyErr, fallbackDataDir, err)
		}
	}
	if err != nil {
		log.Fatalf("prepare data directory %s: %v", cfg.DataDir, err)
	}
	if runtimeInfo.Warning != "" {
		log.Printf("data directory warning: %s", runtimeInfo.Warning)
	}
	log.Printf("data directory ready path=%s uid=%d gid=%d dropped_root=%t", cfg.DataDir, runtimeInfo.UID, runtimeInfo.GID, runtimeInfo.Dropped)
	log.Printf("starting ClashRulePilot provider=%s repo=%s branch=%s sync_upstream=%t upstream_index=%t doh=%t telegram_enabled=%t", cfg.RuleRepoProvider, cfg.RuleRepoProject, cfg.RuleRepoBranch, cfg.SyncUpstream, cfg.UpstreamIndex, cfg.DoHEnabled, cfg.TelegramToken != "")
	service, err := app.New(cfg)
	if err != nil {
		log.Fatal(err)
	}
	defer service.Close()
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	log.Printf("%s bootstrap started", cfg.RuleRepoProvider)
	if err := service.Bootstrap(ctx); err != nil {
		log.Fatal(err)
	}
	access := service.AccessStatus()
	mode := "read-write"
	if !access.Writable {
		mode = "read-only"
	}
	log.Printf("%s preflight authenticated=%t user=%s readable=%t writable=%t public=%t raw_accessible=%t mode=%s error=%q", cfg.RuleRepoProvider, access.Authenticated, access.User, access.Readable, access.Writable, access.Public, access.RawAccessible, mode, access.Error)
	log.Printf("%s bootstrap complete; service ready", cfg.RuleRepoProvider)
	if cfg.TelegramToken == "" {
		log.Printf("Telegram disabled: TELEGRAM_BOT_TOKEN is empty")
	} else {
		b, err := bot.New(service, cfg)
		if err != nil {
			log.Fatalf("initialize Telegram bot: %v", err)
		}
		log.Printf("starting Telegram polling")
		go b.Run(ctx)
	}
	service.StartWorkers(ctx)

	c := cron.New(cron.WithLocation(cfg.Location))
	if service.SyncEnabled() || service.IndexEnabled() {
		if _, err := c.AddFunc(cfg.SyncCron, func() {
			if result, err := service.SyncWithSource(ctx, "cron"); err != nil {
				log.Printf("scheduled sync failed: %v", err)
			} else {
				log.Printf("scheduled sync complete index_changed=%t commit=%s", result.IndexChanged, result.Commit)
			}
		}); err != nil {
			log.Fatal(err)
		}
	}
	c.Start()
	defer c.Stop()
	if service.SyncEnabled() || service.IndexEnabled() {
		go func() {
			log.Printf("initial upstream index sync started in background")
			if result, err := service.SyncWithSource(ctx, "startup"); err != nil {
				log.Printf("initial upstream sync failed: %v", err)
			} else if result.Commit != "" {
				log.Printf("initial upstream sync committed %s", result.Commit)
			} else {
				status := service.IndexStatus()
				log.Printf("initial upstream index sync complete changed=%t sources=%d direct=%d proxy=%d category=%d geosite_cn=%d geosite_gfw=%d", result.IndexChanged, status.Sources, status.Direct, status.Proxy, status.Category, status.GeoSite, status.GFW)
			}
		}()
	}
	<-ctx.Done()
}

func init() {
	if os.Getenv("TZ") == "" {
		_ = os.Setenv("TZ", "Asia/Shanghai")
	}
}
