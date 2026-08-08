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
