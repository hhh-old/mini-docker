package content

import (
	"bufio"
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

// digestPath 将 digest 转换为文件系统路径
// 对齐 containerd: blobs/sha256/<encoded-digest>，平铺格式，无前缀分桶
func digestPath(root, digest string) string {
	hash := DigestToCacheID(digest)
	if hash == "" {
		return ""
	}
	return filepath.Join(root, hash)
}

type contentWriter struct {
	store   *fsStore
	tmpPath string
	file    *os.File
	//bufWriter的作用：
	//### 减少 syscall，提升 I/O 吞吐
	//content store 写的是 OCI blob（manifest/config/layer），单层 tar.gz 动辄几十～几百 MB。 os.File.Write 每次小写入都是一次 write() syscall，没 bufio 的话上层 DownloadBlob 那边一次 Read 几十 KB 就直接落盘，开销爆炸。256KB buffer 一次刷盘能聚合多次写入，对顺序写尤其友好。
	bufWriter *bufio.Writer
	hash      hash.Hash
	written   int64
	mediaType string
	committed bool
}

// ### 写入流程（核心）
// Writer → Write → Commit 的流程是关键：
//
// 1. Writer ：在 root 目录创建临时文件 .tmp-<timestamp>-<pid>
// 2. Write ：数据同时写入临时文件和 SHA256 哈希计算器（边写边算摘要）
// 3. Commit ：
//   - 刷盘并关闭临时文件
//   - 校验计算的 digest 与期望 digest 是否一致，不一致则删除临时文件并报错
//   - 将临时文件 rename 为最终的 digest 文件名（幂等：若已存在则直接删除临时文件）
//   - 将 Info 元信息写入 BoltDB
//
// 这种"先写临时文件，再 rename"的方式保证了 原子性 ——不会出现写到一半的不完整数据。
// Writer也提供了校验写入的内容是否是预期内容的功能（使用sha256）
func (s *fsStore) Writer(ctx context.Context, expected string, size int64, mediaType string) (Writer, error) {
	if err := os.MkdirAll(s.root, 0755); err != nil {
		return nil, err
	}
	//在 root 目录创建临时文件 .tmp-<timestamp>-<pid>
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
		bufWriter: bufio.NewWriterSize(f, 256*1024),
		hash:      sha256.New(),
		mediaType: mediaType,
	}, nil
}

func (w *contentWriter) Write(p []byte) (int, error) {
	n, err := w.bufWriter.Write(p) // 数据先进 bufio 缓冲
	if n > 0 {
		w.hash.Write(p[:n]) // SHA256 边写边算
		w.written += int64(n)
	}
	return n, err
}

func (w *contentWriter) Commit(ctx context.Context, expectedDigest string) error {
	if w.committed {
		return fmt.Errorf("已经提交")
	}
	// ← 提交前必须 Flush
	if err := w.bufWriter.Flush(); err != nil {
		return fmt.Errorf("flush 失败: %w", err)
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

	if hash == "" {
		os.Remove(w.tmpPath)
		return fmt.Errorf("无效的 digest: %s", calculated)
	}

	finalPath := filepath.Join(w.store.root, hash)
	if _, err := os.Stat(finalPath); err == nil { //finalPath 已经存在 → 说明这个 digest 的内容之前已经写入过了（幂等），所以直接删除临时文件即可
		os.Remove(w.tmpPath)
	} else {
		//finalPath 不存在 → 把临时文件 rename 为最终文件名，这就是 创建最终文件的时刻
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
	w.bufWriter.Flush()
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
	//先删除磁盘上的blob目录内容
	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("删除内容文件失败: %w", err)
	}
	//再删除boltdb中的元数据
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

func (s *fsStore) Path(ctx context.Context, digest string) (string, error) {
	p := digestPath(s.root, digest)
	if p == "" {
		return "", fmt.Errorf("无效的 digest: %s", digest)
	}
	return p, nil
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
