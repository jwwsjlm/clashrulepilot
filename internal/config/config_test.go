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
