package app

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"clashrulepilot/internal/config"
	gh "clashrulepilot/internal/github"
	gl "clashrulepilot/internal/gitlab"
	"clashrulepilot/internal/lookup"
	"clashrulepilot/internal/repository"
	"clashrulepilot/internal/ruleindex"
	"clashrulepilot/internal/rules"
	"clashrulepilot/internal/syncer"
)

const personalPath = "data/personal_rules.json"

type Service struct {
	cfg      config.Config
	repo     repository.Repository
	upstream *syncer.Client
	index    *ruleindex.Manager
	lookup   *lookup.Inspector
	mu       sync.Mutex
	ready    bool
	lastSync time.Time
}
type ConflictError struct{ Existing rules.Rule }

func (e *ConflictError) Error() string {
	return fmt.Sprintf("domain already exists as %s/%s", e.Existing.Action, e.Existing.Match)
}

type Result struct {
	Commit       string
	Changed      int
	Store        rules.Store
	IndexChanged bool
	Related      []rules.Rule
}

type QueryResult struct {
	Domain   string
	Personal []rules.Rule
	Upstream []ruleindex.Match
	Network  lookup.Report
}

func New(cfg config.Config) (*Service, error) {
	var repo repository.Repository
	switch cfg.RuleRepoProvider {
	case "github":
		if strings.TrimSpace(cfg.GitHubToken) == "" {
			return nil, fmt.Errorf("GITHUB_TOKEN is required")
		}
		repo = gh.New(cfg.GitHubToken, cfg.RuleRepoProject, cfg.RuleRepoBranch)
	case "gitlab":
		if strings.TrimSpace(cfg.GitLabToken) == "" {
			return nil, fmt.Errorf("GITLAB_TOKEN is required")
		}
		var err error
		repo, err = gl.New(cfg.GitLabBaseURL, cfg.GitLabToken, cfg.RuleRepoProject, cfg.RuleRepoBranch)
		if err != nil {
			return nil, fmt.Errorf("initialize GitLab client: %w", err)
		}
	default:
		return nil, fmt.Errorf("unsupported rule repository provider %q", cfg.RuleRepoProvider)
	}
	return &Service{cfg: cfg, repo: repo, upstream: syncer.New(cfg.UpstreamRepo, cfg.UpstreamBranch, cfg.GitHubToken), index: ruleindex.New(cfg.UpstreamIndex, cfg.DataDir, cfg.UpstreamRepo, cfg.UpstreamBranch, cfg.GitHubToken), lookup: lookup.New(cfg.GeoIPAPIURL, lookup.DoHConfig{
		Enabled: cfg.DoHEnabled, Endpoints: cfg.DoHAPIURLs,
		Domestic: lookup.DNSGroupConfig{Enabled: cfg.DomesticDNSEnabled, Endpoints: cfg.DomesticDNSURLs},
		Foreign:  lookup.DNSGroupConfig{Enabled: cfg.ForeignDNSEnabled, Endpoints: cfg.ForeignDNSURLs},
		Timeout:  cfg.DNSTimeout, CacheSize: cfg.DNSCacheSize,
	})}, nil
}
func (s *Service) Ready() bool                   { s.mu.Lock(); defer s.mu.Unlock(); return s.ready }
func (s *Service) Close() error                  { return s.index.Close() }
func (s *Service) SyncEnabled() bool             { return s.cfg.SyncUpstream }
func (s *Service) IndexEnabled() bool            { return s.cfg.UpstreamIndex }
func (s *Service) RepoWebURL() string            { return s.repo.WebURL() }
func (s *Service) RepoRawURL(file string) string { return s.repo.RawURL(file) }
func (s *Service) IndexStatus() ruleindex.Status { return s.index.Status() }
func (s *Service) LookupStatus() lookup.Status   { return s.lookup.Status() }
func (s *Service) Bootstrap(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.repo.EnsureRepo(ctx); err != nil {
		return err
	}
	if err := s.index.Load(); err != nil {
		return fmt.Errorf("load upstream index: %w", err)
	}
	files, err := s.desiredFiles(ctx, nil, nil, nil)
	if err != nil {
		return err
	}
	if _, err := s.commitChanged(ctx, files, "chore: initialize ClashRulePilot rules"); err != nil {
		return err
	}
	s.ready = true
	return nil
}

func (s *Service) LoadStore(ctx context.Context) (rules.Store, error) {
	b, _, err := s.repo.GetFile(ctx, personalPath)
	if err != nil {
		return rules.Store{}, err
	}
	return rules.Decode(b)
}

func (s *Service) AddRule(ctx context.Context, r rules.Rule, force bool) (Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	store, err := s.LoadStore(ctx)
	if err != nil {
		return Result{}, err
	}
	related := rules.Related(store.Rules, r)
	conflict, duplicate := store.Add(r)
	if duplicate {
		return Result{Store: store}, fmt.Errorf("rule already exists")
	}
	if conflict != nil && !force {
		return Result{}, &ConflictError{Existing: *conflict}
	}
	if force {
		store.Move(r)
	}
	enc, err := rules.Encode(store)
	if err != nil {
		return Result{}, err
	}
	files, err := s.desiredFiles(ctx, &store, enc, nil)
	if err != nil {
		return Result{}, err
	}
	sha, err := s.commitChanged(ctx, files, "feat: update personal rule")
	if err != nil {
		return Result{}, err
	}
	return Result{Commit: sha, Changed: 1, Store: store, Related: related}, nil
}

func (s *Service) RemoveRule(ctx context.Context, domainName string, match *rules.Match) (Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	store, err := s.LoadStore(ctx)
	if err != nil {
		return Result{}, err
	}
	n := store.Remove(domainName, match)
	if n == 0 {
		return Result{Store: store}, fmt.Errorf("rule not found")
	}
	enc, err := rules.Encode(store)
	if err != nil {
		return Result{}, err
	}
	files, err := s.desiredFiles(ctx, &store, enc, nil)
	if err != nil {
		return Result{}, err
	}
	sha, err := s.commitChanged(ctx, files, "feat: remove personal rule")
	if err != nil {
		return Result{}, err
	}
	return Result{Commit: sha, Changed: n, Store: store}, nil
}

func (s *Service) Sync(ctx context.Context) (Result, error) {
	indexChanged, indexErr := s.index.Sync(ctx)
	if !s.cfg.SyncUpstream {
		return Result{IndexChanged: indexChanged}, indexErr
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	store, err := s.LoadStore(ctx)
	if err != nil {
		return Result{}, err
	}
	up, shas, err := s.upstream.FetchRuleYAML(ctx)
	if err != nil {
		return Result{}, err
	}
	enc, err := rules.Encode(store)
	if err != nil {
		return Result{}, err
	}
	files, err := s.desiredFiles(ctx, &store, enc, up)
	if err != nil {
		return Result{}, err
	}
	metaBytes, _, _ := s.repo.GetFile(ctx, "metadata/upstream.json")
	var oldMeta struct {
		Files map[string]string `json:"files"`
	}
	_ = json.Unmarshal(metaBytes, &oldMeta)
	changedUpstream := len(oldMeta.Files) != len(shas)
	if !changedUpstream {
		for name, sha := range shas {
			if oldMeta.Files[name] != sha {
				changedUpstream = true
				break
			}
		}
	}
	if changedUpstream || len(metaBytes) == 0 {
		meta, _ := json.MarshalIndent(map[string]any{"repository": s.cfg.UpstreamRepo, "branch": s.cfg.UpstreamBranch, "synced_at": time.Now().UTC().Format(time.RFC3339), "files": shas}, "", "  ")
		metaBytes = append(meta, '\n')
	}
	files["metadata/upstream.json"] = metaBytes
	sha, err := s.commitChanged(ctx, files, "chore: sync upstream rules")
	if err != nil {
		return Result{}, err
	}
	s.lastSync = time.Now()
	return Result{Commit: sha, Changed: len(up), Store: store, IndexChanged: indexChanged}, indexErr
}

func (s *Service) Query(ctx context.Context, domainName string) (QueryResult, error) {
	store, err := s.LoadStore(ctx)
	if err != nil {
		return QueryResult{}, err
	}
	result := QueryResult{Domain: domainName, Upstream: s.index.Query(domainName)}
	for _, r := range rules.Ordered(store.Rules) {
		if rules.Matches(r, domainName) {
			result.Personal = append(result.Personal, r)
		}
	}
	result.Network = s.lookup.Inspect(ctx, domainName)
	return result, nil
}

func (s *Service) desiredFiles(ctx context.Context, store *rules.Store, encoded []byte, upstream map[string][]byte) (map[string][]byte, error) {
	if store == nil {
		st, err := s.LoadStore(ctx)
		if err != nil {
			if strings.Contains(err.Error(), "404") {
				st = rules.Empty()
			} else {
				return nil, err
			}
		}
		store = &st
	}
	if encoded == nil {
		var err error
		encoded, err = rules.Encode(*store)
		if err != nil {
			return nil, err
		}
	}
	direct := rules.Render(*store, rules.Direct, s.cfg.ProxyPolicyGroup)
	proxy := rules.Render(*store, rules.Proxy, s.cfg.ProxyPolicyGroup)
	files := map[string][]byte{personalPath: append(encoded, '\n'), "rules/personal/My_Direct_Domain.yaml": direct, "rules/personal/My_Proxy_Domain.yaml": proxy, "README.md": []byte(s.readme()), "LICENSE-THIRD-PARTY.md": []byte(thirdPartyLicense()), "openclash/providers.yaml": []byte(s.providers(upstream)), "openclash/rules-order.yaml": []byte(s.ruleOrder(upstream)), "openclash/personal-overwrite.ini": []byte(s.personalOverwrite(*store))}
	if s.cfg.SyncUpstream {
		files["openclash/overwrite.ini"] = []byte(s.overwrite(upstream))
	}
	for name, b := range upstream {
		files["rules/upstream/"+name] = b
	}
	return files, nil
}

func (s *Service) commitChanged(ctx context.Context, files map[string][]byte, msg string) (string, error) {
	changed := map[string][]byte{}
	for p, b := range files {
		old, _, err := s.repo.GetFile(ctx, p)
		if err != nil {
			return "", err
		}
		if string(old) != string(b) {
			changed[p] = b
		}
	}
	if len(changed) == 0 {
		return "", nil
	}
	return s.repo.CommitFiles(ctx, changed, msg)
}
func (s *Service) readme() string {
	return fmt.Sprintf("# ClashRulePilot Rules\n\n个人 OpenClash/Mihomo 规则库，由 ClashRulePilot 维护。代理策略组默认为 `%s`。\n\n上游：[%s](https://github.com/%s)\n\nOpenClash 推荐使用 `openclash/personal-overwrite.ini`。该文件将个人规则以显式 `+rules` 写入，并按精确度排序，使更具体的子域名例外优先。\n\n项目源码不包含在本规则仓库中，本仓库只发布规则文件。\n", s.cfg.ProxyPolicyGroup, s.cfg.UpstreamRepo, s.cfg.UpstreamRepo)
}
func (s *Service) providers(upstream map[string][]byte) string {
	var b strings.Builder
	fmt.Fprintf(&b, "rule-providers:\n  my_proxy:\n    type: http\n    behavior: classical\n    format: yaml\n    url: %s\n    interval: 86400\n  my_direct:\n    type: http\n    behavior: classical\n    format: yaml\n    url: %s\n    interval: 86400\n", s.repo.RawURL("rules/personal/My_Proxy_Domain.yaml"), s.repo.RawURL("rules/personal/My_Direct_Domain.yaml"))
	names := sortedNames(upstream)
	for _, name := range names {
		provider := providerName(name)
		fmt.Fprintf(&b, "  %s:\n    type: http\n    behavior: %s\n    format: yaml\n    url: %s\n    interval: 86400\n", provider, providerBehavior(name), s.repo.RawURL("rules/upstream/"+name))
	}
	return b.String()
}
func (s *Service) ruleOrder(upstream map[string][]byte) string {
	var b strings.Builder
	fmt.Fprintf(&b, "rules:\n  - RULE-SET,my_proxy,%s\n  - RULE-SET,my_direct,DIRECT\n", s.cfg.ProxyPolicyGroup)
	for _, name := range sortedNames(upstream) {
		action := "DIRECT"
		if strings.HasPrefix(name, "Custom_Proxy_") {
			action = s.cfg.ProxyPolicyGroup
		}
		fmt.Fprintf(&b, "  - RULE-SET,%s,%s\n", providerName(name), action)
	}
	return b.String()
}

func (s *Service) overwrite(upstream map[string][]byte) string {
	providers := s.providers(upstream)
	order := strings.Replace(s.ruleOrder(upstream), "rules:\n", "+rules:\n", 1)
	return "[YAML]\n" + providers + "\n" + order
}

func (s *Service) personalOverwrite(store rules.Store) string {
	return "[YAML]\n" + string(rules.RenderExplicit(store, s.cfg.ProxyPolicyGroup))
}

func sortedNames(m map[string][]byte) []string {
	names := make([]string, 0, len(m))
	for name := range m {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
func providerName(name string) string {
	name = strings.TrimSuffix(name, ".yaml")
	name = strings.ToLower(name)
	name = strings.NewReplacer("-", "_", ".", "_").Replace(name)
	return "upstream_" + name
}
func providerBehavior(name string) string {
	switch {
	case strings.Contains(name, "_Domain."):
		return "domain"
	case strings.Contains(name, "_IP.") && !strings.Contains(name, "Classical"):
		return "ipcidr"
	default:
		return "classical"
	}
}
func thirdPartyLicense() string {
	return "# Third-party attribution\n\nThe mirrored upstream rules originate from Aethersailor/Custom_OpenClash_Rules.\nRepository: https://github.com/Aethersailor/Custom_OpenClash_Rules\nThe upstream project is distributed under CC BY-SA 4.0. Preserve attribution and the applicable license when redistributing mirrored rules.\n"
}
