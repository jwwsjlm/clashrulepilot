package ruleindex

import (
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
	"time"

	"github.com/goccy/go-yaml"
	gh "github.com/google/go-github/v81/github"
	"github.com/hashicorp/go-retryablehttp"
	bolt "go.etcd.io/bbolt"
)

type Action string

const (
	Direct     Action = "direct"
	Proxy      Action = "proxy"
	Category   Action = "category"
	GeoSiteCN  Action = "geosite-cn"
	GeoSiteGFW Action = "geosite-gfw"
	GeoSite           = GeoSiteCN // backward-compatible name for GEOSITE:CN
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

type diskMetadata struct {
	Version             int                   `json:"version"`
	UpdatedAt           time.Time             `json:"updated_at"`
	Sources             map[string]sourceMeta `json:"sources"`
	RuleVersion         string                `json:"rule_version,omitempty"`
	UpstreamSHA         string                `json:"upstream_sha,omitempty"`
	UpstreamCommittedAt time.Time             `json:"upstream_committed_at,omitempty"`
	UpstreamCheckedAt   time.Time             `json:"upstream_checked_at,omitempty"`
	Direct              int                   `json:"direct"`
	Proxy               int                   `json:"proxy"`
	Category            int                   `json:"category"`
	GeoSite             int                   `json:"geosite"`
	GFW                 int                   `json:"gfw"`
}

type Status struct {
	Enabled             bool
	Loaded              bool
	UpdatedAt           time.Time
	Sources             int
	Direct              int
	Proxy               int
	Category            int
	GeoSite             int
	GFW                 int
	LastError           string
	RuleVersion         string
	IndexedUpstreamSHA  string
	UpstreamSHA         string
	UpstreamCommittedAt time.Time
	UpstreamCheckedAt   time.Time
	UpstreamState       string
	UpstreamCheckError  string
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
	gfwURL       string
	http         *http.Client
	github       *gh.Client
	syncMu       sync.Mutex
	dbMu         sync.RWMutex
	db           *bolt.DB
	metadata     diskMetadata
	statusMu     sync.RWMutex
	status       Status
}

type source struct {
	name, url, remoteSHA string
	action               Action
	domainBehavior       bool
}

var (
	bucketExact    = []byte("exact")
	bucketSuffix   = []byte("suffix")
	bucketKeyword  = []byte("keyword")
	bucketMetadata = []byte("metadata")
	metadataKey    = []byte("snapshot")
)

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
	m := &Manager{
		enabled: enabled, dataDir: dataDir, upstreamRepo: repo, branch: branch,
		geositeURL: "https://raw.githubusercontent.com/MetaCubeX/meta-rules-dat/meta/geo/geosite/classical/cn.yaml",
		gfwURL:     "https://raw.githubusercontent.com/MetaCubeX/meta-rules-dat/meta/geo/geosite/classical/gfw.yaml",
		http:       httpClient, github: githubClient,
	}
	m.status.Enabled = enabled
	return m
}

func (m *Manager) Load() error {
	if !m.enabled {
		return nil
	}
	m.cleanupTemporaryFiles()
	m.removeLegacySnapshots()
	db, metadata, err := openReadDatabase(m.databasePath())
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	m.dbMu.Lock()
	m.db = db
	m.metadata = metadata
	m.dbMu.Unlock()
	m.installStatus(metadata)
	return nil
}

func (m *Manager) Close() error {
	m.dbMu.Lock()
	defer m.dbMu.Unlock()
	if m.db == nil {
		return nil
	}
	err := m.db.Close()
	m.db = nil
	return err
}

func (m *Manager) Sync(ctx context.Context) (bool, error) {
	if !m.enabled {
		return false, nil
	}
	m.syncMu.Lock()
	defer m.syncMu.Unlock()

	m.dbMu.RLock()
	old := m.metadata
	hadDatabase := m.db != nil
	m.dbMu.RUnlock()
	if old.Sources == nil {
		old.Sources = map[string]sourceMeta{}
	}
	sources, err := m.discoverSources(ctx)
	if err != nil {
		m.setError(err)
		return false, err
	}
	revision, revisionErr := m.fetchUpstreamRevision(ctx)
	checkedAt := time.Now().UTC()
	if err := os.MkdirAll(m.dataDir, 0o755); err != nil {
		m.setError(err)
		return false, err
	}
	m.cleanupTemporaryFiles()
	stageDir := m.stagingSourceDir()
	if err := os.MkdirAll(stageDir, 0o755); err != nil {
		m.setError(err)
		return false, err
	}
	defer os.RemoveAll(stageDir)

	metas := make(map[string]sourceMeta, len(sources))
	missingLocalSource := false
	for _, src := range sources {
		oldMeta := old.Sources[src.name]
		currentFile := filepath.Join(m.sourceDir(), filepath.Base(src.name))
		stageFile := filepath.Join(stageDir, filepath.Base(src.name))
		hasCurrentFile := fileExists(currentFile)
		if src.remoteSHA != "" && src.remoteSHA == oldMeta.RemoteSHA && hasCurrentFile {
			if err := copyFile(currentFile, stageFile); err != nil {
				m.setError(err)
				return false, err
			}
			metas[src.name] = oldMeta
			continue
		}
		requestMeta := oldMeta
		if !hasCurrentFile {
			missingLocalSource = true
			requestMeta = sourceMeta{}
		}
		body, meta, notModified, fetchErr := m.fetch(ctx, src.url, requestMeta)
		if fetchErr != nil {
			err = fmt.Errorf("sync %s: %w", src.name, fetchErr)
			m.setError(err)
			return false, err
		}
		if notModified {
			if !fileExists(currentFile) {
				err = fmt.Errorf("sync %s: server returned not modified but local source file is missing", src.name)
				m.setError(err)
				return false, err
			}
			if err := copyFile(currentFile, stageFile); err != nil {
				m.setError(err)
				return false, err
			}
			metas[src.name] = oldMeta
			continue
		}
		meta.RemoteSHA = src.remoteSHA
		if err := os.WriteFile(stageFile, body, 0o644); err != nil {
			m.setError(err)
			return false, err
		}
		metas[src.name] = meta
	}

	changed := !hadDatabase || missingLocalSource || !reflect.DeepEqual(old.Sources, metas)
	if revisionErr == nil && revision.SHA != "" && revision.SHA != old.UpstreamSHA {
		// Persist the repository revision even when the tracked rule files did not change.
		changed = true
	}
	if !changed {
		m.setUpstreamCheck(revision, checkedAt, revisionErr)
		m.setError(nil)
		return false, nil
	}
	metadata := diskMetadata{Version: 4, UpdatedAt: time.Now().UTC(), Sources: metas, RuleVersion: ruleVersion(metas), UpstreamSHA: old.UpstreamSHA, UpstreamCommittedAt: old.UpstreamCommittedAt, UpstreamCheckedAt: old.UpstreamCheckedAt}
	if revisionErr == nil {
		metadata.UpstreamSHA = revision.SHA
		metadata.UpstreamCommittedAt = revision.CommittedAt
		metadata.UpstreamCheckedAt = checkedAt
	}
	tmpDatabase := m.databasePath() + ".tmp"
	metadata, err = buildDatabase(tmpDatabase, stageDir, sources, metadata)
	if err != nil {
		m.setError(err)
		return false, err
	}
	if err := replaceDirectory(stageDir, m.sourceDir()); err != nil {
		_ = os.Remove(tmpDatabase)
		m.setError(err)
		return false, err
	}
	if err := m.replaceDatabase(tmpDatabase, metadata); err != nil {
		m.setError(err)
		return false, err
	}
	m.removeLegacySnapshots()
	m.installStatus(metadata)
	if revisionErr != nil {
		m.setUpstreamCheck(upstreamRevision{}, checkedAt, revisionErr)
	}
	m.setError(nil)
	return true, nil
}

func (m *Manager) Query(domain string) []Match {
	domain = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(domain), "."))
	if domain == "" {
		return nil
	}
	m.dbMu.RLock()
	defer m.dbMu.RUnlock()
	if m.db == nil {
		return nil
	}
	seen := map[string]bool{}
	var out []Match
	addEncoded := func(data []byte) error {
		if len(data) == 0 {
			return nil
		}
		var entries []Entry
		if err := json.Unmarshal(data, &entries); err != nil {
			return err
		}
		for _, entry := range entries {
			key := string(entry.Action) + "\x00" + entry.Kind + "\x00" + entry.Pattern + "\x00" + entry.Source
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, Match{Kind: entry.Kind, Pattern: entry.Pattern, Action: entry.Action, Source: entry.Source})
		}
		return nil
	}
	_ = m.db.View(func(tx *bolt.Tx) error {
		if b := tx.Bucket(bucketExact); b != nil {
			if err := addEncoded(b.Get([]byte(domain))); err != nil {
				return err
			}
		}
		if b := tx.Bucket(bucketSuffix); b != nil {
			candidate := domain
			for {
				if err := addEncoded(b.Get([]byte(candidate))); err != nil {
					return err
				}
				dot := strings.IndexByte(candidate, '.')
				if dot < 0 {
					break
				}
				candidate = candidate[dot+1:]
			}
		}
		if b := tx.Bucket(bucketKeyword); b != nil {
			return b.ForEach(func(key, value []byte) error {
				if strings.Contains(domain, string(key)) {
					return addEncoded(value)
				}
				return nil
			})
		}
		return nil
	})
	sortMatches(out)
	return out
}

func (m *Manager) Status() Status {
	m.statusMu.RLock()
	defer m.statusMu.RUnlock()
	return m.status
}

// CheckLatest performs a lightweight branch HEAD check. It does not rebuild the
// local index; it only updates the in-memory status so the Bot can report whether
// the indexed snapshot is still current.
func (m *Manager) CheckLatest(ctx context.Context) Status {
	if !m.enabled {
		return m.Status()
	}
	revision, err := m.fetchUpstreamRevision(ctx)
	m.setUpstreamCheck(revision, time.Now().UTC(), err)
	return m.Status()
}

type upstreamRevision struct {
	SHA         string
	CommittedAt time.Time
}

func (m *Manager) fetchUpstreamRevision(ctx context.Context) (upstreamRevision, error) {
	parts := strings.SplitN(strings.Trim(m.upstreamRepo, "/"), "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return upstreamRevision{}, fmt.Errorf("UPSTREAM_REPO must be owner/repository")
	}
	commit, _, err := m.github.Repositories.GetCommit(ctx, parts[0], parts[1], m.branch, nil)
	if err != nil {
		return upstreamRevision{}, fmt.Errorf("check upstream revision: %w", err)
	}
	committedAt := commit.GetCommit().GetCommitter().GetDate().Time
	if committedAt.IsZero() {
		committedAt = commit.GetCommit().GetAuthor().GetDate().Time
	}
	return upstreamRevision{SHA: commit.GetSHA(), CommittedAt: committedAt}, nil
}

func (m *Manager) setUpstreamCheck(revision upstreamRevision, checkedAt time.Time, err error) {
	m.statusMu.Lock()
	defer m.statusMu.Unlock()
	m.status.UpstreamCheckedAt = checkedAt
	if err != nil {
		m.status.UpstreamState = "error"
		m.status.UpstreamCheckError = err.Error()
		return
	}
	m.status.UpstreamCheckError = ""
	m.status.UpstreamSHA = revision.SHA
	m.status.UpstreamCommittedAt = revision.CommittedAt
	if revision.SHA == "" || m.status.IndexedUpstreamSHA == "" {
		m.status.UpstreamState = "unknown"
	} else if revision.SHA == m.status.IndexedUpstreamSHA {
		m.status.UpstreamState = "latest"
	} else {
		m.status.UpstreamState = "stale"
	}
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
	sources = append(sources,
		source{name: "GEOSITE_CN.yaml", url: m.geositeURL, action: GeoSiteCN},
		source{name: "GEOSITE_GFW.yaml", url: m.gfwURL, action: GeoSiteGFW},
	)
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

func buildDatabase(path, sourceDir string, sources []source, metadata diskMetadata) (diskMetadata, error) {
	_ = os.Remove(path)
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: 2 * time.Second})
	if err != nil {
		return metadata, err
	}
	defer db.Close()
	if err := db.Update(func(tx *bolt.Tx) error {
		for _, name := range [][]byte{bucketExact, bucketSuffix, bucketKeyword, bucketMetadata} {
			if _, err := tx.CreateBucketIfNotExists(name); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return metadata, err
	}
	for _, src := range sources {
		data, err := os.ReadFile(filepath.Join(sourceDir, filepath.Base(src.name)))
		if err != nil {
			return metadata, fmt.Errorf("read staged source %s: %w", src.name, err)
		}
		var entries []Entry
		if src.domainBehavior {
			entries, err = parseDomainBehavior(data, src.action, src.name)
		} else {
			entries, err = parseClassical(data, src.action, src.name)
		}
		if err != nil {
			return metadata, fmt.Errorf("parse %s: %w", src.name, err)
		}
		if err := db.Update(func(tx *bolt.Tx) error { return storeEntries(tx, entries) }); err != nil {
			return metadata, fmt.Errorf("index %s: %w", src.name, err)
		}
		for _, entry := range entries {
			switch entry.Action {
			case Direct:
				metadata.Direct++
			case Proxy:
				metadata.Proxy++
			case Category:
				metadata.Category++
			case GeoSiteCN:
				metadata.GeoSite++
			case GeoSiteGFW:
				metadata.GFW++
			}
		}
	}
	encoded, err := json.Marshal(metadata)
	if err != nil {
		return metadata, err
	}
	if err := db.Update(func(tx *bolt.Tx) error { return tx.Bucket(bucketMetadata).Put(metadataKey, encoded) }); err != nil {
		return metadata, err
	}
	if err := db.Sync(); err != nil {
		return metadata, err
	}
	return metadata, nil
}

func storeEntries(tx *bolt.Tx, entries []Entry) error {
	for _, entry := range entries {
		var bucketName []byte
		switch entry.Kind {
		case "DOMAIN":
			bucketName = bucketExact
		case "DOMAIN-SUFFIX":
			bucketName = bucketSuffix
		case "DOMAIN-KEYWORD":
			bucketName = bucketKeyword
		default:
			continue
		}
		bucket := tx.Bucket(bucketName)
		key := []byte(entry.Pattern)
		var existing []Entry
		if value := bucket.Get(key); len(value) > 0 {
			if err := json.Unmarshal(value, &existing); err != nil {
				return err
			}
		}
		existing = append(existing, entry)
		encoded, err := json.Marshal(existing)
		if err != nil {
			return err
		}
		if err := bucket.Put(key, encoded); err != nil {
			return err
		}
	}
	return nil
}

func openReadDatabase(path string) (*bolt.DB, diskMetadata, error) {
	db, err := bolt.Open(path, 0o600, &bolt.Options{ReadOnly: true, Timeout: 2 * time.Second})
	if err != nil {
		return nil, diskMetadata{}, err
	}
	var metadata diskMetadata
	if err := db.View(func(tx *bolt.Tx) error {
		bucket := tx.Bucket(bucketMetadata)
		if bucket == nil || len(bucket.Get(metadataKey)) == 0 {
			return fmt.Errorf("index metadata is missing")
		}
		return json.Unmarshal(bucket.Get(metadataKey), &metadata)
	}); err != nil {
		_ = db.Close()
		return nil, diskMetadata{}, err
	}
	return db, metadata, nil
}

func (m *Manager) replaceDatabase(tmp string, metadata diskMetadata) error {
	m.dbMu.Lock()
	defer m.dbMu.Unlock()
	if m.db != nil {
		if err := m.db.Close(); err != nil {
			return err
		}
		m.db = nil
	}
	target := m.databasePath()
	backup := target + ".prev"
	_ = os.Remove(backup)
	if fileExists(target) {
		if err := os.Rename(target, backup); err != nil {
			return err
		}
	}
	if err := os.Rename(tmp, target); err != nil {
		_ = os.Rename(backup, target)
		return err
	}
	db, loadedMetadata, err := openReadDatabase(target)
	if err != nil {
		_ = os.Remove(target)
		_ = os.Rename(backup, target)
		if restored, restoredMetadata, restoreErr := openReadDatabase(target); restoreErr == nil {
			m.db = restored
			m.metadata = restoredMetadata
		}
		return err
	}
	m.db = db
	m.metadata = loadedMetadata
	_ = os.Remove(backup)
	return nil
}

func replaceDirectory(stage, target string) error {
	backup := target + ".prev"
	_ = os.RemoveAll(backup)
	if fileExists(target) {
		if err := os.Rename(target, backup); err != nil {
			return err
		}
	}
	if err := os.Rename(stage, target); err != nil {
		_ = os.Rename(backup, target)
		return err
	}
	_ = os.RemoveAll(backup)
	return nil
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

func sortMatches(out []Match) {
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
}

func (m *Manager) installStatus(metadata diskMetadata) {
	status := Status{
		Enabled: true, Loaded: true, UpdatedAt: metadata.UpdatedAt, Sources: len(metadata.Sources),
		Direct: metadata.Direct, Proxy: metadata.Proxy, Category: metadata.Category, GeoSite: metadata.GeoSite, GFW: metadata.GFW,
		RuleVersion: metadata.RuleVersion, IndexedUpstreamSHA: metadata.UpstreamSHA, UpstreamSHA: metadata.UpstreamSHA,
		UpstreamCommittedAt: metadata.UpstreamCommittedAt, UpstreamCheckedAt: metadata.UpstreamCheckedAt,
	}
	if metadata.UpstreamSHA != "" {
		status.UpstreamState = "latest"
	} else {
		status.UpstreamState = "unknown"
	}
	m.statusMu.Lock()
	status.LastError = m.status.LastError
	status.UpstreamCheckError = m.status.UpstreamCheckError
	m.status = status
	m.statusMu.Unlock()
}

func ruleVersion(metas map[string]sourceMeta) string {
	keys := make([]string, 0, len(metas))
	for name := range metas {
		keys = append(keys, name)
	}
	sort.Strings(keys)
	h := sha256.New()
	for _, name := range keys {
		meta := metas[name]
		fmt.Fprintf(h, "%s\x00%s\x00%s\n", name, meta.RemoteSHA, meta.SHA256)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func (m *Manager) setError(err error) {
	m.statusMu.Lock()
	defer m.statusMu.Unlock()
	if err == nil {
		m.status.LastError = ""
	} else {
		m.status.LastError = err.Error()
	}
}

func (m *Manager) cleanupTemporaryFiles() {
	recoverBackup(m.databasePath())
	recoverBackup(m.sourceDir())
	for _, path := range []string{m.databasePath() + ".tmp", filepath.Join(m.dataDir, "upstream-index.json.gz.tmp"), m.stagingSourceDir()} {
		_ = os.RemoveAll(path)
	}
}

func recoverBackup(target string) {
	backup := target + ".prev"
	if !fileExists(backup) {
		return
	}
	if !fileExists(target) {
		_ = os.Rename(backup, target)
		return
	}
	_ = os.RemoveAll(backup)
}

func (m *Manager) removeLegacySnapshots() {
	_ = os.Remove(filepath.Join(m.dataDir, "upstream-index.json.gz"))
	matches, _ := filepath.Glob(filepath.Join(m.dataDir, "upstream-index-*.json.gz"))
	for _, name := range matches {
		_ = os.Remove(name)
	}
}

func (m *Manager) databasePath() string     { return filepath.Join(m.dataDir, "upstream-index.db") }
func (m *Manager) sourceDir() string        { return filepath.Join(m.dataDir, "upstream") }
func (m *Manager) stagingSourceDir() string { return filepath.Join(m.dataDir, "upstream.next") }

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func copyFile(source, target string) error {
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(target)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}
