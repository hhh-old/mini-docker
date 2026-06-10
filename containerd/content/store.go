package content

import (
	"context"
	"io"
	"strings"
)

// DigestToCacheID 将 digest 转换为 cacheID（去掉 sha256: 前缀）
// 对齐 containerd: content store 中的文件名是 digest 去掉算法前缀后的 hex 值
// 此函数为项目中 digest → cacheID 转换的唯一实现，避免在多个包中重复定义
func DigestToCacheID(digest string) string {
	if strings.HasPrefix(digest, "sha256:") {
		return digest[7:]
	}
	return digest
}

type Info struct {
	Digest    string            `json:"digest"`
	Size      int64             `json:"size"`
	MediaType string            `json:"media_type,omitempty"`
	CreatedAt string            `json:"created_at"`
	Labels    map[string]string `json:"labels,omitempty"`
}

type Store interface {
	Writer(ctx context.Context, expected string, size int64, mediaType string) (Writer, error) //创建一个写入器，用于写入 blob 数据

	Reader(ctx context.Context, digest string) (io.ReadCloser, error) //按 digest 读取 blob 内容

	Delete(ctx context.Context, digest string) error //按 digest 删除 blob

	Info(ctx context.Context, digest string) (Info, error) //查询 blob 的元信息（大小、类型、标签等）

	Walk(ctx context.Context, fn func(Info) error) error //遍历所有 blob 的元信息

	Update(ctx context.Context, digest string, labels map[string]string) error //更新 blob 的标签

	Exists(ctx context.Context, digest string) bool //检查 blob 是否存在

	// Path 返回 blob 的本地存储路径
	// 仅对基于文件系统的实现有意义；调用方应仅在确实需要直接路径访问
	//（如流式 tar 解压）时使用。如能用 Reader 流式处理，请优先用 Reader。
	Path(ctx context.Context, digest string) (string, error)
}

type Writer interface {
	io.Writer
	Commit(ctx context.Context, expectedDigest string) error
	Status() (int64, error)
	Close() error
	Digest() string
}
