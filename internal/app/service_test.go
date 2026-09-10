package app

import (
	"context"
	"errors"
	"io"
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
	"clashrulepilot/internal/ruleindex"
	"clashrulepilot/internal/rules"
	runtimestate "clashrulepilot/internal/state"
	"clashrulepilot/internal/syncer"
	"github.com/goccy/go-yaml"
)

type mutableRepository struct {
	mu        sync.Mutex
	head      string
	files     map[string][]byte
	err       error
	commitErr error
	rawURL    string
	commits   int
	access    *repository.AccessReport
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
	changes := make(map[string]repository.FileChange, len(files))
	for path, content := range files {
		changes[path] = repository.FileChange{Content: content}
	}
	expectedRevision := ""
	if len(expected) > 0 {
		expectedRevision = expected[0]
	}
	return r.CommitChanges(context.Background(), changes, "", expectedRevision)
}
func (r *mutableRepository) CommitChanges(_ context.Context, changes map[string]repository.FileChange, _ string, expected string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return "", r.err
	}
	if r.commitErr != nil {
		return "", r.commitErr
	}
	if expected != "" && expected != r.head {
		return "", repository.ErrConflict
	}
	r.commits++
	r.head = "commit" + string(rune('0'+r.commits))
	if r.files == nil {
		r.files = make(map[string][]byte)
	}
	for path, change := range changes {
		if change.Delete {
			delete(r.files, path)
			continue
		}
		r.files[path] = append([]byte(nil), change.Content...)
	}
	return r.head, nil
}
func (r *mutableRepository) ListFiles(_ context.Context, prefix, _ string) ([]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return nil, r.err
	}
	var paths []string
	for path := range r.files {
		if strings.HasPrefix(path, strings.Trim(prefix, "/")) {
			paths = append(paths, path)
		}
	}
	return paths, nil
}
func (r *mutableRepository) Owner() string  { return "owner" }
func (r *mutableRepository) Repo() string   { return "repo" }
func (r *mutableRepository) Branch() string { return "main" }
func (r *mutableRepository) WebURL() string { return "https://example.test/owner/repo" }
func (r *mutableRepository) RawURL(file string) string {
	base := r.rawURL
	if base == "" {
		base = "https://example.test/raw"
	}
	return strings.TrimRight(base, "/") + "/" + strings.TrimLeft(file, "/")
}
func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func testUpstreamClient(onContents func()) *syncer.Client {
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		body := ""
		switch {
		case strings.Contains(request.URL.Path, "/contents/rule"):
			if onContents != nil {
				onContents()
			}
			body = `[{"name":"Custom_Direct_Domain.yaml","sha":"upstream-file","download_url":"https://download.test/Custom_Direct_Domain.yaml","type":"file"}]`
		case request.URL.Host == "download.test":
			body = "payload:\n  - '+.upstream.example'\n"
		case strings.Contains(request.URL.Path, "/commits/"):
			body = `{"sha":"upstream-head"}`
		default:
			return &http.Response{StatusCode: http.StatusNotFound, Body: io.NopCloser(strings.NewReader("not found")), Header: make(http.Header), Request: request}, nil
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header), Request: request}, nil
	})
	return &syncer.Client{Repo: "owner/upstream", Branch: "main", HTTP: &http.Client{Transport: transport}}
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

func TestDesiredFilesPublishesClassicalProvidersAndLegacyAliases(t *testing.T) {
	store := rules.Store{Version: 1, Rules: []rules.Rule{
		{Domain: "direct.example", Match: rules.Suffix, Action: rules.Direct},
		{Domain: "proxy.example", Match: rules.Exact, Action: rules.Proxy},
	}}
	repo := &mutableRepository{rawURL: "https://gitlab.example/group/rules/-/raw/main"}
	service := &Service{cfg: config.Config{ProxyPolicyGroup: "Proxy"}, repo: repo}
	files, err := service.desiredFiles(context.Background(), &store, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	direct := files["rules/personal/My_Direct_Classical.yaml"]
	proxy := files["rules/personal/My_Proxy_Classical.yaml"]
	if len(direct) == 0 || len(proxy) == 0 {
		t.Fatalf("classical providers missing: %v", files)
	}
	if string(files["rules/personal/My_Direct_Domain.yaml"]) != string(direct) || string(files["rules/personal/My_Proxy_Domain.yaml"]) != string(proxy) {
		t.Fatal("legacy provider aliases differ from classical providers")
	}
	if string(files["rules/acl/Custom_Direct.list"]) != string(rules.RenderACL(store, rules.Direct)) || string(files["rules/acl/Custom_Proxy.list"]) != string(rules.RenderACL(store, rules.Proxy)) {
		t.Fatal("plain-text ACL files were not generated from personal_rules.json")
	}
	providers := string(files["openclash/providers.yaml"])
	for _, want := range []string{"My_Direct_Classical.yaml", "My_Proxy_Classical.yaml", "behavior: classical", "format: yaml"} {
		if !strings.Contains(providers, want) {
			t.Fatalf("providers fragment missing %q:\n%s", want, providers)
		}
	}
	if strings.Contains(providers, "My_Direct_Domain.yaml") || strings.Contains(providers, "My_Proxy_Domain.yaml") {
		t.Fatalf("providers fragment still uses legacy aliases:\n%s", providers)
	}
	script := string(files["openclash/openclash_custom_overwrite.sh"])
	for _, want := range []string{"TARGET_GROUP", "🚀 手动选择", "proxy-groups", "YAML.load_file", "exit 0"} {
		if !strings.Contains(script, want) {
			t.Fatalf("custom overwrite script missing %q:\n%s", want, script)
		}
	}
	acl := string(files["clash/Custom_Mihomo_Optimized.ini"])
	for _, want := range []string{
		"https://gitlab.example/group/rules/-/raw/main/rules/acl/Custom_Direct.list",
		"ruleset=🚀 手动选择,[]DOMAIN,proxy.example",
		"ruleset=🎯 全球直连,[]DOMAIN-SUFFIX,direct.example",
		"Aethersailor/Custom_OpenClash_Rules@main/rule/Custom_Direct_Domain.yaml",
		"custom_proxy_group=⚡ URLTest`url-test`.*`https://cp.cloudflare.com/generate_204`600,,50",
		"custom_proxy_group=♻️ 自动选择`select`[]⚡ URLTest",
		"custom_proxy_group=🤖 AI 服务`select`[]🇺🇸 美国节点",
		"custom_proxy_group=📹 YouTube`select`[]🇭🇰 香港节点",
		"custom_proxy_group=🎬 国际媒体`select`[]🇭🇰 香港节点",
		"custom_proxy_group=🌐 国外服务`select`[]🇭🇰 香港节点",
	} {
		if !strings.Contains(acl, want) {
			t.Fatalf("optimized ACL missing %q:\n%s", want, acl)
		}
	}
	for _, unwanted := range []string{"{{PUBLIC_RAW_BASE}}", "{{PERSONAL_ACL_RULESETS}}", "🤖 ChatGPT`select", "🎥 Netflix`select", "💳 PayPal`select", "🪙 加密货币`select"} {
		if strings.Contains(acl, unwanted) {
			t.Fatalf("optimized ACL still contains over-segmented or unresolved item %q:\n%s", unwanted, acl)
		}
	}
}

func TestOptimizedACLKeepsCrossActionSpecificRulesFirst(t *testing.T) {
	store := rules.Store{Version: 1, Rules: []rules.Rule{
		{Domain: "proxy-parent.example", Match: rules.Suffix, Action: rules.Proxy},
		{Domain: "api.proxy-parent.example", Match: rules.Exact, Action: rules.Direct},
		{Domain: "direct-parent.example", Match: rules.Suffix, Action: rules.Direct},
		{Domain: "api.direct-parent.example", Match: rules.Exact, Action: rules.Proxy},
	}}
	service := &Service{repo: &mutableRepository{rawURL: "https://example.test/raw"}}
	acl := string(service.customMihomoOptimized(store))
	for _, pair := range [][2]string{
		{"ruleset=🎯 全球直连,[]DOMAIN,api.proxy-parent.example", "ruleset=🚀 手动选择,[]DOMAIN-SUFFIX,proxy-parent.example"},
		{"ruleset=🚀 手动选择,[]DOMAIN,api.direct-parent.example", "ruleset=🎯 全球直连,[]DOMAIN-SUFFIX,direct-parent.example"},
	} {
		specific, broad := strings.Index(acl, pair[0]), strings.Index(acl, pair[1])
		if specific < 0 || broad < 0 || specific > broad {
			t.Fatalf("specific personal ACL rule must precede broad opposite action:\n%s", acl)
		}
	}
	order := service.ruleOrder(store, nil)
	if strings.Contains(order, "RULE-SET,my_direct") || strings.Contains(order, "RULE-SET,my_proxy") {
		t.Fatalf("OpenClash order must not split cross-action personal rules:\n%s", order)
	}
	if strings.Index(order, "DOMAIN,api.proxy-parent.example,DIRECT") > strings.Index(order, "DOMAIN-SUFFIX,proxy-parent.example,") {
		t.Fatalf("OpenClash order lost specific-rule precedence:\n%s", order)
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

func TestBootstrapReconcilesPublicMirror(t *testing.T) {
	privateAccess := repository.AccessReport{Provider: "test", Authenticated: true, Readable: true, Writable: true, Revision: "private-1"}
	publicAccess := repository.AccessReport{Provider: "test", Authenticated: true, Readable: true, Writable: true, Revision: "public-1"}
	privateRepo := &mutableRepository{head: "private-1", files: map[string][]byte{}, access: &privateAccess}
	publicRepo := &mutableRepository{head: "public-1", files: map[string][]byte{}, access: &publicAccess}
	stateDB, err := runtimestate.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer stateDB.Close()
	index := ruleindex.New(false, t.TempDir(), "", "", "")
	service := &Service{
		cfg:         config.Config{ProxyPolicyGroup: "Proxy", RuleRepoProvider: "test"},
		repo:        privateRepo,
		privateRepo: privateRepo,
		publicRepo:  publicRepo,
		state:       stateDB,
		index:       index,
	}
	if err := service.Bootstrap(context.Background()); err != nil {
		t.Fatal(err)
	}
	privateRepo.mu.Lock()
	if len(privateRepo.files[personalPath]) == 0 {
		privateRepo.mu.Unlock()
		t.Fatal("confirmed missing personal store was not initialized")
	}
	privateRepo.mu.Unlock()
	publicRepo.mu.Lock()
	defer publicRepo.mu.Unlock()
	for _, path := range []string{"rules/acl/Custom_Direct.list", "rules/acl/Custom_Proxy.list", "clash/Custom_Mihomo_Optimized.ini"} {
		if len(publicRepo.files[path]) == 0 {
			t.Fatalf("bootstrap did not publish %s", path)
		}
	}
}

func TestBootstrapDoesNotOverwriteMalformedRemoteStore(t *testing.T) {
	privateAccess := repository.AccessReport{Provider: "test", Authenticated: true, Readable: true, Writable: true, Revision: "private-1"}
	publicAccess := repository.AccessReport{Provider: "test", Authenticated: true, Readable: true, Writable: true, Revision: "public-1"}
	malformed := []byte(`{"version":1,"rules":[`)
	privateRepo := &mutableRepository{head: "private-1", files: map[string][]byte{personalPath: malformed}, access: &privateAccess}
	publicRepo := &mutableRepository{head: "public-1", files: map[string][]byte{}, access: &publicAccess}
	stateDB, err := runtimestate.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer stateDB.Close()
	service := &Service{
		cfg:         config.Config{ProxyPolicyGroup: "Proxy", RuleRepoProvider: "test"},
		repo:        privateRepo,
		privateRepo: privateRepo,
		publicRepo:  publicRepo,
		state:       stateDB,
		index:       ruleindex.New(false, t.TempDir(), "", "", ""),
	}
	if err := service.Bootstrap(context.Background()); err != nil {
		t.Fatal(err)
	}
	privateRepo.mu.Lock()
	defer privateRepo.mu.Unlock()
	if privateRepo.commits != 0 || string(privateRepo.files[personalPath]) != string(malformed) {
		t.Fatalf("malformed remote store was overwritten: commits=%d content=%q", privateRepo.commits, privateRepo.files[personalPath])
	}
	if !service.ReadOnly() {
		t.Fatal("service did not enter read-only mode without a valid personal rules cache")
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

func TestPublicPublishFailureIsMarkedAndRetryClearsIt(t *testing.T) {
	empty, _ := rules.Encode(rules.Empty())
	privateRepo := &mutableRepository{head: "private-1", files: map[string][]byte{personalPath: empty}}
	publicRepo := &mutableRepository{head: "public-1", files: map[string][]byte{}, commitErr: errors.New("temporary publish failure")}
	stateDB, err := runtimestate.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer stateDB.Close()
	service := &Service{
		cfg:         config.Config{ProxyPolicyGroup: "Proxy", MutationQueueLimit: 10, MutationRetryInterval: time.Second},
		repo:        privateRepo,
		privateRepo: privateRepo,
		publicRepo:  publicRepo,
		state:       stateDB,
		access:      repository.AccessReport{Writable: true, Readable: true},
	}
	result, err := service.AddRule(context.Background(), rules.Rule{Domain: "new.example", Match: rules.Exact, Action: rules.Proxy}, false)
	if err != nil {
		t.Fatal(err)
	}
	if !result.PublicPending || !service.publicPending.Load() {
		t.Fatalf("failed public publish was not marked pending: %+v", result)
	}
	publicRepo.mu.Lock()
	publicRepo.commitErr = nil
	publicRepo.mu.Unlock()
	if err := service.retryPublicPublish(context.Background()); err != nil {
		t.Fatal(err)
	}
	if service.publicPending.Load() {
		t.Fatal("successful background retry did not clear pending state")
	}
	publicRepo.mu.Lock()
	_, published := publicRepo.files["rules/acl/Custom_Proxy.list"]
	publicRepo.mu.Unlock()
	if !published {
		t.Fatal("public retry did not publish generated ACL files")
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
	service.repo = &mutableRepository{head: "rev1", rawURL: server.URL}
	direct := rules.RenderClassical(store, rules.Direct)
	proxy := rules.RenderClassical(store, rules.Proxy)
	providers := []byte(service.providers(nil))
	repo := service.repo.(*mutableRepository)
	repo.files = map[string][]byte{
		personalPath:                              encoded,
		"openclash/personal-overwrite.ini":        overwrite,
		"rules/personal/My_Direct_Classical.yaml": direct,
		"rules/personal/My_Proxy_Classical.yaml":  proxy,
		"openclash/providers.yaml":                providers,
		"rules/acl/Custom_Direct.list":            rules.RenderACL(store, rules.Direct),
		"rules/acl/Custom_Proxy.list":             rules.RenderACL(store, rules.Proxy),
		"clash/Custom_Mihomo_Optimized.ini":       service.customMihomoOptimized(store),
	}
	service.repo = repo
	report := service.SelfCheck(context.Background())
	if report.Failed != 0 || report.Passed < 5 {
		t.Fatalf("unexpected self-check: %+v", report)
	}
}

func TestSelfCheckDetectsACLDrift(t *testing.T) {
	store := rules.Store{Version: 1, Rules: []rules.Rule{{Domain: "example.com", Match: rules.Suffix, Action: rules.Direct}}}
	service := &Service{cfg: config.Config{ProxyPolicyGroup: "Proxy"}}
	overwrite := []byte(service.personalOverwrite(store))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(overwrite) }))
	defer server.Close()
	repo := &mutableRepository{head: "rev1", rawURL: server.URL}
	service.repo = repo
	files, err := service.desiredFiles(context.Background(), &store, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	repo.files = files
	repo.files["rules/acl/Custom_Direct.list"] = []byte("DOMAIN-SUFFIX,stale.example\n")
	report := service.SelfCheck(context.Background())
	found := false
	for _, item := range report.Items {
		if item.Level == "fail" && strings.Contains(item.Text, "自定义直连 ACL") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("ACL drift was not reported: %+v", report)
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

func TestSyncRefreshesPersonalRulesBeforeCommitting(t *testing.T) {
	oldStore, _ := rules.Encode(rules.Store{Version: 1, Rules: []rules.Rule{{Domain: "old.example", Match: rules.Exact, Action: rules.Direct}}})
	newStore, _ := rules.Encode(rules.Store{Version: 1, Rules: []rules.Rule{{Domain: "new.example", Match: rules.Exact, Action: rules.Proxy}}})
	privateRepo := &mutableRepository{head: "rev1", files: map[string][]byte{personalPath: oldStore}}
	stateDB, err := runtimestate.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer stateDB.Close()
	service := &Service{
		cfg:         config.Config{SyncUpstream: true, UpstreamRepo: "owner/upstream", UpstreamBranch: "main", ProxyPolicyGroup: "Proxy"},
		repo:        privateRepo,
		privateRepo: privateRepo,
		state:       stateDB,
		index:       ruleindex.New(false, t.TempDir(), "", "", ""),
		upstream:    testUpstreamClient(nil),
	}
	if err := service.RefreshStore(context.Background()); err != nil {
		t.Fatal(err)
	}
	privateRepo.mu.Lock()
	privateRepo.head = "rev2"
	privateRepo.files[personalPath] = newStore
	privateRepo.mu.Unlock()
	if _, err := service.syncOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	privateRepo.mu.Lock()
	committed := append([]byte(nil), privateRepo.files[personalPath]...)
	privateRepo.mu.Unlock()
	store, err := rules.Decode(committed)
	if err != nil || len(store.Rules) != 1 || store.Rules[0].Domain != "new.example" {
		t.Fatalf("sync committed stale personal rules: %+v err=%v", store, err)
	}
}

func TestSyncRejectsConcurrentPersonalRuleChange(t *testing.T) {
	base, _ := rules.Encode(rules.Store{Version: 1, Rules: []rules.Rule{{Domain: "base.example", Match: rules.Exact, Action: rules.Direct}}})
	external, _ := rules.Encode(rules.Store{Version: 1, Rules: []rules.Rule{{Domain: "external.example", Match: rules.Exact, Action: rules.Proxy}}})
	privateRepo := &mutableRepository{head: "rev1", files: map[string][]byte{personalPath: base}}
	stateDB, err := runtimestate.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer stateDB.Close()
	service := &Service{
		cfg:         config.Config{SyncUpstream: true, UpstreamRepo: "owner/upstream", UpstreamBranch: "main", ProxyPolicyGroup: "Proxy"},
		repo:        privateRepo,
		privateRepo: privateRepo,
		state:       stateDB,
		index:       ruleindex.New(false, t.TempDir(), "", "", ""),
	}
	service.upstream = testUpstreamClient(func() {
		privateRepo.mu.Lock()
		privateRepo.head = "rev2"
		privateRepo.files[personalPath] = external
		privateRepo.mu.Unlock()
	})
	if err := service.RefreshStore(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := service.syncOnce(context.Background()); !errors.Is(err, repository.ErrConflict) {
		t.Fatalf("expected revision conflict, got %v", err)
	}
	privateRepo.mu.Lock()
	committed := append([]byte(nil), privateRepo.files[personalPath]...)
	privateRepo.mu.Unlock()
	if string(committed) != string(external) {
		t.Fatal("sync overwrote the concurrent personal rule change")
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
