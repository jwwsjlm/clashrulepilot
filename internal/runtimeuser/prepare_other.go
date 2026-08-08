//go:build !linux

package runtimeuser

import (
	"fmt"
	"os"
	"path/filepath"
)

type Info struct {
	UID     int
	GID     int
	Dropped bool
	Warning string
}

func Prepare(dataDir string) (Info, error) {
	if dataDir == "" {
		return Info{}, fmt.Errorf("DATA_DIR is empty")
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return Info{}, fmt.Errorf("create data directory: %w", err)
	}
	testPath := filepath.Join(dataDir, ".clashrulepilot-write-test")
	if err := os.WriteFile(testPath, nil, 0o600); err != nil {
		return Info{}, fmt.Errorf("data directory is not writable: %w", err)
	}
	if err := os.Remove(testPath); err != nil {
		return Info{}, fmt.Errorf("remove data directory write test: %w", err)
	}
	return Info{UID: -1, GID: -1}, nil
}
