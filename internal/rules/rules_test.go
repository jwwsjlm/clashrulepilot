package rules

import (
	"strings"
	"testing"

	"github.com/goccy/go-yaml"
)

func TestRender(t *testing.T) {
	s := Store{Version: 1, Rules: []Rule{{Domain: "example.com", Match: Suffix, Action: Proxy}}}
	out := string(Render(s, Proxy, "🚀 手动选择"))
	if !contains(out, "DOMAIN-SUFFIX,example.com") {
		t.Fatal(out)
	}
}

func TestRenderClassicalIncludesAllModernDomainTypes(t *testing.T) {
	s := Store{Version: 1, Rules: []Rule{
		{Domain: "api.example.com", Match: Exact, Action: Proxy},
		{Domain: "example.com", Match: Suffix, Action: Proxy},
		{Domain: "example", Match: Keyword, Action: Proxy},
		{Domain: "*.example.net", Match: Wildcard, Action: Proxy},
		{Domain: `^api[0-9]+\.example\.org$`, Match: Regex, Action: Proxy},
		{Domain: "direct.example", Match: Suffix, Action: Direct},
	}}
	out := RenderClassical(s, Proxy)
	for _, want := range []string{
		"DOMAIN,api.example.com",
		"DOMAIN-SUFFIX,example.com",
		"DOMAIN-KEYWORD,example",
		"DOMAIN-WILDCARD,*.example.net",
		`DOMAIN-REGEX,^api[0-9]+\.example\.org$`,
	} {
		if !strings.Contains(string(out), want) {
			t.Fatalf("classical provider missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(string(out), "direct.example") {
		t.Fatalf("provider contains rule from another action:\n%s", out)
	}
	var document map[string]any
	if err := yaml.Unmarshal(out, &document); err != nil {
		t.Fatalf("classical provider is not YAML: %v\n%s", err, out)
	}
	if payload, ok := document["payload"].([]any); !ok || len(payload) != 5 {
		t.Fatalf("unexpected payload: %#v", document["payload"])
	}
}

func TestRenderClassicalEmptyPayloadIsArray(t *testing.T) {
	out := RenderClassical(Empty(), Direct)
	var document map[string]any
	if err := yaml.Unmarshal(out, &document); err != nil {
		t.Fatalf("empty provider is not YAML: %v\n%s", err, out)
	}
	if payload, ok := document["payload"].([]any); !ok || len(payload) != 0 {
		t.Fatalf("empty provider payload must be an array: %#v", document["payload"])
	}
}

func TestRenderACLIsPlainTextAndActionScoped(t *testing.T) {
	store := Store{Version: 1, Rules: []Rule{
		{Domain: "api.example.com", Match: Exact, Action: Direct},
		{Domain: "example.com", Match: Suffix, Action: Direct},
		{Domain: "proxy.example", Match: Suffix, Action: Proxy},
	}}
	out := string(RenderACL(store, Direct))
	if strings.Contains(out, "payload:") || strings.Contains(out, "proxy.example") {
		t.Fatalf("ACL must be plain text and action-scoped:\n%s", out)
	}
	exact := strings.Index(out, "DOMAIN,api.example.com")
	suffix := strings.Index(out, "DOMAIN-SUFFIX,example.com")
	if exact < 0 || suffix < 0 || exact > suffix {
		t.Fatalf("ACL precedence is incorrect:\n%s", out)
	}
}

func TestAllMatchTypes(t *testing.T) {
	cases := []struct {
		rule Rule
		yes  string
		no   string
	}{
		{Rule{Domain: "cdn.example.com", Match: Exact}, "cdn.example.com", "x.cdn.example.com"},
		{Rule{Domain: "cdn.example.com", Match: Suffix}, "x.cdn.example.com", "api.example.com"},
		{Rule{Domain: "oops", Match: Keyword}, "gw.oops.asia", "example.com"},
		{Rule{Domain: "*.oops.asia", Match: Wildcard}, "gw.oops.asia", "oops.asia"},
		{Rule{Domain: `^gw[0-9]+\.oops\.asia$`, Match: Regex}, "gw2.oops.asia", "api.oops.asia"},
	}
	for _, tc := range cases {
		if !Matches(tc.rule, tc.yes) || Matches(tc.rule, tc.no) {
			t.Fatalf("unexpected match behavior for %+v", tc.rule)
		}
	}
}

func TestRenderExplicitSpecificRulesFirst(t *testing.T) {
	s := Store{Version: 1, Rules: []Rule{
		{Domain: "example.com", Match: Suffix, Action: Proxy},
		{Domain: "cdn.example.com", Match: Suffix, Action: Direct},
		{Domain: "api.cdn.example.com", Match: Exact, Action: Proxy},
		{Domain: "*example*", Match: Wildcard, Action: Proxy},
	}}
	out := string(RenderExplicit(s, "🚀 手动选择"))
	exact := strings.Index(out, "DOMAIN,api.cdn.example.com")
	child := strings.Index(out, "DOMAIN-SUFFIX,cdn.example.com")
	root := strings.Index(out, "DOMAIN-SUFFIX,example.com")
	wild := strings.Index(out, "DOMAIN-WILDCARD,*example*")
	if exact < 0 || !(exact < child && child < root && root < wild) {
		t.Fatalf("unexpected precedence:\n%s", out)
	}
}

func TestValidateAdvancedPatterns(t *testing.T) {
	for match, value := range map[Match]string{Keyword: "oops", Wildcard: "*.oops.asia", Regex: `(^|\.)oops\.asia$`} {
		if _, err := ValidatePattern(match, value); err != nil {
			t.Fatalf("%s: %v", match, err)
		}
	}
	if _, err := ValidatePattern(Regex, "["); err == nil {
		t.Fatal("invalid regex accepted")
	}
}
func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(s) > len(sub) && (string([]byte(s)[:len(sub)]) == sub || contains(s[1:], sub)))
}
