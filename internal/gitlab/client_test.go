package gitlab

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCommitFilesUsesAtomicActions(t *testing.T) {
	var received map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("PRIVATE-TOKEN") != "token" {
			t.Errorf("token header missing")
		}
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/repository/files/"):
			http.Error(w, `{"message":"404 File Not Found"}`, http.StatusNotFound)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/repository/commits"):
			if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
				t.Fatal(err)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "abcdef123456"})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()
	c, err := New(server.URL, "token", "group/rules", "main")
	if err != nil {
		t.Fatal(err)
	}
	sha, err := c.CommitFiles(t.Context(), map[string][]byte{"a.yaml": []byte("a"), "b.yaml": []byte("b")}, "test")
	if err != nil || sha != "abcdef123456" {
		t.Fatalf("sha=%q err=%v", sha, err)
	}
	actions, ok := received["actions"].([]any)
	if !ok || len(actions) != 2 {
		t.Fatalf("actions=%#v", received["actions"])
	}
	if received["branch"] != "main" || received["commit_message"] != "test" {
		t.Fatalf("payload=%#v", received)
	}
}

func TestRawURLSupportsSelfHostedGitLab(t *testing.T) {
	c, err := New("https://gitlab.example.com/", "token", "group/rules", "main")
	if err != nil {
		t.Fatal(err)
	}
	if got := c.RawURL("openclash/personal-overwrite.ini"); got != "https://gitlab.example.com/group/rules/-/raw/main/openclash/personal-overwrite.ini" {
		t.Fatalf("got %q", got)
	}
}
