package gc

import (
	"context"

	"mini-docker/containerd/content"
	"mini-docker/containerd/snapshots"
)

/*
=======================================================================
  GC 适配器 —— 把 content.Store / snapshots.Snapshotter 适配成 GC 所需的删除/遍历接口
=======================================================================

  GC 模块通过 ContentDeleter / SnapshotDeleter 这两个**最小必要接口**与外部模块解耦：

  ┌──────────────────┐         ┌──────────────────┐
  │  ContentDeleter  │  ◀────  │  ContentStore    │
  │  (Delete/Walk)   │  适配   │  Adapter         │
  └──────────────────┘         └──────────────────┘
                                    ▲
                                    │ 包装
                                    │
                              ┌─────┴────────┐
                              │ content.Store│
                              │  (file/blob) │
                              └──────────────┘

  同样的，SnapshotDeleter 由 SnapshotterAdapter 把 snapshots.Snapshotter 适配而来。

  这种适配的价值：
  - GC 模块只依赖 Delete/Walk 两个方法，content.Store 的元数据 API（Info/Exists）都被屏蔽
  - 换 content 存储后端（file → s3 → blob）不影响 GC
  - 单元测试可以直接 mock ContentDeleter / SnapshotDeleter，无需构造真实 content/snapshot

  之前 adapter 类型（contentDeleter / snapshotDeleter）放在
  `containerd/handler_snapshot_linux.go` 中，这是 kitchen-sink 反模式：
  - handler 文件本应只关注请求路由，不应承担适配器职责
  - 适配器属于 gc 模块的内部组件，应该由 gc 包自己暴露

  本文件修复上述问题，把 adapter 收敛到 gc 包内并导出。

=======================================================================
*/

// ContentStoreAdapter 适配 content.Store 到 GC ContentDeleter 接口
// 对齐 containerd: GC 通过 ContentDeleter 接口删除/遍历 blob，
// 不直接依赖 content.Store 的元数据 API（Info/Exists），降低耦合
type ContentStoreAdapter struct {
	store content.Store
}

// NewContentStoreAdapter 创建一个 ContentStoreAdapter
func NewContentStoreAdapter(store content.Store) *ContentStoreAdapter {
	return &ContentStoreAdapter{store: store}
}

// Delete 删除指定的 content blob
func (a *ContentStoreAdapter) Delete(ctx context.Context, digest string) error {
	return a.store.Delete(ctx, digest)
}

// Walk 遍历所有 content blob，对每个 blob 调用 fn
// 把 content.Info（包含 Digest 和 Size）压缩为 GC 所需的 (digest, size) 形式
func (a *ContentStoreAdapter) Walk(ctx context.Context, fn func(digest string, size int64) error) error {
	return a.store.Walk(ctx, func(info content.Info) error {
		return fn(info.Digest, info.Size)
	})
}

// SnapshotterAdapter 适配 snapshots.Snapshotter 到 GC SnapshotDeleter 接口
// 对齐 containerd: 依赖 snapshots.Snapshotter 接口而非 overlay 具体类型，
// 支持可插拔 Snapshotter（overlay/btrfs/native）
type SnapshotterAdapter struct {
	snap snapshots.Snapshotter
}

// NewSnapshotterAdapter 创建一个 SnapshotterAdapter
func NewSnapshotterAdapter(snap snapshots.Snapshotter) *SnapshotterAdapter {
	return &SnapshotterAdapter{snap: snap}
}

// Remove 删除指定的快照
func (a *SnapshotterAdapter) Remove(ctx context.Context, key string) error {
	return a.snap.Remove(ctx, key)
}

// Walk 遍历所有快照，对每个快照调用 fn
// 把 snapshots.Info（包含 Name 和 Parent 等）压缩为 GC 所需的 (name) 形式
func (a *SnapshotterAdapter) Walk(ctx context.Context, fn func(name string) error) error {
	return a.snap.Walk(ctx, func(info snapshots.Info) error {
		return fn(info.Name)
	})
}
