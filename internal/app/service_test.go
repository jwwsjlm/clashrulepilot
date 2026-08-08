package app

import (
	"strings"
	"testing"

	"clashrulepilot/internal/config"
	"clashrulepilot/internal/rules"
	"github.com/goccy/go-yaml"
)

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
