package ruleindex

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	gh "github.com/google/go-github/v81/github"
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
	c := compile(diskSnapshot{Version: 2, Entries: append(direct, geo...)})
	m := &Manager{}
	m.current.Store(c)
	cases := map[string]Action{"exact.example.com": Direct, "sub.example.org": Direct, "foo-m-team.net": Direct, "www.cn.example": GeoSite}
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
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	dir := t.TempDir()
	m := New(true, dir, "Aethersailor/Custom_OpenClash_Rules", "main", "")
	api := gh.NewClient(server.Client())
	api.BaseURL, _ = url.Parse(server.URL + "/")
	m.github = api
	m.http = server.Client()
	m.geositeURL = server.URL + "/raw/geosite"

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
	if status.Sources != 2 { // current direct file + GEOSITE:CN
		t.Fatalf("unexpected source count: %+v", status)
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
	if changed, err := m.Sync(context.Background()); err != nil || !changed {
		t.Fatalf("changed=%v err=%v status=%+v", changed, err, m.Status())
	}
	if got := m.Query("testingcf.jsdelivr.net"); len(got) == 0 {
		t.Fatal("expected testingcf.jsdelivr.net to match an Aethersailor domain rule")
	}
	if got := m.Query("www.baidu.com"); len(got) == 0 {
		t.Fatal("expected www.baidu.com to match GEOSITE:CN")
	}
}
