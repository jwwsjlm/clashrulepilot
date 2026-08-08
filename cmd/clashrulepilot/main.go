package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

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
	log.Printf("starting ClashRulePilot provider=%s repo=%s branch=%s sync_upstream=%t upstream_index=%t doh=%t health=%s telegram_enabled=%t", cfg.RuleRepoProvider, cfg.RuleRepoProject, cfg.RuleRepoBranch, cfg.SyncUpstream, cfg.UpstreamIndex, cfg.DoHEnabled, cfg.HealthAddr, cfg.TelegramToken != "")
	service, err := app.New(cfg)
	if err != nil {
		log.Fatal(err)
	}
	defer service.Close()
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	log.Printf("github bootstrap started")
	if err := service.Bootstrap(ctx); err != nil {
		log.Fatal(err)
	}
	log.Printf("github bootstrap complete; service ready")
	go serveHealth(ctx, cfg.HealthAddr, service)
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

	c := cron.New(cron.WithLocation(cfg.Location))
	if service.SyncEnabled() || service.IndexEnabled() {
		if _, err := c.AddFunc(cfg.SyncCron, func() {
			if result, err := service.Sync(context.Background()); err != nil {
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
			if result, err := service.Sync(ctx); err != nil {
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

func serveHealth(ctx context.Context, addr string, service *app.Service) {
	log.Printf("health endpoint listening on %s/healthz", addr)
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if service.Ready() {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("ok\n"))
			return
		}
		http.Error(w, "not ready", http.StatusServiceUnavailable)
	})
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { <-ctx.Done(); _ = srv.Shutdown(context.Background()) }()
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Printf("health server: %v", err)
	}
}

func init() {
	if os.Getenv("TZ") == "" {
		_ = os.Setenv("TZ", "Asia/Shanghai")
	}
}
