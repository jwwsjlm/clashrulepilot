package app

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"clashrulepilot/internal/config"
	"clashrulepilot/internal/repository"
	"clashrulepilot/internal/rules"
	runtimestate "clashrulepilot/internal/state"
	"github.com/goccy/go-yaml"
)

type mutableRepository struct {
	mu      sync.Mutex
	head    string
	files   map[string][]byte
	err     error
	rawURL  string
	commits int
	access  *repository.AccessReport
}

func (r *mutableRepository) EnsureRepo(context.Context) error { return r.err }
func (r *mutableRepository) CheckAccess(context.Context) repository.AccessReport {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.access != nil {
		return *r.access
	}
	return repository.AccessReport{Provider: "test", Authenticated: true, Readable: r.err == nil, Writable: true, Public: true, Branch: "main", Revision: r.head, Error: errorText(r.err), Transient: r.err != nil}
}
func (r *mutableRepository) HeadRevision(context.Context) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.head, r.err
}
func (r *mutableRepository) GetFile(ctx context.Context, path string) ([]byte, string, error) {
	return r.GetFileAtRevision(ctx, path, r.head)
}
func (r *mutableRepository) GetFileAtRevision(_ context.Context, path, _ string) ([]byte, string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return nil, "", r.err
	}
	return append([]byte(nil), r.files[path]...), r.head, nil
}
func (r *mutableRepository) CommitFiles(_ context.Context, files map[string][]byte, _ string, expected ...string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return "", r.err
	}
	if len(expected) > 0 && expected[0] != "" && expected[0] != r.head {
		return "", repository.ErrConflict
	}
	r.commits++
	r.head = "commit" + string(rune('0'+r.commits))
	for k, v := range files {
		r.files[k] = append([]byte(nil), v...)
	}
	return r.head, nil
}
func (r *mutableRepository) Owner() string        { return "owner" }
func (r *mutableRepository) Repo() string         { return "repo" }
func (r *mutableRepository) Branch() string       { return "main" }
func (r *mutableRepository) WebURL() string       { return "https://example.test/owner/repo" }
func (r *mutableRepository) RawURL(string) string { return r.rawURL }
func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

type testRepository struct {
	file []byte
	err  error
}

func (r *testRepository) EnsureRepo(context.Context) error { return nil }
func (r *testRepository) CheckAccess(context.Context) repository.AccessReport {
	return repository.AccessReport{Provider: "test", Authenticated: true, Readable: r.err == nil, Writable: r.err == nil, Public: true, Branch: "main", Revision: "sha"}
}
func (r *testRepository) HeadRevision(context.Context) (string, error) {
	if r.err != nil {
		return "", r.err
	}
	return "sha", nil
}
func (r *testRepository) GetFile(context.Context, string) ([]byte, string, error) {
	if r.err != nil {
		return nil, "", r.err
	}
	return append([]byte(nil), r.file...), "sha", nil
}
func (r *testRepository) GetFileAtRevision(ctx context.Context, path, revision string) ([]byte, string, error) {
	return r.GetFile(ctx, path)
}
func (r *testRepository) CommitFiles(context.Context, map[string][]byte, string, ...string) (string, error) {
	return "commit", nil
}
func (r *testRepository) Owner() string        { return "owner" }
func (r *testRepository) Repo() string         { return "repo" }
func (r *testRepository) Branch() string       { return "main" }
func (r *testRepository) WebURL() string       { return "https://example.test/owner/repo" }
func (r *testRepository) RawURL(string) string { return "https://example.test/raw" }

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

func TestLoadStoreUsesDiskCacheAfterRemoteFailure(t *testing.T) {
	store := rules.Store{Version: 1, Rules: []rules.Rule{{Domain: "example.com", Match: rules.Exact, Action: rules.Direct}}}
	encoded, err := rules.Encode(store)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	repo := &testRepository{file: encoded}
	stateDB, err := runtimestate.Open(filepath.Join(dir, "data"))
	if err != nil {
		t.Fatal(err)
	}
	defer stateDB.Close()
	s := &Service{cfg: config.Config{DataDir: filepath.Join(dir, "data")}, repo: repo, state: stateDB}
	if err := s.RefreshStore(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, err := s.LoadStore(context.Background())
	if err != nil || len(got.Rules) != 1 {
		t.Fatalf("initial LoadStore got=%+v err=%v", got, err)
	}

	snapshot, ok, err := stateDB.LoadStore()
	if err != nil || !ok {
		t.Fatalf("snapshot missing: %v", err)
	}
	reloaded := &Service{cfg: config.Config{DataDir: filepath.Join(dir, "data")}, repo: &testRepository{err: errors.New("gitlab EOF")}}
	decoded, err := rules.Decode(snapshot.Data)
	if err != nil {
		t.Fatal(err)
	}
	reloaded.setStoreSnapshot(decoded, snapshot.Revision, snapshot.FetchedAt)
	got, err = reloaded.LoadStore(context.Background())
	if err != nil || len(got.Rules) != 1 || got.Rules[0].Domain != "example.com" {
		t.Fatalf("cached LoadStore got=%+v err=%v", got, err)
	}
}

func TestRefreshStoreTracksRemoteRevision(t *testing.T) {
	first, _ := rules.Encode(rules.Store{Version: 1, Rules: []rules.Rule{{Domain: "one.example", Match: rules.Exact, Action: rules.Direct}}})
	second, _ := rules.Encode(rules.Store{Version: 1, Rules: []rules.Rule{{Domain: "two.example", Match: rules.Exact, Action: rules.Proxy}}})
	repo := &mutableRepository{head: "rev1", files: map[string][]byte{personalPath: first}}
	stateDB, _ := runtimestate.Open(t.TempDir())
	defer stateDB.Close()
	s := &Service{cfg: config.Config{}, repo: repo, state: stateDB}
	if err := s.RefreshStore(context.Background()); err != nil {
		t.Fatal(err)
	}
	repo.mu.Lock()
	repo.head = "rev2"
	repo.files[personalPath] = second
	repo.mu.Unlock()
	if err := s.RefreshStore(context.Background()); err != nil {
		t.Fatal(err)
	}
	store, err := s.LoadStore(context.Background())
	if err != nil || len(store.Rules) != 1 || store.Rules[0].Domain != "two.example" {
		t.Fatalf("remote refresh not applied: %+v err=%v", store, err)
	}
}

func TestAddRuleRefreshesRemoteBeforeCommit(t *testing.T) {
	remote, _ := rules.Encode(rules.Store{Version: 1, Rules: []rules.Rule{{Domain: "remote.example", Match: rules.Exact, Action: rules.Direct}}})
	repo := &mutableRepository{head: "rev2", files: map[string][]byte{personalPath: remote}}
	stateDB, _ := runtimestate.Open(t.TempDir())
	defer stateDB.Close()
	s := &Service{cfg: config.Config{ProxyPolicyGroup: "Proxy", MutationQueueLimit: 10, MutationRetryInterval: time.Second}, repo: repo, state: stateDB}
	s.access = repository.AccessReport{Writable: true, Readable: true}
	result, err := s.AddRule(context.Background(), rules.Rule{Domain: "new.example", Match: rules.Suffix, Action: rules.Proxy}, false)
	if err != nil || result.Commit == "" {
		t.Fatalf("add failed: %+v %v", result, err)
	}
	stored, err := rules.Decode(repo.files[personalPath])
	if err != nil || len(stored.Rules) != 2 {
		t.Fatalf("remote rule was overwritten: %+v err=%v", stored, err)
	}
}

func TestOfflineWriteQueuesWithoutChangingActiveStore(t *testing.T) {
	base, _ := rules.Encode(rules.Store{Version: 1, Rules: []rules.Rule{{Domain: "active.example", Match: rules.Exact, Action: rules.Direct}}})
	repo := &mutableRepository{head: "rev1", files: map[string][]byte{personalPath: base}}
	stateDB, _ := runtimestate.Open(t.TempDir())
	defer stateDB.Close()
	s := &Service{cfg: config.Config{ProxyPolicyGroup: "Proxy", MutationQueueLimit: 10, MutationRetryInterval: time.Second}, repo: repo, state: stateDB}
	s.access = repository.AccessReport{Writable: true, Readable: true}
	if err := s.RefreshStore(context.Background()); err != nil {
		t.Fatal(err)
	}
	repo.err = errors.New("temporary EOF")
	result, err := s.AddRule(context.Background(), rules.Rule{Domain: "queued.example", Match: rules.Exact, Action: rules.Proxy, CreatedBy: 123}, false)
	if err != nil || !result.Queued {
		t.Fatalf("expected queued result: %+v err=%v", result, err)
	}
	store, _ := s.LoadStore(context.Background())
	if len(store.Rules) != 1 {
		t.Fatalf("queued mutation changed active cache: %+v", store)
	}
	items, _ := s.Queue()
	if len(items) != 1 || items[0].ChatID != 123 {
		t.Fatalf("queue not persisted: %+v", items)
	}
}

func TestSelfCheckValidatesPublishedOverwrite(t *testing.T) {
	store := rules.Store{Version: 1, Rules: []rules.Rule{{Domain: "example.com", Match: rules.Suffix, Action: rules.Proxy}}}
	encoded, _ := rules.Encode(store)
	service := &Service{cfg: config.Config{ProxyPolicyGroup: "Proxy"}}
	overwrite := []byte(service.personalOverwrite(store))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(overwrite) }))
	defer server.Close()
	repo := &mutableRepository{head: "rev1", rawURL: server.URL, files: map[string][]byte{personalPath: encoded, "openclash/personal-overwrite.ini": overwrite}}
	service.repo = repo
	report := service.SelfCheck(context.Background())
	if report.Failed != 0 || report.Passed < 5 {
		t.Fatalf("unexpected self-check: %+v", report)
	}
}

func TestPreflightPreservesKnownWritePermissionOnTransientFailure(t *testing.T) {
	repo := &mutableRepository{access: &repository.AccessReport{Provider: "test", Error: "temporary EOF", Transient: true}}
	s := &Service{repo: repo, access: repository.AccessReport{Provider: "test", Authenticated: true, Readable: true, Writable: true, Public: true, User: "owner", Branch: "main", Revision: "rev1"}}
	report := s.Preflight(context.Background())
	if !report.Writable || !report.Authenticated || report.User != "owner" || report.Revision != "rev1" {
		t.Fatalf("transient preflight discarded known capabilities: %+v", report)
	}
}

func TestPreflightKnownPermissionFailureEntersReadOnly(t *testing.T) {
	repo := &mutableRepository{access: &repository.AccessReport{Provider: "test", Authenticated: true, Readable: true, Writable: false, Public: false, Branch: "main", Revision: "rev1"}}
	s := &Service{repo: repo}
	report := s.Preflight(context.Background())
	if report.Writable || !s.ReadOnly() || !strings.Contains(report.Error, "没有写权限") {
		t.Fatalf("permission failure did not enter explained read-only mode: %+v", report)
	}
}

func TestQueueIsIsolatedByUser(t *testing.T) {
	stateDB, err := runtimestate.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer stateDB.Close()
	s := &Service{state: stateDB}
	for _, item := range []runtimestate.Mutation{
		{ID: "user-a", Operation: "remove", Domain: "a.example", UserID: 100, ChatID: 100},
		{ID: "user-b", Operation: "remove", Domain: "b.example", UserID: 200, ChatID: 200},
	} {
		if err := stateDB.Enqueue(item, 10); err != nil {
			t.Fatal(err)
		}
	}
	items, err := s.QueueForUser(100)
	if err != nil || len(items) != 1 || items[0].ID != "user-a" {
		t.Fatalf("unexpected per-user queue: %+v err=%v", items, err)
	}
	if err := s.CancelQueued("user-b", 100); err == nil {
		t.Fatal("user should not be able to cancel another user's mutation")
	}
	if err := s.CancelQueued("user-a", 100); err != nil {
		t.Fatalf("owner could not cancel mutation: %v", err)
	}
}

func TestQueueMaintainsFIFOForSameDomainAndContinuesOtherDomains(t *testing.T) {
	empty, _ := rules.Encode(rules.Empty())
	repo := &mutableRepository{head: "rev1", files: map[string][]byte{personalPath: empty}}
	stateDB, err := runtimestate.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer stateDB.Close()
	now := time.Now().UTC()
	items := []runtimestate.Mutation{
		{ID: "same-first", Operation: "add", Rule: rules.Rule{Domain: "same.example", Match: rules.Exact, Action: rules.Direct}, CreatedAt: now, Status: "pending", NextAttempt: now.Add(time.Hour)},
		{ID: "same-second", Operation: "add", Rule: rules.Rule{Domain: "same.example", Match: rules.Exact, Action: rules.Proxy}, CreatedAt: now.Add(time.Millisecond), Status: "pending"},
		{ID: "other", Operation: "add", Rule: rules.Rule{Domain: "other.example", Match: rules.Exact, Action: rules.Proxy}, CreatedAt: now.Add(2 * time.Millisecond), Status: "pending"},
	}
	for _, item := range items {
		if err := stateDB.Enqueue(item, 10); err != nil {
			t.Fatal(err)
		}
	}
	s := &Service{cfg: config.Config{ProxyPolicyGroup: "Proxy", MutationRetryInterval: time.Second}, repo: repo, state: stateDB, access: repository.AccessReport{Writable: true, Readable: true}}
	if err := s.ProcessQueue(context.Background()); err != nil {
		t.Fatal(err)
	}
	remaining, err := s.Queue()
	if err != nil || len(remaining) != 2 || remaining[0].ID != "same-first" || remaining[1].ID != "same-second" {
		t.Fatalf("same-domain FIFO was not preserved: %+v err=%v", remaining, err)
	}
	stored, err := rules.Decode(repo.files[personalPath])
	if err != nil || len(stored.Rules) != 1 || stored.Rules[0].Domain != "other.example" {
		t.Fatalf("independent domain did not continue: %+v err=%v", stored, err)
	}
}

func TestSyncCoalescesConcurrentCallsAndSurvivesCallerCancellation(t *testing.T) {
	var calls atomic.Int32
	started := make(chan struct{})
	release := make(chan struct{})
	s := &Service{cfg: config.Config{SyncTimeout: time.Second}}
	s.syncTask = func(ctx context.Context) (Result, error) {
		if calls.Add(1) == 1 {
			close(started)
		}
		select {
		case <-ctx.Done():
			return Result{}, ctx.Err()
		case <-release:
			return Result{IndexChanged: true}, nil
		}
	}
	firstCtx, cancelFirst := context.WithCancel(context.Background())
	firstDone := make(chan error, 1)
	secondDone := make(chan error, 1)
	go func() { _, err := s.SyncWithSource(firstCtx, "manual"); firstDone <- err }()
	<-started
	go func() { _, err := s.SyncWithSource(context.Background(), "cron"); secondDone <- err }()
	time.Sleep(20 * time.Millisecond)
	cancelFirst()
	if err := <-firstDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("first caller should return its cancellation: %v", err)
	}
	close(release)
	if err := <-secondDone; err != nil {
		t.Fatalf("shared sync should continue for second caller: %v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("expected one real sync, got %d", calls.Load())
	}
}

func TestSyncTimeoutCancelsRealTask(t *testing.T) {
	s := &Service{cfg: config.Config{SyncTimeout: 20 * time.Millisecond}}
	s.syncTask = func(ctx context.Context) (Result, error) {
		<-ctx.Done()
		return Result{}, ctx.Err()
	}
	_, err := s.SyncWithSource(context.Background(), "manual")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected sync timeout, got %v", err)
	}
	status := s.SyncStatus()
	if status.Running || status.Stage != "失败" || status.LastError == "" {
		t.Fatalf("unexpected sync status after timeout: %+v", status)
	}
}
