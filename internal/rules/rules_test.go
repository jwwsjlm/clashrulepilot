package rules

import "testing"

func TestRender(t *testing.T) {
	s := Store{Version: 1, Rules: []Rule{{Domain: "example.com", Match: Suffix, Action: Proxy}}}
	out := string(Render(s, Proxy, "🚀 手动选择"))
	if !contains(out, "DOMAIN-SUFFIX,example.com") {
		t.Fatal(out)
	}
}
func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(s) > len(sub) && (string([]byte(s)[:len(sub)]) == sub || contains(s[1:], sub)))
}
