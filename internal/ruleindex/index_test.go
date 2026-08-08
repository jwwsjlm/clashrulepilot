package ruleindex

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	gh "github.com/google/go-github/v81/github"
	bolt "go.etcd.io/bbolt"
)

func TestParseAndQuery(t *testing.T) {
	direct, err := parseClassical([]byte("payload:\n  - DOMAIN,exact.example.com\n  - DOMAIN-SUFFIX,example.org\n  - DOMAIN-KEYWORD,m-team\n  - IP-CIDR,1.2.3.4/32\n"), Direct, "direct.yaml")
	if err != nil {
		t.Fatal(err)
	}
	geo, err := parseClassical([]byte("payload:\n  - DOMAIN-SUFFIX,cn.example\n"), GeoSite, "cn.yaml")
	if err != nil {
		t.Fatal(err)
	}
	gfw, err := parseClassical([]byte("payload:\n  - DOMAIN-SUFFIX,linux.do\n"), GeoSiteGFW, "gfw.yaml")
	if err != nil {
		t.Fatal(err)
	}
	m := testManagerWithEntries(t, append(append(direct, geo...), gfw...))
	cases := map[string]Action{"exact.example.com": Direct, "sub.example.org": Direct, "foo-m-team.net": Direct, "www.cn.example": GeoSite, "linux.do": GeoSiteGFW}
	for domain, action := range cases {
		got := m.Query(domain)
		if len(got) == 0 || got[0].Action != action {
			t.Fatalf("%s: %#v", domain, got)
		}
	}
}

func TestParseDomainBehavior(t *testing.T) {
	entries, err := parseDomainBehavior([]byte("payload:\n  - '+.example.com'\n  - '.suffix.test'\n  - '*m-team*'\n  - 'exact.example.net'\n"), Category, "Category_Domain.yaml")
	if err != nil {
		t.Fatal(err)
	}
	want := []Entry{
		{Kind: "DOMAIN-SUFFIX", Pattern: "example.com", Action: Category, Source: "Category_Domain.yaml"},
		{Kind: "DOMAIN-SUFFIX", Pattern: "suffix.test", Action: Category, Source: "Category_Domain.yaml"},
		{Kind: "DOMAIN-KEYWORD", Pattern: "m-team", Action: Category, Source: "Category_Domain.yaml"},
		{Kind: "DOMAIN", Pattern: "exact.example.net", Action: Category, Source: "Category_Domain.yaml"},
	}
	if fmt.Sprint(entries) != fmt.Sprint(want) {
		t.Fatalf("got %#v want %#v", entries, want)
	}
}

func TestSyncRebuildsFromLatestRemoteFileList(t *testing.T) {
	var removed atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/Aethersailor/Custom_OpenClash_Rules/contents/rule":
			w.Header().Set("Content-Type", "application/json")
			items := fmt.Sprintf(`[{"type":"file","name":"Custom_Direct_Domain.yaml","sha":"direct-v1","download_url":%q}`, serverURL(r)+"/raw/direct")
			if !removed.Load() {
				items += fmt.Sprintf(`,{"type":"file","name":"Encrypted_DNS_Domain.yaml","sha":"dns-v1","download_url":%q}`, serverURL(r)+"/raw/dns")
			}
			_, _ = fmt.Fprint(w, items+"]")
		case "/raw/direct":
			_, _ = fmt.Fprint(w, "payload:\n  - '+.direct.example'\n")
		case "/raw/dns":
			_, _ = fmt.Fprint(w, "payload:\n  - '+.dns.example'\n")
		case "/raw/geosite":
			_, _ = fmt.Fprint(w, "payload:\n  - DOMAIN-SUFFIX,cn.example\n")
		case "/raw/gfw":
			_, _ = fmt.Fprint(w, "payload:\n  - DOMAIN-SUFFIX,linux.do\n")
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	dir := t.TempDir()
	m := New(true, dir, "Aethersailor/Custom_OpenClash_Rules", "main", "")
	t.Cleanup(func() { _ = m.Close() })
	api := gh.NewClient(server.Client())
	api.BaseURL, _ = url.Parse(server.URL + "/")
	m.github = api
	m.http = server.Client()
	m.geositeURL = server.URL + "/raw/geosite"
	m.gfwURL = server.URL + "/raw/gfw"

	changed, err := m.Sync(context.Background())
	if err != nil || !changed {
		t.Fatalf("first sync changed=%v err=%v", changed, err)
	}
	if got := m.Query("sub.dns.example"); len(got) != 1 || got[0].Source != "Encrypted_DNS_Domain.yaml" {
		t.Fatalf("expected category source match, got %#v", got)
	}
	removed.Store(true)
	changed, err = m.Sync(context.Background())
	if err != nil || !changed {
		t.Fatalf("second sync changed=%v err=%v", changed, err)
	}
	if got := m.Query("sub.dns.example"); len(got) != 0 {
		t.Fatalf("deleted remote file survived new snapshot: %#v", got)
	}
	if got := m.Query("sub.direct.example"); len(got) != 1 || got[0].Action != Direct {
		t.Fatalf("direct rule missing after rebuild: %#v", got)
	}
	status := m.Status()
	if status.Sources != 3 { // current direct file + GEOSITE:CN + GEOSITE:GFW
		t.Fatalf("unexpected source count: %+v", status)
	}
	if _, err := os.Stat(filepath.Join(dir, "upstream", "Encrypted_DNS_Domain.yaml")); !os.IsNotExist(err) {
		t.Fatalf("deleted remote source file survived: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "upstream-index.db")); err != nil {
		t.Fatalf("disk database missing: %v", err)
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	reloaded := New(true, dir, "Aethersailor/Custom_OpenClash_Rules", "main", "")
	t.Cleanup(func() { _ = reloaded.Close() })
	if err := reloaded.Load(); err != nil {
		t.Fatalf("reload disk database: %v", err)
	}
	if got := reloaded.Query("sub.direct.example"); len(got) != 1 || got[0].Source != "Custom_Direct_Domain.yaml" {
		t.Fatalf("disk query after restart failed: %#v", got)
	}
}

func TestLoadCleansOnlyKnownOldSnapshots(t *testing.T) {
	dir := t.TempDir()
	old := filepath.Join(dir, "upstream-index-20260808.json.gz")
	tmp := filepath.Join(dir, "upstream-index.json.gz.tmp")
	keep := filepath.Join(dir, "user-data.json.gz")
	for _, name := range []string{old, tmp, keep} {
		if err := os.WriteFile(name, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	m := New(true, dir, "Aethersailor/Custom_OpenClash_Rules", "main", "")
	t.Cleanup(func() { _ = m.Close() })
	if err := m.Load(); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{old, tmp} {
		if _, err := os.Stat(name); !os.IsNotExist(err) {
			t.Fatalf("stale file not removed: %s", name)
		}
	}
	if _, err := os.Stat(keep); err != nil {
		t.Fatalf("unrelated file should remain: %v", err)
	}
}

func serverURL(r *http.Request) string {
	return "http://" + strings.TrimSuffix(r.Host, "/")
}

func TestLiveIndexSync(t *testing.T) {
	if os.Getenv("CLASHRULEPILOT_LIVE_TEST") == "" {
		t.Skip("set CLASHRULEPILOT_LIVE_TEST=1 to test live rule sources")
	}
	m := New(true, t.TempDir(), "Aethersailor/Custom_OpenClash_Rules", "main", os.Getenv("GITHUB_TOKEN"))
	t.Cleanup(func() { _ = m.Close() })
	if changed, err := m.Sync(context.Background()); err != nil || !changed {
		t.Fatalf("changed=%v err=%v status=%+v", changed, err, m.Status())
	}
	if got := m.Query("testingcf.jsdelivr.net"); len(got) == 0 {
		t.Fatal("expected testingcf.jsdelivr.net to match an Aethersailor domain rule")
	}
	if got := m.Query("www.baidu.com"); len(got) == 0 {
		t.Fatal("expected www.baidu.com to match GEOSITE:CN")
	}
	if got := m.Query("linux.do"); len(got) == 0 || got[0].Action != GeoSiteGFW {
		t.Fatal("expected linux.do to match GEOSITE:GFW")
	}
}

func testManagerWithEntries(t *testing.T, entries []Entry) *Manager {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	db, err := bolt.Open(path, 0o600, nil)
	if err != nil {
		t.Fatal(err)
	}
	metadata := diskMetadata{Version: 3, UpdatedAt: time.Now().UTC(), Sources: map[string]sourceMeta{"test": {SHA256: "test"}}}
	if err := db.Update(func(tx *bolt.Tx) error {
		for _, name := range [][]byte{bucketExact, bucketSuffix, bucketKeyword, bucketMetadata} {
			if _, err := tx.CreateBucketIfNotExists(name); err != nil {
				return err
			}
		}
		if err := storeEntries(tx, entries); err != nil {
			return err
		}
		encoded, err := json.Marshal(metadata)
		if err != nil {
			return err
		}
		return tx.Bucket(bucketMetadata).Put(metadataKey, encoded)
	}); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	readDB, loaded, err := openReadDatabase(path)
	if err != nil {
		t.Fatal(err)
	}
	m := &Manager{enabled: true, db: readDB, metadata: loaded, status: Status{Enabled: true, Loaded: true}}
	t.Cleanup(func() { _ = m.Close() })
	return m
}
