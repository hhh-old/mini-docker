package native

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	bolt "go.etcd.io/bbolt"

	"mini-docker/constants"
	"mini-docker/containerd/metadata"
	"mini-docker/containerd/snapshots"
)

/*
=======================================================================
  Native Snapshotter —— 对齐 containerd 的 native snapshotter

  与 overlay snapshotter 的核心区别：
  - 不使用 OverlayFS，而是通过目录拷贝（cp -r）构建快照
  - Prepare 时如果有 parent，将 parent 的 fs/ 内容拷贝到新快照的 fs/
  - Mounts 始终返回 bind mount（Active: rw, Committed/View: ro）
  - 简单但占用更多磁盘空间（每层完整拷贝）

  存储结构：
  <root>/snapshots/<id>/fs/   ← 文件系统内容

  适用场景：
  - 不支持 OverlayFS 的环境
  - 需要独立完整文件系统的场景
  - 演示 Snapshotter 可插拔性

=======================================================================
*/

// NativeSnapshotter 基于目录拷贝的 Snapshotter 实现
type NativeSnapshotter struct {
	root string       // 根目录，如 /var/lib/mini-docker/snapshots/overlay-native
	db   *metadata.DB // boltdb 存储快照元数据
}

// NewSnapshotter 创建 Native Snapshotter
func NewSnapshotter(root string, db *metadata.DB) (*NativeSnapshotter, error) {
	if err := os.MkdirAll(root, 0755); err != nil {
		return nil, fmt.Errorf("创建 native snapshotter 根目录失败: %w", err)
	}
	snapshotsDir := filepath.Join(root, "snapshots")
	if err := os.MkdirAll(snapshotsDir, 0755); err != nil {
		return nil, fmt.Errorf("创建 snapshots 子目录失败: %w", err)
	}
	return &NativeSnapshotter{
		root: root,
		db:   db,
	}, nil
}

// fsPath 返回快照的 fs 目录路径：<root>/snapshots/<id>/fs
func (n *NativeSnapshotter) fsPath(id string) string {
	return filepath.Join(n.root, "snapshots", id, "fs")
}

// snapDir 返回快照目录路径：<root>/snapshots/<id>
func (n *NativeSnapshotter) snapDir(id string) string {
	return filepath.Join(n.root, "snapshots", id)
}

// Prepare 创建一个可写快照 (KindActive)
// 与 overlay 不同：如果有 parent，将 parent 的 fs/ 完整拷贝到新快照
func (n *NativeSnapshotter) Prepare(ctx context.Context, key, parent string, opts ...snapshots.Opt) ([]snapshots.Mount, error) {
	return n.createSnapshot(ctx, snapshots.KindActive, key, parent, opts...)
}

// View 创建一个只读活跃快照 (KindView)
func (n *NativeSnapshotter) View(ctx context.Context, key, parent string, opts ...snapshots.Opt) ([]snapshots.Mount, error) {
	return n.createSnapshot(ctx, snapshots.KindView, key, parent, opts...)
}

// createSnapshot 创建快照的内部实现
func (n *NativeSnapshotter) createSnapshot(ctx context.Context, kind snapshots.Kind, key, parent string, opts ...snapshots.Opt) ([]snapshots.Mount, error) {
	now := time.Now().Format(constants.TimeFormat)
	info := &snapshots.Info{
		Name:      key,
		Parent:    parent,
		Kind:      kind,
		CreatedAt: now,
		UpdatedAt: now,
	}
	for _, opt := range opts {
		if err := opt(info); err != nil {
			return nil, fmt.Errorf("应用快照选项失败: %w", err)
		}
	}

	var parentIDs []string

	if parent != "" {
		var parentInfo *snapshots.Info
		if err := n.db.View(func(tx *bolt.Tx) error {
			var err error
			parentInfo, err = metadata.LoadSnapshot(tx, parent)
			return err
		}); err != nil {
			return nil, fmt.Errorf("加载父快照 %s 失败: %w", parent, err)
		}
		// ParentIDs 顺序与 containerd 一致：最近父层在前，最远祖先在后。
		parentIDs = append([]string{parentInfo.ID}, parentInfo.ParentIDs...)
	}

	var id string
	if err := n.db.Update(func(tx *bolt.Tx) error {
		var err error
		id, err = metadata.NextSnapshotID(tx)
		if err != nil {
			return fmt.Errorf("分配快照 ID 失败: %w", err)
		}
		info.ID = id
		info.ParentIDs = parentIDs
		return metadata.SaveSnapshot(tx, info)
	}); err != nil {
		return nil, fmt.Errorf("保存快照元数据失败: %w", err)
	}

	// 创建快照目录
	snapshotsDir := filepath.Join(n.root, "snapshots")
	tmpDir, err := os.MkdirTemp(snapshotsDir, "tmp-")
	if err != nil {
		n.db.Update(func(tx *bolt.Tx) error {
			metadata.DeleteSnapshot(tx, key)
			return nil
		})
		return nil, fmt.Errorf("创建临时目录失败: %w", err)
	}

	// 创建 fs/ 目录
	if err := os.MkdirAll(filepath.Join(tmpDir, "fs"), 0755); err != nil {
		os.RemoveAll(tmpDir)
		n.db.Update(func(tx *bolt.Tx) error {
			metadata.DeleteSnapshot(tx, key)
			return nil
		})
		return nil, fmt.Errorf("创建 fs 目录失败: %w", err)
	}

	// Native 特有：如果有 parent，将 parent 的 fs/ 内容拷贝到新快照
	if parent != "" {
		var parentInfo *snapshots.Info
		if err := n.db.View(func(tx *bolt.Tx) error {
			var err error
			parentInfo, err = metadata.LoadSnapshot(tx, parent)
			return err
		}); err == nil {
			parentFsPath := n.fsPath(parentInfo.ID)
			newFsPath := filepath.Join(tmpDir, "fs")
			// 拷贝父快照内容
			if err := copyDir(parentFsPath, newFsPath); err != nil {
				// 拷贝失败不阻塞创建，仅记录警告
				// native snapshotter 的核心是演示可插拔性，非生产级实现
				fmt.Printf("[native] 拷贝父快照内容失败: %v\n", err)
			}
		}
	}

	// 原子重命名
	finalDir := n.snapDir(id)
	if err := os.Rename(tmpDir, finalDir); err != nil {
		os.RemoveAll(tmpDir)
		n.db.Update(func(tx *bolt.Tx) error {
			metadata.DeleteSnapshot(tx, key)
			return nil
		})
		return nil, fmt.Errorf("重命名快照目录失败: %w", err)
	}

	return n.mounts(key)
}

// Commit 将 Active 快照提交为 Committed 快照
// 与 overlay 相同：纯元数据操作，目录不变
func (n *NativeSnapshotter) Commit(ctx context.Context, name, key string, opts ...snapshots.Opt) error {
	var snapInfo *snapshots.Info
	if err := n.db.View(func(tx *bolt.Tx) error {
		var err error
		snapInfo, err = metadata.LoadSnapshot(tx, key)
		return err
	}); err != nil {
		return fmt.Errorf("加载快照 %s 失败: %w", key, err)
	}

	if snapInfo.Kind != snapshots.KindActive {
		return fmt.Errorf("快照 %s 不是 Active 快照，无法 Commit", key)
	}

	now := time.Now().Format(constants.TimeFormat)
	committedInfo := &snapshots.Info{
		ID:        snapInfo.ID,
		Name:      name,
		Parent:    snapInfo.Parent,
		ParentIDs: snapInfo.ParentIDs,
		Kind:      snapshots.KindCommitted,
		CreatedAt: snapInfo.CreatedAt,
		UpdatedAt: now,
		Labels:    snapInfo.Labels,
	}
	for _, opt := range opts {
		if err := opt(committedInfo); err != nil {
			return fmt.Errorf("应用 Commit 选项失败: %w", err)
		}
	}

	// 计算磁盘使用量
	usage, err := n.diskUsage(committedInfo.ID)
	if err == nil {
		if committedInfo.Labels == nil {
			committedInfo.Labels = make(map[string]string)
		}
		committedInfo.Labels["usage.size"] = fmt.Sprintf("%d", usage.Size)
		committedInfo.Labels["usage.inodes"] = fmt.Sprintf("%d", usage.Inodes)
	}

	if err := n.db.Update(func(tx *bolt.Tx) error {
		metadata.DeleteSnapshot(tx, key)
		return metadata.SaveSnapshot(tx, committedInfo)
	}); err != nil {
		return fmt.Errorf("提交快照元数据失败: %w", err)
	}

	return nil
}

// Mounts 获取快照的挂载信息
// Native snapshotter 始终返回 bind mount
func (n *NativeSnapshotter) Mounts(ctx context.Context, key string) ([]snapshots.Mount, error) {
	return n.mounts(key)
}

// mounts 构建快照的挂载信息
// Native 特有：始终使用 bind mount，不使用 overlay
// - Active: bind mount rw
// - Committed: bind mount ro
// - View: bind mount ro
func (n *NativeSnapshotter) mounts(key string) ([]snapshots.Mount, error) {
	var snapInfo *snapshots.Info
	if err := n.db.View(func(tx *bolt.Tx) error {
		var err error
		snapInfo, err = metadata.LoadSnapshot(tx, key)
		return err
	}); err != nil {
		return nil, fmt.Errorf("加载快照 %s 失败: %w", key, err)
	}

	switch snapInfo.Kind {
	case snapshots.KindActive:
		return []snapshots.Mount{
			{
				Type:    "bind",
				Source:  n.fsPath(snapInfo.ID),
				Options: []string{"rw", "rbind"},
			},
		}, nil
	case snapshots.KindCommitted, snapshots.KindView:
		return []snapshots.Mount{
			{
				Type:    "bind",
				Source:  n.fsPath(snapInfo.ID),
				Options: []string{"ro", "rbind"},
			},
		}, nil
	default:
		return nil, fmt.Errorf("未知的快照类型: %d", snapInfo.Kind)
	}
}

// Remove 删除快照
func (n *NativeSnapshotter) Remove(ctx context.Context, key string) error {
	var snapInfo *snapshots.Info
	var childCount int
	if err := n.db.View(func(tx *bolt.Tx) error {
		var err error
		snapInfo, err = metadata.LoadSnapshot(tx, key)
		if err != nil {
			return err
		}
		childCount, err = metadata.CountSnapshotsByParent(tx, key)
		return err
	}); err != nil {
		return fmt.Errorf("加载快照 %s 失败: %w", key, err)
	}

	if childCount > 0 {
		return fmt.Errorf("快照 %s 仍被 %d 个子快照引用，无法删除", key, childCount)
	}

	if err := n.db.Update(func(tx *bolt.Tx) error {
		return metadata.DeleteSnapshot(tx, key)
	}); err != nil {
		return fmt.Errorf("删除快照元数据失败: %w", err)
	}

	snapPath := n.snapDir(snapInfo.ID)
	if err := os.RemoveAll(snapPath); err != nil {
		return fmt.Errorf("删除快照目录失败: %w", err)
	}

	return nil
}

// Stat 返回指定快照的元信息
func (n *NativeSnapshotter) Stat(ctx context.Context, key string) (snapshots.Info, error) {
	var snapInfo *snapshots.Info
	if err := n.db.View(func(tx *bolt.Tx) error {
		var err error
		snapInfo, err = metadata.LoadSnapshot(tx, key)
		return err
	}); err != nil {
		return snapshots.Info{}, fmt.Errorf("加载快照 %s 失败: %w", key, err)
	}
	return *snapInfo, nil
}

// Update 更新快照的元信息
func (n *NativeSnapshotter) Update(ctx context.Context, info snapshots.Info, fieldpaths ...string) (snapshots.Info, error) {
	var snapInfo *snapshots.Info
	if err := n.db.Update(func(tx *bolt.Tx) error {
		var err error
		snapInfo, err = metadata.LoadSnapshot(tx, info.Name)
		if err != nil {
			return err
		}

		if len(fieldpaths) == 0 {
			snapInfo.Labels = info.Labels
		} else {
			for _, path := range fieldpaths {
				switch path {
				case "labels", "labels.":
					snapInfo.Labels = info.Labels
				default:
					if strings.HasPrefix(path, "labels.") {
						lkey := strings.TrimPrefix(path, "labels.")
						if snapInfo.Labels == nil {
							snapInfo.Labels = make(map[string]string)
						}
						if info.Labels != nil {
							if v, ok := info.Labels[lkey]; ok {
								snapInfo.Labels[lkey] = v
							} else {
								delete(snapInfo.Labels, lkey)
							}
						} else {
							delete(snapInfo.Labels, lkey)
						}
					}
				}
			}
		}

		snapInfo.UpdatedAt = time.Now().Format(constants.TimeFormat)
		return metadata.SaveSnapshot(tx, snapInfo)
	}); err != nil {
		return snapshots.Info{}, fmt.Errorf("更新快照元数据失败: %w", err)
	}

	return *snapInfo, nil
}

// Usage 返回快照的资源使用量
func (n *NativeSnapshotter) Usage(ctx context.Context, key string) (snapshots.Usage, error) {
	var snapInfo *snapshots.Info
	if err := n.db.View(func(tx *bolt.Tx) error {
		var err error
		snapInfo, err = metadata.LoadSnapshot(tx, key)
		return err
	}); err != nil {
		return snapshots.Usage{}, fmt.Errorf("加载快照 %s 失败: %w", key, err)
	}

	if snapInfo.Kind == snapshots.KindCommitted {
		usage := snapshots.Usage{}
		if v, ok := snapInfo.Labels["usage.size"]; ok {
			fmt.Sscanf(v, "%d", &usage.Size)
		}
		if v, ok := snapInfo.Labels["usage.inodes"]; ok {
			fmt.Sscanf(v, "%d", &usage.Inodes)
		}
		return usage, nil
	}

	return n.diskUsage(snapInfo.ID)
}

// diskUsage 扫描快照的 fs/ 目录，计算磁盘使用量
func (n *NativeSnapshotter) diskUsage(id string) (snapshots.Usage, error) {
	fsDir := n.fsPath(id)
	var usage snapshots.Usage
	err := filepath.Walk(fsDir, func(path string, info fs.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		usage.Size += info.Size()
		usage.Inodes++
		return nil
	})
	if err != nil {
		return snapshots.Usage{}, fmt.Errorf("计算磁盘使用量失败: %w", err)
	}
	return usage, nil
}

// Walk 遍历所有快照
func (n *NativeSnapshotter) Walk(ctx context.Context, fn snapshots.WalkFunc, filters ...string) error {
	return n.db.View(func(tx *bolt.Tx) error {
		return metadata.WalkSnapshots(tx, func(info *snapshots.Info) error {
			return fn(ctx, *info)
		})
	})
}

// Cleanup 清理已移除/废弃快照的磁盘资源
func (n *NativeSnapshotter) Cleanup(ctx context.Context) error {
	idMap := make(map[string]struct{})
	if err := n.db.View(func(tx *bolt.Tx) error {
		m, err := metadata.GetSnapshotIDMap(tx)
		if err != nil {
			return err
		}
		idMap = m
		return nil
	}); err != nil {
		return fmt.Errorf("获取快照 ID 映射失败: %w", err)
	}

	snapshotsDir := filepath.Join(n.root, "snapshots")
	entries, err := os.ReadDir(snapshotsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("读取 snapshots 目录失败: %w", err)
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if strings.HasPrefix(entry.Name(), "tmp-") {
			continue
		}
		if _, exists := idMap[entry.Name()]; !exists {
			orphanPath := filepath.Join(snapshotsDir, entry.Name())
			if err := os.RemoveAll(orphanPath); err != nil {
				fmt.Printf("警告: 清理孤立目录 %s 失败: %v\n", orphanPath, err)
			}
		}
	}

	return nil
}

// Close 关闭 Snapshotter
func (n *NativeSnapshotter) Close() error {
	return nil
}

// copyDir 递归拷贝目录内容
// 将 src 目录下的所有文件和子目录拷贝到 dst 目录
func copyDir(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // 跳过无法访问的文件
		}

		relPath, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		targetPath := filepath.Join(dst, relPath)

		if info.IsDir() {
			return os.MkdirAll(targetPath, info.Mode())
		}

		// 拷贝文件
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(targetPath, data, info.Mode())
	})
}
