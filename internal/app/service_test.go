package app

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"clashrulepilot/internal/config"
	"clashrulepilot/internal/rules"
	"github.com/goccy/go-yaml"
)

type testRepository struct {
	file []byte
	err  error
}

func (r *testRepository) EnsureRepo(context.Context) error { return nil }
func (r *testRepository) GetFile(context.Context, string) ([]byte, string, error) {
	if r.err != nil {
		return nil, "", r.err
	}
	return append([]byte(nil), r.file...), "sha", nil
}
func (r *testRepository) CommitFiles(context.Context, map[string][]byte, string) (string, error) {
	return "commit", nil
}
func (r *testRepository) Owner() string        { return "owner" }
func (r *testRepository) Repo() string         { return "repo" }
func (r *testRepository) Branch() string       { return "main" }
func (r *testRepository) WebURL() string       { return "https://example.test/owner/repo" }
func (r *testRepository) RawURL(string) string { return "https://example.test/raw" }

func TestPersonalOverwriteUsesExplicitOrderedRules(t *testing.T) {
	s := &Service{cfg: config.Config{ProxyPolicyGroup: "Proxy"}}
	store := rules.Store{Version: 1, Rules: []rules.Rule{
		{Domain: "example.com", Match: rules.Suffix, Action: rules.Proxy},
		{Domain: "cdn.example.com", Match: rules.Exact, Action: rules.Direct},
	}}
	out := s.personalOverwrite(store)
	if strings.Contains(out, "rule-providers:") || strings.Contains(out, "RULE-SET") {
		t.Fatalf("personal overwrite must use explicit rules:\n%s", out)
	}
	exact := strings.Index(out, "DOMAIN,cdn.example.com,DIRECT")
	root := strings.Index(out, "DOMAIN-SUFFIX,example.com,Proxy")
	if exact < 0 || root < 0 || exact > root {
		t.Fatalf("specific exception must be first:\n%s", out)
	}
	var document map[string]any
	if err := yaml.Unmarshal([]byte(strings.TrimPrefix(out, "[YAML]\n")), &document); err != nil {
		t.Fatalf("generated overwrite is not valid YAML: %v\n%s", err, out)
	}
}

func TestLoadStoreUsesDiskCacheAfterRemoteFailure(t *testing.T) {
	store := rules.Store{Version: 1, Rules: []rules.Rule{{Domain: "example.com", Match: rules.Exact, Action: rules.Direct}}}
	encoded, err := rules.Encode(store)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	repo := &testRepository{file: encoded}
	s := &Service{cfg: config.Config{DataDir: filepath.Join(dir, "data")}, repo: repo}
	got, err := s.LoadStore(context.Background())
	if err != nil || len(got.Rules) != 1 {
		t.Fatalf("initial LoadStore got=%+v err=%v", got, err)
	}

	reloaded := &Service{cfg: config.Config{DataDir: filepath.Join(dir, "data")}, repo: &testRepository{err: errors.New("gitlab EOF")}}
	got, err = reloaded.LoadStore(context.Background())
	if err != nil || len(got.Rules) != 1 || got.Rules[0].Domain != "example.com" {
		t.Fatalf("cached LoadStore got=%+v err=%v", got, err)
	}
}
