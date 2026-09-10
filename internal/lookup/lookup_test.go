package lookup

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestFirstString(t *testing.T) {
	if got := firstString(map[string]any{"country_code": "CN"}, "countryCode", "country_code"); got != "CN" {
		t.Fatalf("got %q", got)
	}
}

func TestInspectCoalescesConcurrentDomainLookups(t *testing.T) {
	inspector := New("", DoHConfig{QueryTimeout: 2 * time.Second})
	var calls atomic.Int32
	inspector.lookupIP = func(context.Context, string) ([]net.IPAddr, error) {
		calls.Add(1)
		time.Sleep(80 * time.Millisecond)
		return []net.IPAddr{{IP: net.ParseIP("8.8.8.8")}}, nil
	}
	start := make(chan struct{})
	results := make(chan Report, 12)
	for range 12 {
		go func() { <-start; results <- inspector.Inspect(context.Background(), "EXAMPLE.com.") }()
	}
	close(start)
	shared := 0
	for range 12 {
		if (<-results).Shared {
			shared++
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("expected one shared lookup, got %d", calls.Load())
	}
	if shared == 0 {
		t.Fatal("expected shared results to be marked")
	}
}

func TestIsChinaWithReq(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/1.2.3.4" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true,"country_code":"CN"}`))
	}))
	defer server.Close()

	inspector := New(server.URL+"/{ip}", DoHConfig{})
	china, err := inspector.isChina(context.Background(), "1.2.3.4")
	if err != nil {
		t.Fatal(err)
	}
	if !china {
		t.Fatal("expected China result")
	}
	server.Close()
	china, err = inspector.isChina(context.Background(), "1.2.3.4")
	if err != nil || !china {
		t.Fatalf("expected cached result, china=%v err=%v", china, err)
	}
}

func TestLookupGeoKeepsLocationAndNetworkDetails(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true,"country":"Singapore","country_code":"SG","region":"Central Singapore","city":"Singapore","connection":{"isp":"Example ISP","org":"Example Org","asn":13335}}`))
	}))
	defer server.Close()

	inspector := New(server.URL+"/{ip}", DoHConfig{})
	info, err := inspector.lookupGeo(context.Background(), "84.17.37.214")
	if err != nil {
		t.Fatal(err)
	}
	if info.IP != "84.17.37.214" || info.Country != "Singapore" || info.CountryCode != "SG" || info.City != "Singapore" {
		t.Fatalf("location fields missing: %+v", info)
	}
	if info.ISP != "Example ISP" || info.Org != "Example Org" || info.ASN != "13335" || info.China {
		t.Fatalf("network fields missing: %+v", info)
	}
}

func TestIsFakeIP(t *testing.T) {
	cases := map[string]bool{
		"198.18.0.96":          true,
		"198.19.255.255":       true,
		"198.20.0.1":           false,
		"fdfe:dcba:9876::1234": true,
		"2400:3200::1":         false,
		"not-an-ip":            false,
	}
	for value, want := range cases {
		if got := isFakeIP(value); got != want {
			t.Fatalf("isFakeIP(%q)=%v want %v", value, got, want)
		}
	}
}

func TestFakeIPSkipsGeoIPRequest(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		_, _ = w.Write([]byte(`{"success":true,"country_code":"CN"}`))
	}))
	defer server.Close()

	inspector := New(server.URL+"/{ip}", DoHConfig{})
	report := inspector.inspectAddresses(context.Background(), "linux.do", []net.IPAddr{{IP: net.ParseIP("198.18.0.96")}})
	if requests.Load() != 0 {
		t.Fatalf("Fake-IP unexpectedly triggered %d GeoIP requests", requests.Load())
	}
	if len(report.FakeIP) != 1 || report.FakeIP[0] != "198.18.0.96" {
		t.Fatalf("unexpected Fake-IP report: %+v", report)
	}
	if report.GeoError != "" || report.ChinaChecked != 0 {
		t.Fatalf("Fake-IP should be inconclusive, got %+v", report)
	}
}

func TestDisabledDNSGroupsKeepLocalAddresses(t *testing.T) {
	inspector := New("", DoHConfig{
		Domestic: DNSGroupConfig{Enabled: false, Endpoints: []string{"https://domestic.example/dns-query"}},
		Foreign:  DNSGroupConfig{Enabled: false, Endpoints: []string{"https://foreign.example/dns-query"}},
	})
	report := inspector.inspectAddresses(context.Background(), "example.com", []net.IPAddr{{IP: net.ParseIP("8.8.8.8")}})
	if report.DNSSource != "local" || len(report.A) != 1 || report.A[0] != "8.8.8.8" {
		t.Fatalf("disabled DNS groups discarded local resolution: %+v", report)
	}
}

func TestFakeIPFallsBackToDoHAndGeoIP(t *testing.T) {
	var dohRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/dns-query":
			dohRequests.Add(1)
			w.Header().Set("Content-Type", "application/dns-json")
			if r.URL.Query().Get("type") == "A" {
				_, _ = w.Write([]byte(`{"Status":0,"Answer":[{"name":"gw2.oops.asia.","type":5,"TTL":120,"data":"edge.example."},{"name":"edge.example.","type":1,"TTL":120,"data":"1.2.3.4"}]}`))
			} else {
				_, _ = w.Write([]byte(`{"Status":0,"Answer":[{"name":"gw2.oops.asia.","type":28,"TTL":120,"data":"2001:db8::1"}]}`))
			}
		case strings.HasPrefix(r.URL.Path, "/geo/"):
			_, _ = w.Write([]byte(`{"success":true,"country_code":"CN"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	inspector := New(server.URL+"/geo/{ip}", DoHConfig{Enabled: true, Endpoints: []string{server.URL + "/dns-query"}, Timeout: time.Second, CacheSize: 16})
	report := inspector.inspectAddresses(context.Background(), "gw2.oops.asia", []net.IPAddr{{IP: net.ParseIP("198.18.0.96")}})
	if report.DNSSource != "doh" || report.DoHProvider == "" || len(report.A) != 1 || len(report.AAAA) != 1 {
		t.Fatalf("unexpected DoH report: %+v", report)
	}
	if !report.China || report.ChinaChecked != 2 {
		t.Fatalf("real DoH IPs were not passed to GeoIP: %+v", report)
	}
	if dohRequests.Load() != 2 {
		t.Fatalf("expected A and AAAA requests, got %d", dohRequests.Load())
	}
	report = inspector.inspectAddresses(context.Background(), "gw2.oops.asia", []net.IPAddr{{IP: net.ParseIP("198.18.0.96")}})
	if !report.DoHCacheHit || dohRequests.Load() != 2 {
		t.Fatalf("expected cached DoH response: %+v requests=%d", report, dohRequests.Load())
	}
}

func TestDoHFallsBackToNextProvider(t *testing.T) {
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "no", http.StatusBadGateway) }))
	defer bad.Close()
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("type") == "A" {
			_, _ = w.Write([]byte(`{"Status":0,"Answer":[{"type":1,"TTL":60,"data":"8.8.8.8"}]}`))
		} else {
			_, _ = w.Write([]byte(`{"Status":0,"Answer":[]}`))
		}
	}))
	defer good.Close()
	inspector := New("", DoHConfig{Enabled: true, Endpoints: []string{bad.URL + "/dns-query", good.URL + "/resolve"}, Timeout: time.Second, CacheSize: 16})
	report := inspector.inspectAddresses(context.Background(), "example.com", []net.IPAddr{{IP: net.ParseIP("198.18.0.2")}})
	if report.DNSSource != "doh" || len(report.A) != 1 || report.A[0] != "8.8.8.8" {
		t.Fatalf("fallback failed: %+v", report)
	}
}

func TestRealLocalDNSDoesNotCallDoH(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1) }))
	defer server.Close()
	inspector := New("", DoHConfig{Enabled: true, Endpoints: []string{server.URL}, Timeout: time.Second, CacheSize: 16})
	report := inspector.inspectAddresses(context.Background(), "example.com", []net.IPAddr{{IP: net.ParseIP("93.184.216.34")}})
	if report.DNSSource != "local" || requests.Load() != 0 {
		t.Fatalf("real local DNS should bypass DoH: %+v requests=%d", report, requests.Load())
	}
}

func TestDualDNSGroupsRunInParallelAndKeepSources(t *testing.T) {
	var domesticCalls, foreignCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/geo/") {
			_, _ = w.Write([]byte(`{"success":true,"country":"United States","country_code":"US","connection":{"isp":"Example ISP","asn":"AS13335"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/dns-json")
		if strings.HasPrefix(r.URL.Path, "/domestic") {
			domesticCalls.Add(1)
			if r.URL.Query().Get("type") == "A" {
				_, _ = w.Write([]byte(`{"Status":0,"Answer":[{"type":1,"TTL":60,"data":"1.1.1.1"}]}`))
			} else {
				_, _ = w.Write([]byte(`{"Status":0,"Answer":[]}`))
			}
			return
		}
		foreignCalls.Add(1)
		if r.URL.Query().Get("type") == "A" {
			_, _ = w.Write([]byte(`{"Status":0,"Answer":[{"type":1,"TTL":60,"data":"8.8.8.8"}]}`))
		} else {
			_, _ = w.Write([]byte(`{"Status":0,"Answer":[]}`))
		}
	}))
	defer server.Close()

	inspector := New(server.URL+"/geo/{ip}", DoHConfig{
		Domestic: DNSGroupConfig{Enabled: true, Endpoints: []string{server.URL + "/domestic"}},
		Foreign:  DNSGroupConfig{Enabled: true, Endpoints: []string{server.URL + "/foreign"}},
		Timeout:  time.Second, CacheSize: 16,
	})
	report := inspector.inspectAddresses(context.Background(), "example.com", []net.IPAddr{{IP: net.ParseIP("198.18.0.2")}})
	if report.DNSSource != "dual" || len(report.Domestic.A) != 1 || report.Domestic.A[0] != "1.1.1.1" || len(report.Foreign.A) != 1 || report.Foreign.A[0] != "8.8.8.8" {
		t.Fatalf("unexpected dual report: %+v", report)
	}
	if domesticCalls.Load() != 2 || foreignCalls.Load() != 2 {
		t.Fatalf("expected A and AAAA for both groups, domestic=%d foreign=%d", domesticCalls.Load(), foreignCalls.Load())
	}
	if len(report.GeoIPs) != 2 || report.ChinaChecked != 2 || report.China {
		t.Fatalf("unexpected GeoIP aggregation: %+v", report)
	}
}

func TestLiveDoH(t *testing.T) {
	if os.Getenv("CLASHRULEPILOT_LIVE_TEST") == "" {
		t.Skip("set CLASHRULEPILOT_LIVE_TEST=1 to test public DoH")
	}
	inspector := New("", DoHConfig{
		Enabled: true,
		Endpoints: []string{
			"https://cloudflare-dns.com/dns-query",
			"https://dns.google/resolve",
		},
		Timeout:   5 * time.Second,
		CacheSize: 16,
	})
	report := inspector.inspectAddresses(context.Background(), "gw2.oops.asia", []net.IPAddr{{IP: net.ParseIP("198.18.0.96")}})
	if report.DNSSource != "doh" || len(report.A)+len(report.AAAA) == 0 {
		t.Fatalf("live DoH failed: %+v", report)
	}
}

func TestLiveDualDNS(t *testing.T) {
	if os.Getenv("CLASHRULEPILOT_LIVE_TEST") == "" {
		t.Skip("set CLASHRULEPILOT_LIVE_TEST=1 to test public dual DNS")
	}
	inspector := New("", DoHConfig{
		Domestic:  DNSGroupConfig{Enabled: true, Endpoints: []string{"https://dns.alidns.com/resolve", "https://doh.pub/dns-query"}},
		Foreign:   DNSGroupConfig{Enabled: true, Endpoints: []string{"https://cloudflare-dns.com/dns-query", "https://dns.google/resolve"}},
		Timeout:   5 * time.Second,
		CacheSize: 16,
	})
	report := inspector.inspectAddresses(context.Background(), "legendsen.se", []net.IPAddr{{IP: net.ParseIP("198.18.0.96")}})
	if report.DNSSource != "dual" || len(report.Domestic.A)+len(report.Domestic.AAAA) == 0 || len(report.Foreign.A)+len(report.Foreign.AAAA) == 0 {
		t.Fatalf("live dual DNS failed: %+v", report)
	}
}
