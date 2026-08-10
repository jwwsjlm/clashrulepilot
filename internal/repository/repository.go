package repository

import (
	"context"
	"errors"
	"time"
)

var ErrConflict = errors.New("repository revision conflict")

type AccessReport struct {
	Provider      string
	User          string
	Authenticated bool
	Readable      bool
	Writable      bool
	Public        bool
	RawAccessible bool
	RawError      string
	Branch        string
	Revision      string
	Error         string
	Transient     bool
	CheckedAt     time.Time
}

type FileChange struct {
	Content []byte
	Delete  bool
}

type Repository interface {
	EnsureRepo(context.Context) error
	CheckAccess(context.Context) AccessReport
	HeadRevision(context.Context) (string, error)
	GetFile(context.Context, string) ([]byte, string, error)
	GetFileAtRevision(context.Context, string, string) ([]byte, string, error)
	CommitFiles(context.Context, map[string][]byte, string, ...string) (string, error)
	Owner() string
	Repo() string
	Branch() string
	WebURL() string
	RawURL(string) string
}

// ExtendedRepository adds atomic create/update/delete and tree listing support.
// Keeping it separate preserves compatibility with lightweight repository fakes.
type ExtendedRepository interface {
	Repository
	CommitChanges(context.Context, map[string]FileChange, string, string) (string, error)
	ListFiles(context.Context, string, string) ([]string, error)
}
