package content

import (
	"context"
	"io"
)

type Info struct {
	Digest    string            `json:"digest"`
	Size      int64             `json:"size"`
	MediaType string            `json:"media_type,omitempty"`
	CreatedAt string            `json:"created_at"`
	Labels    map[string]string `json:"labels,omitempty"`
}

type Store interface {
	Writer(ctx context.Context, expected string, size int64, mediaType string) (Writer, error)

	Reader(ctx context.Context, digest string) (io.ReadCloser, error)

	Delete(ctx context.Context, digest string) error

	Info(ctx context.Context, digest string) (Info, error)

	Walk(ctx context.Context, fn func(Info) error) error

	Update(ctx context.Context, digest string, labels map[string]string) error

	Exists(ctx context.Context, digest string) bool
}

type Writer interface {
	io.Writer
	Commit(ctx context.Context, expectedDigest string) error
	Status() (int64, error)
	Close() error
	Digest() string
}
