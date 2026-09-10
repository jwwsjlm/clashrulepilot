package gitlab

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"clashrulepilot/internal/repository"
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

func TestCommitChangesReadsFilesAtExpectedRevision(t *testing.T) {
	var received map[string]any
	var fileRef string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/repository/branches/main"):
			_ = json.NewEncoder(w).Encode(map[string]any{"name": "main", "commit": map[string]any{"id": "oldrev"}})
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/repository/files/"):
			fileRef = r.URL.Query().Get("ref")
			_ = json.NewEncoder(w).Encode(map[string]any{"content": "b2xk", "encoding": "base64", "last_commit_id": "file-at-oldrev"})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/repository/commits"):
			if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
				t.Fatal(err)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "newrev"})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.String())
		}
	}))
	defer server.Close()
	c, err := New(server.URL, "token", "group/rules", "main")
	if err != nil {
		t.Fatal(err)
	}
	sha, err := c.CommitChanges(t.Context(), map[string]repository.FileChange{"a.yaml": {Content: []byte("new")}}, "test", "oldrev")
	if err != nil || sha != "newrev" {
		t.Fatalf("sha=%q err=%v", sha, err)
	}
	if fileRef != "oldrev" {
		t.Fatalf("file existence was read from %q instead of expected revision", fileRef)
	}
	actions := received["actions"].([]any)
	action := actions[0].(map[string]any)
	if action["action"] != "update" || action["last_commit_id"] != "file-at-oldrev" {
		t.Fatalf("unexpected action from expected revision: %#v", action)
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
