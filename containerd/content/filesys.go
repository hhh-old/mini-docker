package content

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"mini-docker/constants"
	"mini-docker/containerd/metadata"

	bolt "go.etcd.io/bbolt"
)

type fsStore struct {
	root string
	db   *metadata.DB
}

func NewFilesystemStore(root string, db *metadata.DB) (Store, error) {
	if err := os.MkdirAll(root, 0755); err != nil {
		return nil, fmt.Errorf("创建 content 目录失败: %w", err)
	}
	return &fsStore{root: root, db: db}, nil
}

func digestPath(root, digest string) string {
	hash := digest
	if strings.HasPrefix(hash, "sha256:") {
		hash = strings.TrimPrefix(hash, "sha256:")
	}
	if len(hash) < 2 {
		return ""
	}
	return filepath.Join(root, hash[:2], hash)
}

type contentWriter struct {
	store      *fsStore
	tmpPath    string
	file       *os.File
	hash       hash.Hash
	written    int64
	mediaType  string
	committed  bool
}

func (s *fsStore) Writer(ctx context.Context, expected string, size int64, mediaType string) (Writer, error) {
	if err := os.MkdirAll(s.root, 0755); err != nil {
		return nil, err
	}

	tmpName := fmt.Sprintf(".tmp-%d-%d", time.Now().UnixNano(), os.Getpid())
	tmpPath := filepath.Join(s.root, tmpName)

	f, err := os.Create(tmpPath)
	if err != nil {
		return nil, fmt.Errorf("创建临时文件失败: %w", err)
	}

	return &contentWriter{
		store:     s,
		tmpPath:   tmpPath,
		file:      f,
		hash:      sha256.New(),
		mediaType: mediaType,
	}, nil
}

func (w *contentWriter) Write(p []byte) (int, error) {
	n, err := w.file.Write(p)
	if n > 0 {
		w.hash.Write(p[:n])
		w.written += int64(n)
	}
	return n, err
}

func (w *contentWriter) Commit(ctx context.Context, expectedDigest string) error {
	if w.committed {
		return fmt.Errorf("已经提交")
	}

	if err := w.file.Sync(); err != nil {
		return fmt.Errorf("sync 失败: %w", err)
	}
	if err := w.file.Close(); err != nil {
		return fmt.Errorf("关闭文件失败: %w", err)
	}

	calculated := "sha256:" + hex.EncodeToString(w.hash.Sum(nil))

	if expectedDigest != "" && calculated != expectedDigest {
		os.Remove(w.tmpPath)
		return fmt.Errorf("digest 校验失败: 期望 %s, 实际 %s", expectedDigest, calculated)
	}

	hash := calculated
	if strings.HasPrefix(hash, "sha256:") {
		hash = strings.TrimPrefix(hash, "sha256:")
	}

	if len(hash) < 2 {
		os.Remove(w.tmpPath)
		return fmt.Errorf("无效的 digest: %s", calculated)
	}

	subDir := filepath.Join(w.store.root, hash[:2])
	if err := os.MkdirAll(subDir, 0755); err != nil {
		return fmt.Errorf("创建子目录失败: %w", err)
	}

	finalPath := filepath.Join(subDir, hash)
	if _, err := os.Stat(finalPath); err == nil {
		os.Remove(w.tmpPath)
	} else {
		if err := os.Rename(w.tmpPath, finalPath); err != nil {
			return fmt.Errorf("重命名失败: %w", err)
		}
	}

	info := Info{
		Digest:    calculated,
		Size:      w.written,
		MediaType: w.mediaType,
		CreatedAt: time.Now().Format(constants.TimeFormat),
		Labels:    make(map[string]string),
	}

	infoBytes, err := json.Marshal(info)
	if err != nil {
		return fmt.Errorf("序列化 Info 失败: %w", err)
	}

	err = w.store.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(metadata.BucketContent)
		return b.Put([]byte(calculated), infoBytes)
	})
	if err != nil {
		return fmt.Errorf("写入 metadata 失败: %w", err)
	}

	w.committed = true
	return nil
}

func (w *contentWriter) Status() (int64, error) {
	return w.written, nil
}

func (w *contentWriter) Close() error {
	if w.committed {
		return nil
	}
	w.file.Close()
	os.Remove(w.tmpPath)
	return nil
}

func (w *contentWriter) Digest() string {
	return "sha256:" + hex.EncodeToString(w.hash.Sum(nil))
}

func (s *fsStore) Reader(ctx context.Context, digest string) (io.ReadCloser, error) {
	p := digestPath(s.root, digest)
	if p == "" {
		return nil, fmt.Errorf("无效的 digest: %s", digest)
	}
	f, err := os.Open(p)
	if err != nil {
		return nil, fmt.Errorf("打开内容文件失败: %w", err)
	}
	return f, nil
}

func (s *fsStore) Delete(ctx context.Context, digest string) error {
	p := digestPath(s.root, digest)
	if p == "" {
		return fmt.Errorf("无效的 digest: %s", digest)
	}

	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("删除内容文件失败: %w", err)
	}

	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(metadata.BucketContent)
		return b.Delete([]byte(digest))
	})
}

func (s *fsStore) Info(ctx context.Context, digest string) (Info, error) {
	var info Info
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(metadata.BucketContent)
		data := b.Get([]byte(digest))
		if data == nil {
			return fmt.Errorf("内容不存在: %s", digest)
		}
		return json.Unmarshal(data, &info)
	})
	return info, err
}

func (s *fsStore) Walk(ctx context.Context, fn func(Info) error) error {
	return s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(metadata.BucketContent)
		c := b.Cursor()
		for k, v := c.First(); k != nil; k, v = c.Next() {
			var info Info
			if err := json.Unmarshal(v, &info); err != nil {
				continue
			}
			if err := fn(info); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *fsStore) Update(ctx context.Context, digest string, labels map[string]string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(metadata.BucketContent)
		data := b.Get([]byte(digest))
		if data == nil {
			return fmt.Errorf("内容不存在: %s", digest)
		}

		var info Info
		if err := json.Unmarshal(data, &info); err != nil {
			return err
		}
		info.Labels = labels

		infoBytes, err := json.Marshal(info)
		if err != nil {
			return err
		}
		return b.Put([]byte(digest), infoBytes)
	})
}

func (s *fsStore) Exists(ctx context.Context, digest string) bool {
	found := false
	s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(metadata.BucketContent)
		data := b.Get([]byte(digest))
		found = data != nil
		return nil
	})
	return found
}
