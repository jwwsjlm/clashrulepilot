package app

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"clashrulepilot/internal/config"
	gh "clashrulepilot/internal/github"
	gl "clashrulepilot/internal/gitlab"
	"clashrulepilot/internal/lookup"
	"clashrulepilot/internal/repository"
	"clashrulepilot/internal/ruleindex"
	"clashrulepilot/internal/rules"
	runtimestate "clashrulepilot/internal/state"
	"clashrulepilot/internal/syncer"
	"github.com/goccy/go-yaml"
	"golang.org/x/sync/singleflight"
)

const personalPath = "data/personal_rules.json"

type Service struct {
	cfg            config.Config
	repo           repository.Repository
	upstream       *syncer.Client
	index          *ruleindex.Manager
	lookup         *lookup.Inspector
	state          *runtimestate.DB
	mu             sync.Mutex
	storeMu        sync.RWMutex
	store          rules.Store
	storeLoaded    bool
	storeLastError string
	storeRevision  string
	storeFetchedAt time.Time
	accessMu       sync.RWMutex
	access         repository.AccessReport
	syncGroup      singleflight.Group
	syncStatusMu   sync.RWMutex
	syncStatus     SyncStatus
	notifyMu       sync.RWMutex
	notify         func(int64, string)
	runCtxMu       sync.RWMutex
	runCtx         context.Context
	syncTask       func(context.Context) (Result, error)
}
type ConflictError struct{ Existing rules.Rule }
type ReadOnlyError struct{ Reason string }

func (e *ReadOnlyError) Error() string {
	if e.Reason == "" {
		return "rule repository is read-only"
	}
	return "rule repository is read-only: " + e.Reason
}

func (e *ConflictError) Error() string {
	return fmt.Sprintf("domain already exists as %s/%s", e.Existing.Action, e.Existing.Match)
}

type Result struct {
	Commit       string
	Changed      int
	Store        rules.Store
	IndexChanged bool
	Related      []rules.Rule
	Queued       bool
	QueueID      string
}

type QueryTiming struct {
	Personal, Upstream, LocalDNS, DomesticDNS, ForeignDNS, GeoIP, Total time.Duration
	Shared                                                              bool
}

type QueryProgress struct {
	Stage  string
	Timing QueryTiming
}

type QueryResult struct {
	Domain        string
	Personal      []rules.Rule
	PersonalError string
	Upstream      []ruleindex.Match
	Network       lookup.Report
	Timing        QueryTiming
}

type SyncStatus struct {
	Running    bool
	Source     string
	Stage      string
	StartedAt  time.Time
	FinishedAt time.Time
	Duration   time.Duration
	Shared     bool
	LastError  string
}

type SelfCheckItem struct{ Level, Text string }
type SelfCheckReport struct {
	Items                    []SelfCheckItem
	Passed, Warnings, Failed int
	CheckedAt                time.Time
}

func New(cfg config.Config) (*Service, error) {
	var repo repository.Repository
	switch cfg.RuleRepoProvider {
	case "github":
		repo = gh.New(cfg.GitHubToken, cfg.RuleRepoProject, cfg.RuleRepoBranch)
	case "gitlab":
		var err error
		repo, err = gl.New(cfg.GitLabBaseURL, cfg.GitLabToken, cfg.RuleRepoProject, cfg.RuleRepoBranch)
		if err != nil {
			return nil, fmt.Errorf("initialize GitLab client: %w", err)
		}
	default:
		return nil, fmt.Errorf("unsupported rule repository provider %q", cfg.RuleRepoProvider)
	}
	stateDB, err := runtimestate.Open(cfg.DataDir)
	if err != nil {
		return nil, fmt.Errorf("open runtime state: %w", err)
	}
	if err := stateDB.ImportLegacy(filepath.Join(cfg.DataDir, "personal_rules.cache.json")); err != nil {
		log.Printf("legacy personal rules cache import failed: %v", err)
	}
	service := &Service{cfg: cfg, repo: repo, state: stateDB, upstream: syncer.New(cfg.UpstreamRepo, cfg.UpstreamBranch, cfg.GitHubToken), index: ruleindex.New(cfg.UpstreamIndex, cfg.DataDir, cfg.UpstreamRepo, cfg.UpstreamBranch, cfg.GitHubToken), lookup: lookup.New(cfg.GeoIPAPIURL, lookup.DoHConfig{
		Enabled: cfg.DoHEnabled, Endpoints: cfg.DoHAPIURLs,
		Domestic: lookup.DNSGroupConfig{Enabled: cfg.DomesticDNSEnabled, Endpoints: cfg.DomesticDNSURLs},
		Foreign:  lookup.DNSGroupConfig{Enabled: cfg.ForeignDNSEnabled, Endpoints: cfg.ForeignDNSURLs},
		Timeout:  cfg.DNSTimeout, CacheSize: cfg.DNSCacheSize, QueryTimeout: cfg.QueryTimeout,
	})}
	if snapshot, ok, loadErr := stateDB.LoadStore(); loadErr == nil && ok {
		if store, decodeErr := rules.Decode(snapshot.Data); decodeErr == nil {
			service.setStoreSnapshot(store, snapshot.Revision, snapshot.FetchedAt)
		}
	}
	if access, ok, _ := stateDB.LoadAccess(); ok {
		service.access = access
	}
	return service, nil
}
func (s *Service) Close() error {
	indexErr := s.index.Close()
	stateErr := s.state.Close()
	if indexErr != nil {
		return indexErr
	}
	return stateErr
}
func (s *Service) SyncEnabled() bool             { return s.cfg.SyncUpstream }
func (s *Service) IndexEnabled() bool            { return s.cfg.UpstreamIndex }
func (s *Service) RepoWebURL() string            { return s.repo.WebURL() }
func (s *Service) RepoRawURL(file string) string { return s.repo.RawURL(file) }
func (s *Service) IndexStatus() ruleindex.Status { return s.index.Status() }
func (s *Service) CheckIndexStatus(ctx context.Context) ruleindex.Status {
	return s.index.CheckLatest(ctx)
}
func (s *Service) LookupStatus() lookup.Status { return s.lookup.Status() }
func (s *Service) AccessStatus() repository.AccessReport {
	s.accessMu.RLock()
	defer s.accessMu.RUnlock()
	return s.access
}
func (s *Service) ReadOnly() bool                          { access := s.AccessStatus(); return !access.Writable }
func (s *Service) Queue() ([]runtimestate.Mutation, error) { return s.state.Mutations() }
func (s *Service) QueueForUser(userID int64) ([]runtimestate.Mutation, error) {
	items, err := s.state.Mutations()
	if err != nil || userID == 0 {
		return items, err
	}
	filtered := items[:0]
	for _, item := range items {
		if item.UserID == userID || item.ChatID == userID {
			filtered = append(filtered, item)
		}
	}
	return filtered, nil
}
func (s *Service) CancelQueued(id string, actors ...int64) error {
	item, err := s.queuedMutation(id)
	if err != nil {
		return err
	}
	if len(actors) > 0 && actors[0] != 0 && item.UserID != actors[0] && item.ChatID != actors[0] {
		return fmt.Errorf("queued mutation belongs to another user")
	}
	return s.state.DeleteMutation(id)
}
func (s *Service) ResumeQueued(id string, force bool, actors ...int64) error {
	items, err := s.state.Mutations()
	if err != nil {
		return err
	}
	for _, item := range items {
		if item.ID != id {
			continue
		}
		if len(actors) > 0 && actors[0] != 0 && item.UserID != actors[0] && item.ChatID != actors[0] {
			return fmt.Errorf("queued mutation belongs to another user")
		}
		item.Force = force
		item.Status = "pending"
		item.LastError = ""
		item.NextAttempt = time.Now().UTC()
		return s.state.UpdateMutation(item)
	}
	return fmt.Errorf("queued mutation not found")
}

func (s *Service) queuedMutation(id string) (runtimestate.Mutation, error) {
	items, err := s.state.Mutations()
	if err != nil {
		return runtimestate.Mutation{}, err
	}
	for _, item := range items {
		if item.ID == id {
			return item, nil
		}
	}
	return runtimestate.Mutation{}, fmt.Errorf("queued mutation not found")
}
func (s *Service) SyncStatus() SyncStatus {
	s.syncStatusMu.RLock()
	defer s.syncStatusMu.RUnlock()
	return s.syncStatus
}
func (s *Service) SetNotifier(fn func(int64, string)) {
	s.notifyMu.Lock()
	s.notify = fn
	s.notifyMu.Unlock()
}
func (s *Service) sendNotice(chatID int64, text string) {
	if chatID == 0 {
		return
	}
	s.notifyMu.RLock()
	fn := s.notify
	s.notifyMu.RUnlock()
	if fn != nil {
		fn(chatID, text)
	}
}

func (s *Service) StartWorkers(ctx context.Context) {
	s.runCtxMu.Lock()
	s.runCtx = ctx
	s.runCtxMu.Unlock()
	go s.periodic(ctx, s.cfg.PreflightInterval, func(run context.Context) { s.Preflight(run) })
	go s.periodic(ctx, s.cfg.StoreRefreshInterval, func(run context.Context) {
		if err := s.RefreshStore(run); err != nil {
			log.Printf("personal rules background refresh failed: %v", err)
		}
	})
	go s.periodic(ctx, s.cfg.MutationRetryInterval, func(run context.Context) {
		if err := s.ProcessQueue(run); err != nil {
			log.Printf("mutation queue processing failed: %v", err)
		}
	})
}

func (s *Service) periodic(ctx context.Context, interval time.Duration, fn func(context.Context)) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			run, cancel := context.WithTimeout(ctx, interval)
			fn(run)
			cancel()
		}
	}
}

func (s *Service) Bootstrap(ctx context.Context) error {
	if err := s.index.Load(); err != nil {
		return fmt.Errorf("load upstream index: %w", err)
	}
	access := s.Preflight(ctx)
	if !access.Readable && access.Authenticated {
		if err := s.repo.EnsureRepo(ctx); err == nil {
			access = s.Preflight(ctx)
		}
	}
	if !access.Readable {
		if _, err := s.LoadStore(ctx); err == nil {
			log.Printf("%s repository unavailable; continuing with local personal rules cache: %s", s.cfg.RuleRepoProvider, access.Error)
			return nil
		}
		log.Printf("%s repository unavailable and no personal cache exists; continuing in read-only mode: %s", s.cfg.RuleRepoProvider, access.Error)
		return nil
	}
	if err := s.RefreshStore(ctx); err != nil {
		log.Printf("personal rules refresh failed during bootstrap: %v", err)
	}
	store, err := s.LoadStore(ctx)
	if err != nil {
		store = rules.Empty()
		s.setStoreSnapshot(store, access.Revision, time.Now().UTC())
	}
	if !access.Writable {
		return nil
	}
	files, err := s.desiredFiles(ctx, &store, nil, nil)
	if err != nil {
		return err
	}
	sha, err := s.commitChanged(ctx, files, "chore: initialize ClashRulePilot rules", access.Revision)
	if err != nil {
		log.Printf("rule repository bootstrap commit unavailable; continuing read-only until retry: %v", err)
		return nil
	}
	if sha != "" {
		s.setStoreSnapshot(store, sha, time.Now().UTC())
		if err := s.persistStoreState(store, sha); err != nil {
			log.Printf("persist bootstrap personal rules state: %v", err)
		}
	}
	s.Preflight(ctx)
	return nil
}

func (s *Service) Preflight(ctx context.Context) repository.AccessReport {
	report := s.repo.CheckAccess(ctx)
	previous := s.AccessStatus()
	if report.Transient && previous.Writable {
		report.Authenticated = previous.Authenticated
		report.Writable = true
		report.Public = previous.Public
		report.User = previous.User
		if report.Branch == "" {
			report.Branch = previous.Branch
		}
		if report.Revision == "" {
			report.Revision = previous.Revision
		}
	}
	if report.Readable && report.Authenticated && !report.Writable && report.Error == "" {
		report.Error = "Token 对目标仓库没有写权限"
	}
	if report.Public && report.Readable {
		checkCtx, cancel := context.WithTimeout(ctx, 6*time.Second)
		request, requestErr := http.NewRequestWithContext(checkCtx, http.MethodGet, s.repo.RawURL("openclash/personal-overwrite.ini"), nil)
		var response *http.Response
		var err error
		if requestErr == nil {
			response, err = http.DefaultClient.Do(request)
		} else {
			err = requestErr
		}
		if err != nil {
			report.RawError = err.Error()
		} else {
			report.RawAccessible = response.StatusCode == http.StatusOK
			if !report.RawAccessible {
				report.RawError = fmt.Sprintf("HTTP %d", response.StatusCode)
			}
			response.Body.Close()
		}
		cancel()
	}
	s.accessMu.Lock()
	s.access = report
	s.accessMu.Unlock()
	if s.state != nil {
		if err := s.state.SaveAccess(report); err != nil {
			log.Printf("save access report: %v", err)
		}
	}
	return report
}

func (s *Service) RefreshStore(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.refreshStore(ctx)
}

func (s *Service) refreshStore(ctx context.Context) error {
	revision, err := s.repo.HeadRevision(ctx)
	if err != nil {
		s.markStoreUnavailable(err)
		return err
	}
	s.storeMu.RLock()
	currentRevision := s.storeRevision
	loaded := s.storeLoaded
	s.storeMu.RUnlock()
	if loaded && revision != "" && revision == currentRevision {
		return nil
	}
	b, _, err := s.repo.GetFileAtRevision(ctx, personalPath, revision)
	if err != nil {
		s.markStoreUnavailable(err)
		return err
	}
	if len(b) == 0 {
		b, _ = rules.Encode(rules.Empty())
	}
	store, err := rules.Decode(b)
	if err != nil {
		s.markStoreUnavailable(err)
		return err
	}
	s.setStoreSnapshot(store, revision, time.Now().UTC())
	return s.persistStoreState(store, revision)
}

func (s *Service) LoadStore(ctx context.Context) (rules.Store, error) {
	_ = ctx
	s.storeMu.RLock()
	if s.storeLoaded {
		store := cloneStore(s.store)
		s.storeMu.RUnlock()
		return store, nil
	}
	s.storeMu.RUnlock()
	s.storeMu.RLock()
	lastError := s.storeLastError
	s.storeMu.RUnlock()
	if lastError == "" {
		lastError = "personal rules cache is not available"
	}
	return rules.Store{}, errors.New(lastError)
}

func (s *Service) markStoreUnavailable(err error) {
	s.storeMu.Lock()
	defer s.storeMu.Unlock()
	s.storeLastError = err.Error()
}

func cloneStore(store rules.Store) rules.Store {
	store.Rules = append([]rules.Rule(nil), store.Rules...)
	return store
}

func (s *Service) setStoreSnapshot(store rules.Store, revision string, fetchedAt time.Time) {
	s.storeMu.Lock()
	defer s.storeMu.Unlock()
	s.storeLastError = ""
	s.store = cloneStore(store)
	s.storeLoaded = true
	s.storeRevision = revision
	s.storeFetchedAt = fetchedAt
}

func (s *Service) persistStoreState(store rules.Store, revision string) error {
	encoded, err := rules.Encode(store)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(encoded)
	return s.state.SaveStore(runtimestate.StoreSnapshot{Data: encoded, Revision: revision, SHA256: fmt.Sprintf("%x", sum[:]), FetchedAt: time.Now().UTC()})
}

func (s *Service) AddRule(ctx context.Context, r rules.Rule, force bool) (Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	access := s.AccessStatus()
	if !access.Writable {
		return Result{}, &ReadOnlyError{Reason: access.Error}
	}
	opID := runtimestate.NewMutation()
	for attempt := 0; attempt < 2; attempt++ {
		if err := s.refreshStore(ctx); err != nil {
			return s.enqueueMutation(runtimestate.Mutation{ID: opID, Operation: "add", Rule: r, Force: force, UserID: r.CreatedBy}, err)
		}
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
		revision := s.currentStoreRevision()
		sha, err := s.commitChanged(ctx, files, "feat: update personal rule [crp:"+opID+"]", revision)
		if errors.Is(err, repository.ErrConflict) {
			continue
		}
		if err != nil {
			return s.enqueueMutation(runtimestate.Mutation{ID: opID, Operation: "add", Rule: r, Force: force, UserID: r.CreatedBy}, err)
		}
		s.setStoreSnapshot(store, sha, time.Now().UTC())
		if cacheErr := s.persistStoreState(store, sha); cacheErr != nil {
			log.Printf("personal rules state write failed after add: %v", cacheErr)
		}
		return Result{Commit: sha, Changed: 1, Store: store, Related: related}, nil
	}
	return Result{}, repository.ErrConflict
}

func (s *Service) RemoveRule(ctx context.Context, domainName string, match *rules.Match, actors ...int64) (Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	access := s.AccessStatus()
	if !access.Writable {
		return Result{}, &ReadOnlyError{Reason: access.Error}
	}
	opID := runtimestate.NewMutation()
	actor := int64(0)
	if len(actors) > 0 {
		actor = actors[0]
	}
	for attempt := 0; attempt < 2; attempt++ {
		if err := s.refreshStore(ctx); err != nil {
			return s.enqueueMutation(runtimestate.Mutation{ID: opID, Operation: "remove", Domain: domainName, Match: match, UserID: actor, ChatID: actor}, err)
		}
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
		sha, err := s.commitChanged(ctx, files, "feat: remove personal rule [crp:"+opID+"]", s.currentStoreRevision())
		if errors.Is(err, repository.ErrConflict) {
			continue
		}
		if err != nil {
			return s.enqueueMutation(runtimestate.Mutation{ID: opID, Operation: "remove", Domain: domainName, Match: match, UserID: actor, ChatID: actor}, err)
		}
		s.setStoreSnapshot(store, sha, time.Now().UTC())
		_ = s.persistStoreState(store, sha)
		return Result{Commit: sha, Changed: n, Store: store}, nil
	}
	return Result{}, repository.ErrConflict
}

func (s *Service) currentStoreRevision() string {
	s.storeMu.RLock()
	defer s.storeMu.RUnlock()
	return s.storeRevision
}

func (s *Service) enqueueMutation(m runtimestate.Mutation, cause error) (Result, error) {
	access := s.AccessStatus()
	if !access.Writable {
		return Result{}, &ReadOnlyError{Reason: access.Error}
	}
	m.Status = "pending"
	if m.ChatID == 0 {
		m.ChatID = m.UserID
	}
	m.LastError = cause.Error()
	m.NextAttempt = time.Now().UTC().Add(s.cfg.MutationRetryInterval)
	if err := s.state.Enqueue(m, s.cfg.MutationQueueLimit); err != nil {
		return Result{}, fmt.Errorf("queue mutation after %v: %w", cause, err)
	}
	return Result{Queued: true, QueueID: m.ID}, nil
}

func (s *Service) ProcessQueue(ctx context.Context) error {
	if s.ReadOnly() {
		return nil
	}
	items, err := s.state.Mutations()
	if err != nil {
		return err
	}
	blockedDomains := map[string]bool{}
	for _, item := range items {
		domainKey := mutationDomain(item)
		if blockedDomains[domainKey] {
			continue
		}
		if item.Status == "paused" || (!item.NextAttempt.IsZero() && item.NextAttempt.After(time.Now())) {
			blockedDomains[domainKey] = true
			continue
		}
		if err := s.applyQueued(ctx, item); err != nil {
			log.Printf("queued mutation %s failed: %v", item.ID, err)
			blockedDomains[domainKey] = true
		}
	}
	return nil
}

func mutationDomain(item runtimestate.Mutation) string {
	if item.Operation == "add" {
		return strings.ToLower(item.Rule.Domain)
	}
	return strings.ToLower(item.Domain)
}

func (s *Service) applyQueued(ctx context.Context, item runtimestate.Mutation) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.refreshStore(ctx); err != nil {
		return s.deferMutation(item, err)
	}
	store, err := s.LoadStore(ctx)
	if err != nil {
		return s.deferMutation(item, err)
	}
	changed := 0
	switch item.Operation {
	case "add":
		conflict, duplicate := store.Add(item.Rule)
		if duplicate {
			_ = s.state.DeleteMutation(item.ID)
			s.sendNotice(item.ChatID, "✅ 排队规则已经存在，无需重复提交："+rules.Token(item.Rule))
			return nil
		}
		if conflict != nil && !item.Force {
			item.Status = "paused"
			item.LastError = "远端规则已变化，需要重新确认"
			_ = s.state.UpdateMutation(item)
			s.sendNotice(item.ChatID, "⚠️ 排队规则与远端最新状态冲突，已暂停。请在“📤 待提交队列”中确认或取消："+rules.Token(item.Rule))
			return nil
		}
		if item.Force {
			store.Move(item.Rule)
		}
		changed = 1
	case "remove":
		changed = store.Remove(item.Domain, item.Match)
		if changed == 0 {
			_ = s.state.DeleteMutation(item.ID)
			s.sendNotice(item.ChatID, "✅ 排队删除的规则已经不存在，无需再次提交："+item.Domain)
			return nil
		}
	default:
		return s.deferMutation(item, fmt.Errorf("unknown queued operation %q", item.Operation))
	}
	encoded, err := rules.Encode(store)
	if err != nil {
		return s.deferMutation(item, err)
	}
	files, err := s.desiredFiles(ctx, &store, encoded, nil)
	if err != nil {
		return s.deferMutation(item, err)
	}
	message := "feat: replay queued rule [crp:" + item.ID + "]"
	sha, err := s.commitChanged(ctx, files, message, s.currentStoreRevision())
	if err != nil {
		return s.deferMutation(item, err)
	}
	s.setStoreSnapshot(store, sha, time.Now().UTC())
	_ = s.persistStoreState(store, sha)
	_ = s.state.DeleteMutation(item.ID)
	s.sendNotice(item.ChatID, fmt.Sprintf("✅ 排队规则已提交\n变更：%d 条\ncommit：%s", changed, shortSHA(sha)))
	return nil
}

func (s *Service) deferMutation(item runtimestate.Mutation, err error) error {
	item.Attempts++
	item.LastError = err.Error()
	item.Status = "pending"
	delay := s.cfg.MutationRetryInterval * time.Duration(1<<min(item.Attempts, 5))
	if delay > 15*time.Minute {
		delay = 15 * time.Minute
	}
	item.NextAttempt = time.Now().UTC().Add(delay)
	if saveErr := s.state.UpdateMutation(item); saveErr != nil {
		return saveErr
	}
	return err
}

func shortSHA(value string) string {
	if len(value) > 12 {
		return value[:12]
	}
	return value
}

func (s *Service) Sync(ctx context.Context) (Result, error) {
	return s.SyncWithSource(ctx, "manual")
}

func (s *Service) SyncWithSource(ctx context.Context, source string) (Result, error) {
	ch := s.syncGroup.DoChan("upstream", func() (any, error) {
		started := time.Now().UTC()
		s.syncStatusMu.Lock()
		s.syncStatus = SyncStatus{Running: true, Source: source, Stage: "准备同步", StartedAt: started}
		s.syncStatusMu.Unlock()
		jobCtx, cancel := context.WithTimeout(s.syncBaseContext(ctx), s.cfg.SyncTimeout)
		defer cancel()
		task := s.syncOnce
		if s.syncTask != nil {
			task = s.syncTask
		}
		result, err := task(jobCtx)
		finished := time.Now().UTC()
		status := SyncStatus{Source: source, Stage: "完成", StartedAt: started, FinishedAt: finished, Duration: finished.Sub(started)}
		if err != nil {
			status.Stage = "失败"
			status.LastError = err.Error()
		}
		s.syncStatusMu.Lock()
		s.syncStatus = status
		s.syncStatusMu.Unlock()
		return result, err
	})
	select {
	case <-ctx.Done():
		return Result{}, ctx.Err()
	case item := <-ch:
		if item.Val == nil {
			return Result{}, item.Err
		}
		result := item.Val.(Result)
		s.syncStatusMu.Lock()
		s.syncStatus.Shared = item.Shared
		s.syncStatusMu.Unlock()
		return result, item.Err
	}
}

func (s *Service) syncBaseContext(fallback context.Context) context.Context {
	s.runCtxMu.RLock()
	root := s.runCtx
	s.runCtxMu.RUnlock()
	if root != nil {
		return root
	}
	return context.WithoutCancel(fallback)
}

func (s *Service) setSyncStage(stage string) {
	s.syncStatusMu.Lock()
	if s.syncStatus.Running {
		s.syncStatus.Stage = stage
	}
	s.syncStatusMu.Unlock()
}

func (s *Service) syncOnce(ctx context.Context) (Result, error) {
	s.setSyncStage("更新本地查询索引")
	indexChanged, indexErr := s.index.Sync(ctx)
	if !s.cfg.SyncUpstream {
		return Result{IndexChanged: indexChanged}, indexErr
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.setSyncStage("下载并发布上游规则")
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
	revision, _ := s.repo.HeadRevision(ctx)
	sha, err := s.commitChanged(ctx, files, "chore: sync upstream rules", revision)
	if err != nil {
		return Result{}, err
	}
	return Result{Commit: sha, Changed: len(up), Store: store, IndexChanged: indexChanged}, indexErr
}

func (s *Service) Query(ctx context.Context, domainName string) (QueryResult, error) {
	return s.QueryWithProgress(ctx, domainName, nil)
}

func (s *Service) QueryWithProgress(ctx context.Context, domainName string, progress func(QueryProgress)) (QueryResult, error) {
	started := time.Now()
	notify := func(stage string, timing QueryTiming) {
		if progress != nil {
			timing.Total = time.Since(started)
			progress(QueryProgress{Stage: stage, Timing: timing})
		}
	}
	timing := QueryTiming{}
	personalStarted := time.Now()
	store, err := s.LoadStore(ctx)
	timing.Personal = time.Since(personalStarted)
	notify("personal", timing)
	upstreamStarted := time.Now()
	result := QueryResult{Domain: domainName, Upstream: s.index.Query(domainName)}
	timing.Upstream = time.Since(upstreamStarted)
	notify("upstream", timing)
	notify("network", timing)
	if err != nil {
		result.PersonalError = err.Error()
	} else {
		for _, r := range rules.Ordered(store.Rules) {
			if rules.Matches(r, domainName) {
				result.Personal = append(result.Personal, r)
			}
		}
	}
	result.Network = s.lookup.Inspect(ctx, domainName)
	timing.LocalDNS = result.Network.Timing.Local
	timing.DomesticDNS = result.Network.Timing.Domestic
	timing.ForeignDNS = result.Network.Timing.Foreign
	timing.GeoIP = result.Network.Timing.GeoIP
	timing.Shared = result.Network.Shared
	timing.Total = time.Since(started)
	result.Timing = timing
	notify("complete", timing)
	return result, nil
}

func (s *Service) SelfCheck(ctx context.Context) SelfCheckReport {
	report := SelfCheckReport{CheckedAt: time.Now().UTC()}
	add := func(level, text string) {
		report.Items = append(report.Items, SelfCheckItem{Level: level, Text: text})
		switch level {
		case "ok":
			report.Passed++
		case "warn":
			report.Warnings++
		default:
			report.Failed++
		}
	}
	access := s.Preflight(ctx)
	if access.Readable {
		add("ok", "发布仓库和分支可访问")
	} else {
		add("fail", "发布仓库不可读："+access.Error)
		return report
	}
	if access.Public {
		add("ok", "规则仓库为公开仓库")
	} else {
		add("fail", "规则仓库不是公开仓库，OpenClash 无法匿名下载 Raw 文件")
	}
	revision := access.Revision
	storeBytes, _, err := s.repo.GetFileAtRevision(ctx, personalPath, revision)
	if err != nil {
		add("fail", "读取 personal_rules.json 失败："+err.Error())
		return report
	}
	store, err := rules.Decode(storeBytes)
	if err != nil {
		add("fail", "personal_rules.json 无法解析："+err.Error())
		return report
	}
	add("ok", fmt.Sprintf("personal_rules.json：%d 条", len(store.Rules)))
	overwriteBytes, _, err := s.repo.GetFileAtRevision(ctx, "openclash/personal-overwrite.ini", revision)
	if err != nil || len(overwriteBytes) == 0 {
		if err == nil {
			err = errors.New("文件不存在")
		}
		add("fail", "读取 personal-overwrite.ini 失败："+err.Error())
		return report
	}
	if !utf8.Valid(overwriteBytes) {
		add("fail", "personal-overwrite.ini 不是有效 UTF-8")
	} else {
		add("ok", "personal-overwrite.ini 使用有效 UTF-8 编码")
	}
	expected := []byte(s.personalOverwrite(store))
	if string(overwriteBytes) == string(expected) {
		add("ok", "远端覆写文件与个人规则一致")
	} else {
		add("fail", "远端覆写文件与当前个人规则不一致")
	}
	text := string(overwriteBytes)
	if !strings.HasPrefix(text, "[YAML]\n") {
		add("fail", "缺少 [YAML] 段头")
	} else if !strings.Contains(text, "\n+rules:\n") {
		add("fail", "缺少 +rules 追加规则段")
	} else {
		add("ok", "[YAML] / +rules 结构正确")
	}
	yamlText := strings.TrimPrefix(text, "[YAML]\n")
	var parsed map[string]any
	if err := yaml.Unmarshal([]byte(yamlText), &parsed); err != nil {
		add("fail", "覆写 YAML 无法解析："+err.Error())
	} else {
		add("ok", "覆写 YAML 可以解析")
	}
	seen := map[string]rules.Action{}
	duplicates := 0
	invalid := 0
	for _, rule := range store.Rules {
		if _, err := rules.ValidatePattern(rule.Match, rule.Domain); err != nil {
			invalid++
		}
		key := string(rule.Match) + "\x00" + rule.Domain
		if action, ok := seen[key]; ok {
			if action != rule.Action {
				add("fail", "存在相同匹配条件的直连/代理冲突："+rules.Token(rule))
			} else {
				duplicates++
			}
		}
		seen[key] = rule.Action
	}
	if invalid == 0 {
		add("ok", "规则类型、内容和排序可以重新生成")
	} else {
		add("fail", fmt.Sprintf("发现 %d 条无效规则", invalid))
	}
	if duplicates == 0 {
		add("ok", "个人规则没有重复项")
	} else {
		add("fail", fmt.Sprintf("发现 %d 条重复规则", duplicates))
	}
	client := &http.Client{Timeout: 10 * time.Second}
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, s.repo.RawURL("openclash/personal-overwrite.ini"), nil)
	response, rawErr := client.Do(request)
	if rawErr != nil {
		add("fail", "Raw 地址访问失败："+rawErr.Error())
	} else {
		body, readErr := io.ReadAll(io.LimitReader(response.Body, 2<<20))
		response.Body.Close()
		if readErr != nil {
			add("fail", "Raw 内容读取失败："+readErr.Error())
		} else if response.StatusCode != http.StatusOK {
			add("fail", fmt.Sprintf("Raw 地址返回 HTTP %d", response.StatusCode))
		} else if string(body) != string(overwriteBytes) {
			add("warn", "Raw 文件仍在传播或缓存中，与仓库 API 内容暂不一致")
		} else {
			add("ok", "personal-overwrite.ini Raw 可访问且内容已同步")
		}
	}
	if strings.TrimSpace(s.cfg.ProxyPolicyGroup) == "" {
		add("fail", "代理策略组名称为空")
	} else {
		add("warn", "策略组“"+s.cfg.ProxyPolicyGroup+"”需要在 OpenClash 当前订阅中实际存在")
	}
	return report
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

func (s *Service) commitChanged(ctx context.Context, files map[string][]byte, msg, expectedRevision string) (string, error) {
	if expectedRevision == "" {
		var err error
		expectedRevision, err = s.repo.HeadRevision(ctx)
		if err != nil {
			expectedRevision = ""
		}
	}
	changed := map[string][]byte{}
	for p, b := range files {
		old, _, err := s.repo.GetFileAtRevision(ctx, p, expectedRevision)
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
	return s.repo.CommitFiles(ctx, changed, msg, expectedRevision)
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
