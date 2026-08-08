package rules

import (
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"
)

type Match string

const (
	Exact    Match = "exact"
	Suffix   Match = "suffix"
	Keyword  Match = "keyword"
	Wildcard Match = "wildcard"
	Regex    Match = "regex"
)

type Action string

const (
	Direct Action = "direct"
	Proxy  Action = "proxy"
)

type Rule struct {
	Domain    string    `json:"domain"`
	Match     Match     `json:"match"`
	Action    Action    `json:"action"`
	CreatedBy int64     `json:"created_by"`
	CreatedAt time.Time `json:"created_at"`
}
type Store struct {
	Version int    `json:"version"`
	Rules   []Rule `json:"rules"`
}

func Empty() Store { return Store{Version: 1, Rules: []Rule{}} }
func Decode(b []byte) (Store, error) {
	if len(strings.TrimSpace(string(b))) == 0 {
		return Empty(), nil
	}
	var s Store
	if err := json.Unmarshal(b, &s); err != nil {
		return Store{}, err
	}
	if s.Version == 0 {
		s.Version = 1
	}
	return s, nil
}
func Encode(s Store) ([]byte, error) {
	sort.Slice(s.Rules, func(i, j int) bool {
		a, b := s.Rules[i], s.Rules[j]
		if a.Domain != b.Domain {
			return a.Domain < b.Domain
		}
		if a.Match != b.Match {
			return a.Match < b.Match
		}
		return a.Action < b.Action
	})
	return json.MarshalIndent(s, "", "  ")
}
func (s Store) Find(domain string, match Match) (int, *Rule) {
	for i := range s.Rules {
		if s.Rules[i].Domain == domain && s.Rules[i].Match == match {
			return i, &s.Rules[i]
		}
	}
	return -1, nil
}
func (s *Store) Add(r Rule) (conflict *Rule, duplicate bool) {
	_, old := s.Find(r.Domain, r.Match)
	if old == nil {
		s.Rules = append(s.Rules, r)
		return nil, false
	}
	if old.Action == r.Action {
		return old, true
	}
	return old, false
}
func (s *Store) Move(r Rule) {
	i, _ := s.Find(r.Domain, r.Match)
	if i >= 0 {
		s.Rules[i] = r
	} else {
		s.Rules = append(s.Rules, r)
	}
}
func (s *Store) Remove(domain string, match *Match) int {
	n := 0
	out := s.Rules[:0]
	for _, r := range s.Rules {
		if r.Domain == domain && (match == nil || r.Match == *match) {
			n++
			continue
		}
		out = append(out, r)
	}
	s.Rules = out
	return n
}

func ValidatePattern(match Match, value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", fmt.Errorf("匹配内容不能为空")
	}
	if strings.ContainsAny(value, ",#\r\n") {
		return "", fmt.Errorf("匹配内容不能包含逗号、#号或换行")
	}
	switch match {
	case Exact, Suffix:
		return strings.ToLower(strings.TrimSuffix(value, ".")), nil
	case Keyword:
		value = strings.ToLower(value)
		if len([]rune(value)) < 2 || len(value) > 128 || strings.ContainsAny(value, " \t") {
			return "", fmt.Errorf("关键词长度应为 2-128 且不能包含空格")
		}
	case Wildcard:
		value = strings.ToLower(value)
		if len(value) > 253 || !strings.ContainsAny(value, "*?") {
			return "", fmt.Errorf("通配符规则必须包含 * 或 ? 且不能超过 253 字符")
		}
		if ok, _ := regexp.MatchString(`^[a-z0-9.*?-]+$`, value); !ok {
			return "", fmt.Errorf("通配符规则只能包含字母、数字、点、连字符、* 和 ?")
		}
	case Regex:
		if len(value) > 512 {
			return "", fmt.Errorf("正则表达式不能超过 512 字符")
		}
		if _, err := regexp.Compile(value); err != nil {
			return "", fmt.Errorf("正则表达式无效：%w", err)
		}
	default:
		return "", fmt.Errorf("不支持的匹配方式 %q", match)
	}
	return value, nil
}

func Matches(r Rule, domain string) bool {
	domain = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(domain), "."))
	switch r.Match {
	case Exact:
		return domain == r.Domain
	case Suffix:
		return domain == r.Domain || strings.HasSuffix(domain, "."+r.Domain)
	case Keyword:
		return strings.Contains(domain, strings.ToLower(r.Domain))
	case Wildcard:
		matched, _ := path.Match(strings.ToLower(r.Domain), domain)
		return matched
	case Regex:
		matched, _ := regexp.MatchString(r.Domain, domain)
		return matched
	default:
		return false
	}
}

func Ordered(input []Rule) []Rule {
	out := append([]Rule(nil), input...)
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		ra, rb := matchRank(a.Match), matchRank(b.Match)
		if ra != rb {
			return ra < rb
		}
		if a.Match == Suffix && b.Match == Suffix {
			la, lb := strings.Count(a.Domain, "."), strings.Count(b.Domain, ".")
			if la != lb {
				return la > lb
			}
			if len(a.Domain) != len(b.Domain) {
				return len(a.Domain) > len(b.Domain)
			}
		}
		if a.Domain != b.Domain {
			return a.Domain < b.Domain
		}
		if a.Match != b.Match {
			return a.Match < b.Match
		}
		return a.Action < b.Action
	})
	return out
}

func Related(input []Rule, candidate Rule) []Rule {
	var out []Rule
	for _, existing := range input {
		if existing.Domain == candidate.Domain && existing.Match == candidate.Match {
			continue
		}
		if existing.Action == candidate.Action {
			continue
		}
		if overlaps(existing, candidate) {
			out = append(out, existing)
		}
	}
	return Ordered(out)
}

func overlaps(a, b Rule) bool {
	if a.Match == Regex || b.Match == Regex || a.Match == Wildcard || b.Match == Wildcard || a.Match == Keyword || b.Match == Keyword {
		return Matches(a, b.Domain) || Matches(b, a.Domain)
	}
	return Matches(a, b.Domain) || Matches(b, a.Domain)
}

func Token(r Rule) string {
	kind := map[Match]string{Exact: "DOMAIN", Suffix: "DOMAIN-SUFFIX", Keyword: "DOMAIN-KEYWORD", Wildcard: "DOMAIN-WILDCARD", Regex: "DOMAIN-REGEX"}[r.Match]
	return kind + "," + r.Domain
}

func matchRank(match Match) int {
	switch match {
	case Exact:
		return 0
	case Suffix:
		return 1
	case Wildcard:
		return 2
	case Keyword:
		return 3
	case Regex:
		return 4
	default:
		return 5
	}
}

func Render(s Store, action Action, proxyGroup string) []byte {
	var rs []Rule
	for _, r := range s.Rules {
		if r.Action == action {
			rs = append(rs, r)
		}
	}
	rs = Ordered(rs)
	var b strings.Builder
	fmt.Fprintf(&b, "# Generated by ClashRulePilot. Action: %s\n# Do not edit; change data/personal_rules.json through the Bot.\npayload:\n", action)
	for _, r := range rs {
		fmt.Fprintf(&b, "  - '%s'\n", strings.ReplaceAll(Token(r), "'", "''"))
	}
	_ = proxyGroup
	return []byte(b.String())
}

func RenderExplicit(s Store, proxyGroup string) []byte {
	var b strings.Builder
	b.WriteString("+rules:\n")
	for _, r := range Ordered(s.Rules) {
		action := proxyGroup
		if r.Action == Direct {
			action = "DIRECT"
		}
		line := Token(r) + "," + action
		fmt.Fprintf(&b, "  - '%s'\n", strings.ReplaceAll(line, "'", "''"))
	}
	return []byte(b.String())
}
