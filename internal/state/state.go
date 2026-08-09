package state

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"clashrulepilot/internal/repository"
	"clashrulepilot/internal/rules"
	bolt "go.etcd.io/bbolt"
)

var (
	bucketRuntime = []byte("runtime")
	bucketQueue   = []byte("mutation_queue")
	keyStore      = []byte("personal_store")
	keyAccess     = []byte("access_report")
)

type StoreSnapshot struct {
	Data      []byte    `json:"data"`
	Revision  string    `json:"revision,omitempty"`
	SHA256    string    `json:"sha256,omitempty"`
	FetchedAt time.Time `json:"fetched_at"`
}

type Mutation struct {
	ID          string       `json:"id"`
	Operation   string       `json:"operation"`
	Rule        rules.Rule   `json:"rule,omitempty"`
	Domain      string       `json:"domain,omitempty"`
	Match       *rules.Match `json:"match,omitempty"`
	Force       bool         `json:"force,omitempty"`
	UserID      int64        `json:"user_id"`
	ChatID      int64        `json:"chat_id"`
	CreatedAt   time.Time    `json:"created_at"`
	Status      string       `json:"status"`
	Attempts    int          `json:"attempts"`
	NextAttempt time.Time    `json:"next_attempt,omitempty"`
	LastError   string       `json:"last_error,omitempty"`
}

type DB struct{ db *bolt.DB }

func Open(dataDir string) (*DB, error) {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, err
	}
	db, err := bolt.Open(filepath.Join(dataDir, "runtime-state.db"), 0o600, &bolt.Options{Timeout: 2 * time.Second})
	if err != nil {
		return nil, err
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		if _, err := tx.CreateBucketIfNotExists(bucketRuntime); err != nil {
			return err
		}
		_, err := tx.CreateBucketIfNotExists(bucketQueue)
		return err
	}); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &DB{db: db}, nil
}

func (d *DB) Close() error {
	if d == nil || d.db == nil {
		return nil
	}
	return d.db.Close()
}

func (d *DB) LoadStore() (StoreSnapshot, bool, error) {
	var out StoreSnapshot
	err := d.db.View(func(tx *bolt.Tx) error {
		value := tx.Bucket(bucketRuntime).Get(keyStore)
		if len(value) == 0 {
			return nil
		}
		return json.Unmarshal(value, &out)
	})
	return out, len(out.Data) > 0, err
}

func (d *DB) SaveStore(snapshot StoreSnapshot) error {
	return d.putJSON(bucketRuntime, keyStore, snapshot)
}

func (d *DB) LoadAccess() (repository.AccessReport, bool, error) {
	var out repository.AccessReport
	err := d.db.View(func(tx *bolt.Tx) error {
		value := tx.Bucket(bucketRuntime).Get(keyAccess)
		if len(value) == 0 {
			return nil
		}
		return json.Unmarshal(value, &out)
	})
	return out, out.Provider != "", err
}

func (d *DB) SaveAccess(report repository.AccessReport) error {
	return d.putJSON(bucketRuntime, keyAccess, report)
}

func (d *DB) putJSON(bucket, key []byte, value any) error {
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return d.db.Update(func(tx *bolt.Tx) error { return tx.Bucket(bucket).Put(key, encoded) })
}

func NewMutation() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err == nil {
		return hex.EncodeToString(b[:])
	}
	return fmt.Sprintf("%d", time.Now().UnixNano())
}

func (d *DB) Enqueue(m Mutation, limit int) error {
	if m.ID == "" {
		m.ID = NewMutation()
	}
	if m.CreatedAt.IsZero() {
		m.CreatedAt = time.Now().UTC()
	}
	if m.Status == "" {
		m.Status = "pending"
	}
	encoded, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return d.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketQueue)
		if limit > 0 && b.Stats().KeyN >= limit {
			return fmt.Errorf("mutation queue limit %d reached", limit)
		}
		return b.Put([]byte(m.ID), encoded)
	})
}

func (d *DB) UpdateMutation(m Mutation) error {
	if m.ID == "" {
		return errors.New("mutation id is required")
	}
	encoded, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return d.db.Update(func(tx *bolt.Tx) error { return tx.Bucket(bucketQueue).Put([]byte(m.ID), encoded) })
}

func (d *DB) DeleteMutation(id string) error {
	return d.db.Update(func(tx *bolt.Tx) error { return tx.Bucket(bucketQueue).Delete([]byte(id)) })
}

func (d *DB) Mutations() ([]Mutation, error) {
	var out []Mutation
	err := d.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketQueue).ForEach(func(_, value []byte) error {
			var item Mutation
			if err := json.Unmarshal(value, &item); err != nil {
				return err
			}
			out = append(out, item)
			return nil
		})
	})
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].ID < out[j].ID
		}
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out, err
}

func (d *DB) ImportLegacy(path string) error {
	if _, ok, err := d.LoadStore(); err != nil || ok {
		return err
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if _, err := rules.Decode(data); err != nil {
		return err
	}
	return d.SaveStore(StoreSnapshot{Data: data, FetchedAt: time.Now().UTC()})
}
