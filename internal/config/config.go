package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	TelegramToken string
	Allowlist     map[int64]bool

	RuleRepoProvider string
	RuleRepoProject  string
	RuleRepoBranch   string
	GitHubToken      string
	GitLabToken      string
	GitLabBaseURL    string

	UpstreamRepo     string
	UpstreamBranch   string
	ProxyPolicyGroup string
	SyncCron         string
	HealthAddr       string
	DataDir          string
	GeoIPAPIURL      string
	DoHEnabled       bool
	DoHAPIURLs       []string
	DoHTimeout       time.Duration
	DoHCacheSize     int
	SyncUpstream     bool
	UpstreamIndex    bool
	Location         *time.Location
}

func Load() (Config, error) {
	tz := getenv("TZ", "Asia/Shanghai")
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return Config{}, fmt.Errorf("load TZ %q: %w", tz, err)
	}
	allow := map[int64]bool{}
	for _, raw := range strings.Split(getenv("TELEGRAM_ALLOWLIST", "538031590"), ",") {
		if raw == "" {
			continue
		}
		id, e := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
		if e != nil {
			return Config{}, fmt.Errorf("invalid TELEGRAM_ALLOWLIST: %w", e)
		}
		allow[id] = true
	}
	provider := strings.ToLower(getenv("RULE_REPO_PROVIDER", "github"))
	if provider != "github" && provider != "gitlab" {
		return Config{}, fmt.Errorf("RULE_REPO_PROVIDER must be github or gitlab")
	}
	legacyRepo := getenv("GITHUB_RULE_REPO_NAME", "clash-rule-pilot-rules")
	legacyBranch := getenv("GITHUB_BRANCH", "main")
	dohTimeout, err := time.ParseDuration(getenv("DOH_TIMEOUT", "4s"))
	if err != nil || dohTimeout <= 0 {
		return Config{}, fmt.Errorf("invalid DOH_TIMEOUT %q", os.Getenv("DOH_TIMEOUT"))
	}
	dohCacheSize, err := strconv.Atoi(getenv("DOH_CACHE_SIZE", "2048"))
	if err != nil || dohCacheSize <= 0 {
		return Config{}, fmt.Errorf("DOH_CACHE_SIZE must be a positive integer")
	}
	var dohURLs []string
	for _, raw := range strings.Split(getenv("DOH_API_URLS", "https://cloudflare-dns.com/dns-query,https://dns.google/resolve"), ",") {
		if value := strings.TrimSpace(raw); value != "" {
			dohURLs = append(dohURLs, value)
		}
	}
	if getenvBool("DOH_ENABLED", true) && len(dohURLs) == 0 {
		return Config{}, fmt.Errorf("DOH_API_URLS must contain at least one endpoint when DOH_ENABLED=true")
	}
	return Config{
		TelegramToken: os.Getenv("TELEGRAM_BOT_TOKEN"), Allowlist: allow,
		RuleRepoProvider: provider, RuleRepoProject: getenv("RULE_REPO_PROJECT", legacyRepo), RuleRepoBranch: getenv("RULE_REPO_BRANCH", legacyBranch),
		GitHubToken: os.Getenv("GITHUB_TOKEN"), GitLabToken: os.Getenv("GITLAB_TOKEN"), GitLabBaseURL: strings.TrimRight(getenv("GITLAB_BASE_URL", "https://gitlab.com"), "/"),
		UpstreamRepo: getenv("UPSTREAM_REPO", "Aethersailor/Custom_OpenClash_Rules"), UpstreamBranch: getenv("UPSTREAM_BRANCH", "main"), ProxyPolicyGroup: getenv("PROXY_POLICY_GROUP", "🚀 手动选择"),
		SyncCron: getenv("SYNC_CRON", "0 3 * * *"), HealthAddr: getenv("HEALTH_ADDR", ":8080"), DataDir: getenv("DATA_DIR", "/app/data"), GeoIPAPIURL: getenv("GEOIP_API_URL", "https://ipwho.is/{ip}"),
		DoHEnabled: getenvBool("DOH_ENABLED", true), DoHAPIURLs: dohURLs, DoHTimeout: dohTimeout, DoHCacheSize: dohCacheSize,
		SyncUpstream: getenvBool("SYNC_UPSTREAM", false), UpstreamIndex: getenvBool("UPSTREAM_INDEX_ENABLED", true), Location: loc,
	}, nil
}

func getenv(k, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v
	}
	return fallback
}

func getenvBool(k string, fallback bool) bool {
	v := strings.TrimSpace(strings.ToLower(os.Getenv(k)))
	if v == "" {
		return fallback
	}
	return v == "1" || v == "true" || v == "yes" || v == "on"
}
