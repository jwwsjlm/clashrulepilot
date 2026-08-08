package lookup

import (
	"context"
	"net/http"
	"net/http/httptest"
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
