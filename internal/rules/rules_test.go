package rules

import (
	"strings"
	"testing"
)

func TestRender(t *testing.T) {
	s := Store{Version: 1, Rules: []Rule{{Domain: "example.com", Match: Suffix, Action: Proxy}}}
	out := string(Render(s, Proxy, "🚀 手动选择"))
	if !contains(out, "DOMAIN-SUFFIX,example.com") {
		t.Fatal(out)
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
