package ruleindex

import (
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/goccy/go-yaml"
	gh "github.com/google/go-github/v81/github"
	"github.com/hashicorp/go-retryablehttp"
)

type Action string

const (
	Direct   Action = "direct"
	Proxy    Action = "proxy"
	Category Action = "category"
	GeoSite  Action = "geosite-cn"
)

type Entry struct {
	Kind    string `json:"kind"`
	Pattern string `json:"pattern"`
	Action  Action `json:"action"`
	Source  string `json:"source"`
}

type sourceMeta struct {
	RemoteSHA    string `json:"remote_sha,omitempty"`
	ETag         string `json:"etag,omitempty"`
	LastModified string `json:"last_modified,omitempty"`
	SHA256       string `json:"sha256"`
}

type diskSnapshot struct {
	Version   int                   `json:"version"`
	UpdatedAt time.Time             `json:"updated_at"`
	Entries   []Entry               `json:"entries"`
	Sources   map[string]sourceMeta `json:"sources"`
}

type Status struct {
	Enabled   bool
	Loaded    bool
	UpdatedAt time.Time
	Sources   int
	Direct    int
	Proxy     int
	Category  int
	GeoSite   int
	LastError string
}

type Match struct {
	Kind, Pattern, Source string
	Action                Action
}

type Manager struct {
	enabled      bool
	dataDir      string
	upstreamRepo string
	branch       string
	geositeURL   string
	http         *http.Client
	github       *gh.Client
	current      atomic.Pointer[compiled]
	mu           sync.Mutex
	statusMu     sync.RWMutex
	status       Status
}

type compiled struct {
	snapshot diskSnapshot
	exact    map[string][]Entry
	suffix   *trieNode
	keywords []Entry
}

type trieNode struct {
	children map[string]*trieNode
	entries  []Entry
}

type source struct {
	name, url, remoteSHA string
	action               Action
	domainBehavior       bool
}

func New(enabled bool, dataDir, repo, branch, token string) *Manager {
	retry := retryablehttp.NewClient()
	retry.RetryMax = 4
	retry.RetryWaitMin = 500 * time.Millisecond
	retry.RetryWaitMax = 5 * time.Second
	retry.Logger = nil
	httpClient := retry.StandardClient()
	httpClient.Timeout = 45 * time.Second
	githubClient := gh.NewClient(httpClient)
	if strings.TrimSpace(token) != "" {
		githubClient = githubClient.WithAuthToken(token)
	}
	m := &Manager{enabled: enabled, dataDir: dataDir, upstreamRepo: repo, branch: branch, geositeURL: "https://raw.githubusercontent.com/MetaCubeX/meta-rules-dat/meta/geo/geosite/classical/cn.yaml", http: httpClient, github: githubClient}
	m.status.Enabled = enabled
	return m
}

func (m *Manager) Load() error {
	if !m.enabled {
		return nil
	}
	m.cleanupTemporarySnapshots()
	f, err := os.Open(m.snapshotPath())
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer zr.Close()
	var snap diskSnapshot
	if err := json.NewDecoder(io.LimitReader(zr, 64<<20)).Decode(&snap); err != nil {
		return err
	}
	m.install(snap)
	return nil
}

func (m *Manager) Sync(ctx context.Context) (bool, error) {
	if !m.enabled {
		return false, nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	old := diskSnapshot{Version: 2, Sources: map[string]sourceMeta{}}
	if c := m.current.Load(); c != nil {
		old = c.snapshot
	}
	oldEntries := make(map[string][]Entry, len(old.Sources))
	for _, e := range old.Entries {
		oldEntries[e.Source] = append(oldEntries[e.Source], e)
	}

	sources, err := m.discoverSources(ctx)
	if err != nil {
		m.setError(err)
		return false, err
	}
	entries := make([]Entry, 0, len(old.Entries))
	metas := make(map[string]sourceMeta, len(sources))
	for _, src := range sources {
		oldMeta := old.Sources[src.name]
		if src.remoteSHA != "" && src.remoteSHA == oldMeta.RemoteSHA && len(oldEntries[src.name]) > 0 {
			entries = append(entries, oldEntries[src.name]...)
			metas[src.name] = oldMeta
			continue
		}
		body, meta, notModified, fetchErr := m.fetch(ctx, src.url, oldMeta)
		if fetchErr != nil {
			err = fmt.Errorf("sync %s: %w", src.name, fetchErr)
			m.setError(err)
			return false, err
		}
		if notModified {
			if len(oldEntries[src.name]) == 0 {
				err = fmt.Errorf("sync %s: server returned not modified but no local entries exist", src.name)
				m.setError(err)
				return false, err
			}
			entries = append(entries, oldEntries[src.name]...)
			metas[src.name] = oldMeta
			continue
		}
		meta.RemoteSHA = src.remoteSHA
		var parsed []Entry
		if src.domainBehavior {
			parsed, err = parseDomainBehavior(body, src.action, src.name)
		} else {
			parsed, err = parseClassical(body, src.action, src.name)
		}
		if err != nil {
			err = fmt.Errorf("parse %s: %w", src.name, err)
			m.setError(err)
			return false, err
		}
		entries = append(entries, parsed...)
		metas[src.name] = meta
	}
	if len(entries) == 0 {
		err = fmt.Errorf("no upstream domain index data available")
		m.setError(err)
		return false, err
	}
	sortEntries(entries)
	if m.current.Load() != nil && reflect.DeepEqual(old.Entries, entries) && reflect.DeepEqual(old.Sources, metas) {
		m.setError(nil)
		return false, nil
	}
	snap := diskSnapshot{Version: 2, UpdatedAt: time.Now().UTC(), Entries: entries, Sources: metas}
	if err := m.persist(snap); err != nil {
		m.setError(err)
		return false, err
	}
	m.install(snap)
	m.setError(nil)
	return true, nil
}

func (m *Manager) Query(domain string) []Match {
	c := m.current.Load()
	if c == nil {
		return nil
	}
	domain = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(domain), "."))
	seen := map[string]bool{}
	var out []Match
	add := func(e Entry) {
		key := string(e.Action) + "\x00" + e.Kind + "\x00" + e.Pattern + "\x00" + e.Source
		if seen[key] {
			return
		}
		seen[key] = true
		out = append(out, Match{Kind: e.Kind, Pattern: e.Pattern, Action: e.Action, Source: e.Source})
	}
	for _, e := range c.exact[domain] {
		add(e)
	}
	node := c.suffix
	labels := strings.Split(domain, ".")
	for i := len(labels) - 1; i >= 0 && node != nil; i-- {
		node = node.children[labels[i]]
		if node != nil {
			for _, e := range node.entries {
				add(e)
			}
		}
	}
	for _, e := range c.keywords {
		if strings.Contains(domain, e.Pattern) {
			add(e)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		priority := func(k string) int {
			switch k {
			case "DOMAIN":
				return 0
			case "DOMAIN-SUFFIX":
				return 1
			default:
				return 2
			}
		}
		if priority(out[i].Kind) != priority(out[j].Kind) {
			return priority(out[i].Kind) < priority(out[j].Kind)
		}
		if len(out[i].Pattern) != len(out[j].Pattern) {
			return len(out[i].Pattern) > len(out[j].Pattern)
		}
		return out[i].Source < out[j].Source
	})
	return out
}

func (m *Manager) Status() Status {
	m.statusMu.RLock()
	defer m.statusMu.RUnlock()
	return m.status
}

func (m *Manager) discoverSources(ctx context.Context) ([]source, error) {
	parts := strings.SplitN(strings.Trim(m.upstreamRepo, "/"), "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return nil, fmt.Errorf("UPSTREAM_REPO must be owner/repository")
	}
	_, contents, _, err := m.github.Repositories.GetContents(ctx, parts[0], parts[1], "rule", &gh.RepositoryContentGetOptions{Ref: m.branch})
	if err != nil {
		return nil, fmt.Errorf("list upstream rule directory: %w", err)
	}
	var sources []source
	for _, item := range contents {
		name := item.GetName()
		if item.GetType() != "file" || !strings.HasSuffix(strings.ToLower(name), "_domain.yaml") {
			continue
		}
		action := Category
		switch strings.ToLower(name) {
		case "custom_direct_domain.yaml":
			action = Direct
		case "custom_proxy_domain.yaml":
			action = Proxy
		}
		if item.GetDownloadURL() == "" {
			return nil, fmt.Errorf("upstream file %s has no download URL", name)
		}
		sources = append(sources, source{name: name, url: item.GetDownloadURL(), remoteSHA: item.GetSHA(), action: action, domainBehavior: true})
	}
	if len(sources) == 0 {
		return nil, fmt.Errorf("no *_Domain.yaml files found in upstream rule directory")
	}
	sources = append(sources, source{
		name: "GEOSITE_CN.yaml", url: m.geositeURL, action: GeoSite,
	})
	sort.Slice(sources, func(i, j int) bool { return sources[i].name < sources[j].name })
	return sources, nil
}

func (m *Manager) fetch(ctx context.Context, endpoint string, old sourceMeta) ([]byte, sourceMeta, bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, sourceMeta{}, false, err
	}
	req.Header.Set("User-Agent", "ClashRulePilot/1.0")
	if old.ETag != "" {
		req.Header.Set("If-None-Match", old.ETag)
	}
	if old.LastModified != "" {
		req.Header.Set("If-Modified-Since", old.LastModified)
	}
	resp, err := m.http.Do(req)
	if err != nil {
		return nil, sourceMeta{}, false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotModified {
		return nil, old, true, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return nil, sourceMeta{}, false, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 24<<20))
	if err != nil {
		return nil, sourceMeta{}, false, err
	}
	sum := sha256.Sum256(b)
	return b, sourceMeta{ETag: resp.Header.Get("ETag"), LastModified: resp.Header.Get("Last-Modified"), SHA256: hex.EncodeToString(sum[:])}, false, nil
}

func parseClassical(data []byte, action Action, source string) ([]Entry, error) {
	var document struct {
		Payload []string `yaml:"payload"`
	}
	if err := yaml.Unmarshal(data, &document); err != nil {
		return nil, fmt.Errorf("parse YAML: %w", err)
	}
	seen := map[string]bool{}
	var out []Entry
	for _, line := range document.Payload {
		parts := strings.Split(strings.TrimSpace(line), ",")
		if len(parts) < 2 {
			continue
		}
		kind := strings.ToUpper(strings.TrimSpace(parts[0]))
		if kind != "DOMAIN" && kind != "DOMAIN-SUFFIX" && kind != "DOMAIN-KEYWORD" {
			continue
		}
		pattern := cleanPattern(parts[1])
		if pattern == "" {
			continue
		}
		appendUnique(&out, seen, Entry{Kind: kind, Pattern: pattern, Action: action, Source: source})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no supported domain rules found")
	}
	return out, nil
}

func parseDomainBehavior(data []byte, action Action, source string) ([]Entry, error) {
	var document struct {
		Payload []string `yaml:"payload"`
	}
	if err := yaml.Unmarshal(data, &document); err != nil {
		return nil, fmt.Errorf("parse YAML: %w", err)
	}
	seen := map[string]bool{}
	var out []Entry
	for _, raw := range document.Payload {
		value := strings.ToLower(strings.TrimSpace(raw))
		if value == "" {
			continue
		}
		kind := "DOMAIN"
		pattern := value
		switch {
		case strings.HasPrefix(value, "+.") || strings.HasPrefix(value, "."):
			kind = "DOMAIN-SUFFIX"
			pattern = strings.TrimPrefix(strings.TrimPrefix(value, "+."), ".")
		case strings.Contains(value, "*"):
			kind = "DOMAIN-KEYWORD"
			pattern = strings.ReplaceAll(value, "*", "")
		}
		pattern = cleanPattern(pattern)
		if pattern == "" {
			continue
		}
		appendUnique(&out, seen, Entry{Kind: kind, Pattern: pattern, Action: action, Source: source})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no supported domain rules found")
	}
	return out, nil
}

func cleanPattern(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	value = strings.TrimPrefix(value, "+.")
	value = strings.TrimPrefix(value, ".")
	return strings.TrimSuffix(value, ".")
}

func appendUnique(out *[]Entry, seen map[string]bool, entry Entry) {
	key := entry.Kind + "\x00" + entry.Pattern
	if seen[key] {
		return
	}
	seen[key] = true
	*out = append(*out, entry)
}

func sortEntries(entries []Entry) {
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Source != entries[j].Source {
			return entries[i].Source < entries[j].Source
		}
		if entries[i].Kind != entries[j].Kind {
			return entries[i].Kind < entries[j].Kind
		}
		return entries[i].Pattern < entries[j].Pattern
	})
}

func compile(s diskSnapshot) *compiled {
	c := &compiled{snapshot: s, exact: map[string][]Entry{}, suffix: &trieNode{children: map[string]*trieNode{}}}
	for _, e := range s.Entries {
		switch e.Kind {
		case "DOMAIN":
			c.exact[e.Pattern] = append(c.exact[e.Pattern], e)
		case "DOMAIN-SUFFIX":
			node := c.suffix
			parts := strings.Split(e.Pattern, ".")
			for i := len(parts) - 1; i >= 0; i-- {
				if node.children[parts[i]] == nil {
					node.children[parts[i]] = &trieNode{children: map[string]*trieNode{}}
				}
				node = node.children[parts[i]]
			}
			node.entries = append(node.entries, e)
		case "DOMAIN-KEYWORD":
			c.keywords = append(c.keywords, e)
		}
	}
	return c
}

func (m *Manager) install(s diskSnapshot) {
	m.current.Store(compile(s))
	status := Status{Enabled: m.enabled, Loaded: true, UpdatedAt: s.UpdatedAt, Sources: len(s.Sources)}
	for _, e := range s.Entries {
		switch e.Action {
		case Direct:
			status.Direct++
		case Proxy:
			status.Proxy++
		case Category:
			status.Category++
		case GeoSite:
			status.GeoSite++
		}
	}
	m.statusMu.Lock()
	status.LastError = m.status.LastError
	m.status = status
	m.statusMu.Unlock()
}

func (m *Manager) persist(s diskSnapshot) error {
	if err := os.MkdirAll(m.dataDir, 0o755); err != nil {
		return err
	}
	m.cleanupTemporarySnapshots()
	tmp := m.snapshotPath() + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	z := gzip.NewWriter(f)
	err = json.NewEncoder(z).Encode(s)
	if closeErr := z.Close(); err == nil {
		err = closeErr
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, m.snapshotPath()); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	m.cleanupTemporarySnapshots()
	return nil
}

func (m *Manager) cleanupTemporarySnapshots() {
	if strings.TrimSpace(m.dataDir) == "" {
		return
	}
	patterns := []string{
		filepath.Join(m.dataDir, "upstream-index.json.gz.tmp"),
		filepath.Join(m.dataDir, "upstream-index-*.json.gz"),
	}
	for _, pattern := range patterns {
		matches, _ := filepath.Glob(pattern)
		for _, name := range matches {
			_ = os.Remove(name)
		}
	}
}

func (m *Manager) snapshotPath() string { return filepath.Join(m.dataDir, "upstream-index.json.gz") }

func (m *Manager) setError(err error) {
	m.statusMu.Lock()
	defer m.statusMu.Unlock()
	if err == nil {
		m.status.LastError = ""
	} else {
		m.status.LastError = err.Error()
	}
}
