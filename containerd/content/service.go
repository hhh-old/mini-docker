package content

import (
	"context"
	"io"
)

// Service 对齐 containerd 的 content.Service
// 在 Store 之上提供 Service 层，用于后续加入 namespace 隔离、事件发布、访问控制等横切逻辑。
type Service struct {
	store Store
}

// NewService 创建 Content Service
func NewService(store Store) *Service {
	return &Service{store: store}
}

// Store 返回底层 Content Store（仅供内部特殊场景使用，外部调用应优先使用 Service 方法）
func (s *Service) Store() Store {
	return s.store
}

// Info 查询 blob 元信息
func (s *Service) Info(ctx context.Context, digest string) (Info, error) {
	return s.store.Info(ctx, digest)
}

// Reader 按 digest 读取 blob 内容
func (s *Service) Reader(ctx context.Context, digest string) (io.ReadCloser, error) {
	return s.store.Reader(ctx, digest)
}

// Writer 创建 blob 写入器
func (s *Service) Writer(ctx context.Context, expected string, size int64, mediaType string) (Writer, error) {
	return s.store.Writer(ctx, expected, size, mediaType)
}

// Delete 按 digest 删除 blob
func (s *Service) Delete(ctx context.Context, digest string) error {
	return s.store.Delete(ctx, digest)
}

// Walk 遍历所有 blob 元信息
func (s *Service) Walk(ctx context.Context, fn func(Info) error) error {
	return s.store.Walk(ctx, fn)
}

// Update 更新 blob 标签
func (s *Service) Update(ctx context.Context, digest string, labels map[string]string) error {
	return s.store.Update(ctx, digest, labels)
}

// Exists 检查 blob 是否存在
func (s *Service) Exists(ctx context.Context, digest string) bool {
	return s.store.Exists(ctx, digest)
}

// Path 获取 blob 本地存储路径
func (s *Service) Path(ctx context.Context, digest string) (string, error) {
	return s.store.Path(ctx, digest)
}
