package state

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"clashrulepilot/internal/rules"
)

func TestLegacyCacheMigrationAndQueuePersistence(t *testing.T) {
	dir := t.TempDir()
	legacy := filepath.Join(dir, "personal_rules.cache.json")
	encoded, _ := rules.Encode(rules.Store{Version: 1, Rules: []rules.Rule{{Domain: "example.com", Match: rules.Exact, Action: rules.Direct}}})
	if err := os.WriteFile(legacy, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.ImportLegacy(legacy); err != nil {
		t.Fatal(err)
	}
	snapshot, ok, err := db.LoadStore()
	if err != nil || !ok {
		t.Fatalf("migration failed: ok=%t err=%v", ok, err)
	}
	store, err := rules.Decode(snapshot.Data)
	if err != nil || len(store.Rules) != 1 {
		t.Fatalf("bad migrated store: %+v err=%v", store, err)
	}
	item := Mutation{ID: "op1", Operation: "add", Rule: rules.Rule{Domain: "queued.example", Match: rules.Suffix, Action: rules.Proxy}, CreatedAt: time.Now().UTC()}
	if err := db.Enqueue(item, 10); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	items, err := db.Mutations()
	if err != nil || len(items) != 1 || items[0].ID != "op1" {
		t.Fatalf("queue did not persist: %+v err=%v", items, err)
	}
}

func TestQueueLimit(t *testing.T) {
	db, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Enqueue(Mutation{ID: "one"}, 1); err != nil {
		t.Fatal(err)
	}
	if err := db.Enqueue(Mutation{ID: "two"}, 1); err == nil {
		t.Fatal("expected queue limit error")
	}
}
