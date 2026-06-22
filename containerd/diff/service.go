package diff

import (
	"context"
	"fmt"

	"mini-docker/containerd/content"
	"mini-docker/containerd/snapshots"
)

// Service 对齐 containerd 的 diff.Service
// 协调 Snapshotter + Content Store + Differ/Applier，对外提供统一的层差异能力。
type Service struct {
	applier     Applier
	differ      Differ
	content     content.Store
	snapshotter snapshots.Snapshotter
}

// NewService 创建 Diff Service
func NewService(contentStore content.Store, snap snapshots.Snapshotter, applier Applier, differ Differ) *Service {
	return &Service{
		content:     contentStore,
		snapshotter: snap,
		applier:     applier,
		differ:      differ,
	}
}

// ContentStore 返回底层 Content Store
func (s *Service) ContentStore() content.Store {
	return s.content
}

// Snapshotter 返回底层 Snapshotter
func (s *Service) Snapshotter() snapshots.Snapshotter {
	return s.snapshotter
}

// Applier 返回底层 Applier
func (s *Service) Applier() Applier {
	return s.applier
}

// Differ 返回底层 Differ
func (s *Service) Differ() Differ {
	return s.differ
}

// Apply 将 digest 对应的 layer blob 应用到 key 指定的 Active 快照
func (s *Service) Apply(ctx context.Context, digest, diffID, key string) error {
	info, err := s.content.Info(ctx, digest)
	if err != nil {
		return fmt.Errorf("查询 blob 信息失败: %w", err)
	}

	mounts, err := s.snapshotter.Mounts(ctx, key)
	if err != nil {
		return fmt.Errorf("获取快照挂载信息失败: %w", err)
	}

	path, err := s.content.Path(ctx, info.Digest)
	if err != nil {
		return fmt.Errorf("获取 blob 路径失败: %w", err)
	}

	return s.applier.Apply(ctx, digest, diffID, path, mounts)
}

// Diff 计算 lowerKey 与 upperKey 两个快照之间的差异，生成 blob 写入 Content Store
// lowerKey 为空时表示从空目录开始计算差异
func (s *Service) Diff(ctx context.Context, lowerKey, upperKey string, opts ...DiffOpt) (DiffResult, error) {
	var lower []snapshots.Mount
	var err error
	if lowerKey != "" {
		lower, err = s.snapshotter.Mounts(ctx, lowerKey)
		if err != nil {
			return DiffResult{}, fmt.Errorf("获取 lower 快照挂载信息失败: %w", err)
		}
	}

	upper, err := s.snapshotter.Mounts(ctx, upperKey)
	if err != nil {
		return DiffResult{}, fmt.Errorf("获取 upper 快照挂载信息失败: %w", err)
	}

	return s.differ.Diff(ctx, lower, upper, s.content, opts...)
}
