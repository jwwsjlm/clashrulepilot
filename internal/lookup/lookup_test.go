package lookup

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestFirstString(t *testing.T) {
	if got := firstString(map[string]any{"country_code": "CN"}, "countryCode", "country_code"); got != "CN" {
		t.Fatalf("got %q", got)
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

	inspector := New(server.URL + "/{ip}")
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

	inspector := New(server.URL + "/{ip}")
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
