package domain

import (
	"fmt"
	"net"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"unicode"

	"golang.org/x/net/idna"
	"golang.org/x/net/publicsuffix"
)

var (
	label       = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)
	arrowTarget = regexp.MustCompile(`--?>\s*([^\s]+)`)
	urlToken    = regexp.MustCompile(`(?i)https?://[^\s<>"']+`)
)

type AmbiguousError struct{ Candidates []string }

func (e *AmbiguousError) Error() string { return "multiple domains found" }

// Normalize accepts a domain, URL, host:port, or a log line containing one domain.
func Normalize(input string) (string, error) {
	candidates, err := Extract(input)
	if err != nil {
		return "", err
	}
	if len(candidates) != 1 {
		return "", &AmbiguousError{Candidates: candidates}
	}
	return candidates[0], nil
}

// Extract returns normalized candidate domains. A Mihomo/OpenClash arrow target
// has priority because the source side of the log is normally a LAN IP.
func Extract(input string) ([]string, error) {
	s := strings.TrimSpace(input)
	if s == "" {
		return nil, fmt.Errorf("domain is empty")
	}
	if m := arrowTarget.FindStringSubmatch(s); len(m) == 2 {
		if host, err := normalizeToken(m[1]); err == nil {
			return []string{host}, nil
		}
	}

	seen := map[string]bool{}
	add := func(raw string) {
		if host, err := normalizeToken(raw); err == nil {
			seen[host] = true
		}
	}
	for _, raw := range urlToken.FindAllString(s, -1) {
		add(raw)
	}
	for _, raw := range strings.FieldsFunc(s, func(r rune) bool {
		return unicode.IsSpace(r) || strings.ContainsRune("，,;；|()[]{}<>\"'", r)
	}) {
		add(raw)
	}
	out := make([]string, 0, len(seen))
	for host := range seen {
		out = append(out, host)
	}
	sort.Strings(out)
	if len(out) == 0 {
		return nil, fmt.Errorf("no valid domain found")
	}
	return out, nil
}

func normalizeToken(raw string) (string, error) {
	s := strings.TrimSpace(strings.Trim(raw, "[](){}<>\"'`，。！？；;,"))
	if s == "" {
		return "", fmt.Errorf("empty token")
	}
	if strings.Contains(s, "://") {
		u, err := url.Parse(s)
		if err != nil || u.Hostname() == "" {
			return "", fmt.Errorf("invalid URL")
		}
		s = u.Hostname()
	} else {
		// Strip common log punctuation and everything after a URL path.
		if i := strings.IndexAny(s, "/?#"); i >= 0 {
			s = s[:i]
		}
		if host, port, err := net.SplitHostPort(s); err == nil && port != "" {
			s = host
		} else if i := strings.LastIndexByte(s, ':'); i > 0 && i < len(s)-1 && allDigits(s[i+1:]) {
			s = s[:i]
		}
	}
	s = strings.TrimSuffix(strings.Trim(s, "."), ".")
	if net.ParseIP(strings.Trim(s, "[]")) != nil {
		return "", fmt.Errorf("IP address is not a domain")
	}
	if strings.ContainsAny(s, "@ \\\t") {
		return "", fmt.Errorf("invalid domain token")
	}
	ascii, err := idna.Lookup.ToASCII(strings.ToLower(s))
	if err != nil {
		return "", fmt.Errorf("invalid IDN domain: %w", err)
	}
	if len(ascii) > 253 || !strings.Contains(ascii, ".") {
		return "", fmt.Errorf("invalid domain")
	}
	for _, p := range strings.Split(ascii, ".") {
		if !label.MatchString(p) {
			return "", fmt.Errorf("invalid domain label %q", p)
		}
	}
	return ascii, nil
}

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func Registrable(name string) string {
	root, err := publicsuffix.EffectiveTLDPlusOne(name)
	if err != nil {
		return ""
	}
	// Dynamic-DNS/private hosting suffixes (for example dpdns.org) are useful
	// to display at their service root in a human-facing Bot summary. The PSL
	// still performs validation; only the presentation root is shortened.
	if suffix, icann := publicsuffix.PublicSuffix(name); !icann && strings.Contains(suffix, ".") {
		parts := strings.Split(name, ".")
		if len(parts) >= 2 {
			return strings.Join(parts[len(parts)-2:], ".")
		}
	}
	return root
}
