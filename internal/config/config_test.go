package config

import "testing"

func TestLoadLegacyGitHubVariables(t *testing.T) {
	t.Setenv("RULE_REPO_PROVIDER", "")
	t.Setenv("RULE_REPO_PROJECT", "")
	t.Setenv("RULE_REPO_BRANCH", "")
	t.Setenv("GITHUB_RULE_REPO_NAME", "legacy-rules")
	t.Setenv("GITHUB_BRANCH", "legacy")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RuleRepoProvider != "github" || cfg.RuleRepoProject != "legacy-rules" || cfg.RuleRepoBranch != "legacy" {
		t.Fatalf("unexpected config: %+v", cfg)
	}
}

func TestLoadDoHDefaults(t *testing.T) {
	t.Setenv("DOH_ENABLED", "")
	t.Setenv("DOH_API_URLS", "")
	t.Setenv("DOH_TIMEOUT", "")
	t.Setenv("DOH_CACHE_SIZE", "")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.DoHEnabled || len(cfg.DoHAPIURLs) != 2 || cfg.DoHTimeout.String() != "4s" || cfg.DoHCacheSize != 2048 {
		t.Fatalf("unexpected DoH defaults: %+v", cfg)
	}
	if !cfg.DomesticDNSEnabled || !cfg.ForeignDNSEnabled || len(cfg.DomesticDNSURLs) != 2 || len(cfg.ForeignDNSURLs) != 2 || cfg.DomesticDNSURLs[0] != "https://dns.alidns.com/resolve" {
		t.Fatalf("unexpected DNS group defaults: %+v", cfg)
	}
}

func TestLoadDNSGroupsOverrideLegacyForeign(t *testing.T) {
	t.Setenv("DNS_DOMESTIC_ENABLED", "true")
	t.Setenv("DNS_DOMESTIC_URLS", "https://domestic.example/resolve,https://backup.example/resolve")
	t.Setenv("DNS_FOREIGN_ENABLED", "true")
	t.Setenv("DNS_FOREIGN_URLS", "https://foreign.example/resolve,https://foreign-backup.example/resolve")
	t.Setenv("DNS_TIMEOUT", "3s")
	t.Setenv("DNS_CACHE_SIZE", "99")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DNSTimeout.String() != "3s" || cfg.DNSCacheSize != 99 || cfg.ForeignDNSURLs[0] != "https://foreign.example/resolve" || cfg.DomesticDNSURLs[1] != "https://backup.example/resolve" {
		t.Fatalf("unexpected DNS group override: %+v", cfg)
	}
}

func TestLoadRejectsInvalidDoHSettings(t *testing.T) {
	t.Setenv("DOH_TIMEOUT", "nope")
	if _, err := Load(); err == nil {
		t.Fatal("expected invalid DOH_TIMEOUT to fail")
	}
	t.Setenv("DOH_TIMEOUT", "4s")
	t.Setenv("DOH_CACHE_SIZE", "0")
	if _, err := Load(); err == nil {
		t.Fatal("expected invalid DOH_CACHE_SIZE to fail")
	}
}

func TestLoadGitLab(t *testing.T) {
	t.Setenv("RULE_REPO_PROVIDER", "gitlab")
	t.Setenv("RULE_REPO_PROJECT", "group/rules")
	t.Setenv("RULE_REPO_BRANCH", "main")
	t.Setenv("GITLAB_BASE_URL", "https://gitlab.example.com/")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.GitLabBaseURL != "https://gitlab.example.com" || cfg.RuleRepoProject != "group/rules" {
		t.Fatalf("unexpected config: %+v", cfg)
	}
}
