package snapshots

import "context"

// Service 对齐 containerd 的 snapshots.Service
// 在 Snapshotter 之上提供 Service 层，用于后续加入 namespace 隔离、事件发布、访问控制等横切逻辑。
type Service struct {
	snapshotter Snapshotter
}

// NewService 创建 Snapshotter Service
func NewService(snapshotter Snapshotter) *Service {
	return &Service{snapshotter: snapshotter}
}

// Snapshotter 返回底层 Snapshotter（仅供内部特殊场景使用，外部调用应优先使用 Service 方法）
func (s *Service) Snapshotter() Snapshotter {
	return s.snapshotter
}

// Prepare 创建可写快照
func (s *Service) Prepare(ctx context.Context, key, parent string, opts ...Opt) ([]Mount, error) {
	return s.snapshotter.Prepare(ctx, key, parent, opts...)
}

// View 创建只读活跃快照
func (s *Service) View(ctx context.Context, key, parent string, opts ...Opt) ([]Mount, error) {
	return s.snapshotter.View(ctx, key, parent, opts...)
}

// Commit 将 Active 快照提交为 Committed 快照
func (s *Service) Commit(ctx context.Context, name, key string, opts ...Opt) error {
	return s.snapshotter.Commit(ctx, name, key, opts...)
}

// Mounts 获取快照挂载信息
func (s *Service) Mounts(ctx context.Context, key string) ([]Mount, error) {
	return s.snapshotter.Mounts(ctx, key)
}

// Remove 删除快照
func (s *Service) Remove(ctx context.Context, key string) error {
	return s.snapshotter.Remove(ctx, key)
}

// Stat 查询快照元信息
func (s *Service) Stat(ctx context.Context, key string) (Info, error) {
	return s.snapshotter.Stat(ctx, key)
}

// Update 更新快照元信息
func (s *Service) Update(ctx context.Context, info Info, fieldpaths ...string) (Info, error) {
	return s.snapshotter.Update(ctx, info, fieldpaths...)
}

// Usage 查询快照资源使用量
func (s *Service) Usage(ctx context.Context, key string) (Usage, error) {
	return s.snapshotter.Usage(ctx, key)
}

// Walk 遍历所有快照
func (s *Service) Walk(ctx context.Context, fn WalkFunc, filters ...string) error {
	return s.snapshotter.Walk(ctx, fn, filters...)
}

// Cleanup 清理已移除/废弃快照的磁盘资源
func (s *Service) Cleanup(ctx context.Context) error {
	return s.snapshotter.Cleanup(ctx)
}
