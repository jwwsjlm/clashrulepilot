package repository

import "context"

type Repository interface {
	EnsureRepo(context.Context) error
	GetFile(context.Context, string) ([]byte, string, error)
	CommitFiles(context.Context, map[string][]byte, string) (string, error)
	Owner() string
	Repo() string
	Branch() string
	WebURL() string
	RawURL(string) string
}
