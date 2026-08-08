//go:build linux

package runtimeuser

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

const (
	runtimeUID = 65532
	runtimeGID = 65532
)

type Info struct {
	UID     int
	GID     int
	Dropped bool
	Warning string
}

// Prepare makes a bind-mounted data directory writable and then drops root.
// Only DATA_DIR is traversed; no project or parent directory is modified.
func Prepare(dataDir string) (Info, error) {
	if dataDir == "" {
		return Info{}, fmt.Errorf("DATA_DIR is empty")
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return Info{}, fmt.Errorf("create data directory: %w", err)
	}

	info := Info{UID: os.Geteuid(), GID: os.Getegid()}
	if os.Geteuid() == 0 {
		chownErr := filepath.WalkDir(dataDir, func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			return os.Lchown(path, runtimeUID, runtimeGID)
		})
		if chownErr != nil {
			info.Warning = fmt.Sprintf("automatic chown failed; continuing as root: %v", chownErr)
		} else {
			if err := syscall.Setgroups([]int{runtimeGID}); err != nil {
				return Info{}, fmt.Errorf("set supplementary groups: %w", err)
			}
			if err := syscall.Setgid(runtimeGID); err != nil {
				return Info{}, fmt.Errorf("drop runtime gid: %w", err)
			}
			if err := syscall.Setuid(runtimeUID); err != nil {
				return Info{}, fmt.Errorf("drop runtime uid: %w", err)
			}
			info.UID, info.GID, info.Dropped = os.Geteuid(), os.Getegid(), true
		}
	}

	testPath := filepath.Join(dataDir, ".clashrulepilot-write-test")
	f, err := os.OpenFile(testPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return Info{}, fmt.Errorf("data directory is not writable: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(testPath)
		return Info{}, fmt.Errorf("close data directory write test: %w", err)
	}
	if err := os.Remove(testPath); err != nil {
		return Info{}, fmt.Errorf("remove data directory write test: %w", err)
	}
	return info, nil
}
