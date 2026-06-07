package overlay

import (
	"context"
	"fmt"
	"io"
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
  OverlayFS Snapshotter —— 对齐 containerd 的 overlay snapshotter
=======================================================================

  存储结构：
  /var/lib/mini-docker/snapshots/overlay/
  └── <snap-key>/
      ├── diff/       ← 只读层内容 (Committed) 或可写层 (Active)
      ├── upper/      ← 可写层 (仅 Active)
      ├── work/       ← OverlayFS work 目录 (仅 Active)
      ├── merged/     ← OverlayFS merged (仅 Active，挂载点)
      └── info.json   ← 快照元数据 (备用，主存储在 boltdb)

  OverlayFS 挂载原理：
  - lowerdir: 只读层，多个用 ":" 分隔，从左到右优先级递增
    （最远祖先在前，最近父在后）
  - upperdir: 可写层，所有修改写入此目录
  - workdir: OverlayFS 内部使用的工作目录

=======================================================================
*/

// OverlaySnapshotter OverlayFS 实现的 Snapshotter
type OverlaySnapshotter struct {
	root string       // /var/lib/mini-docker/snapshots/overlay
	db   *metadata.DB // boltdb 存储快照元数据
}

// NewSnapshotter 创建 OverlayFS Snapshotter
func NewSnapshotter(root string, db *metadata.DB) (*OverlaySnapshotter, error) {
	if err := os.MkdirAll(root, 0755); err != nil {
		return nil, fmt.Errorf("创建 overlay snapshotter 根目录失败: %w", err)
	}
	return &OverlaySnapshotter{
		root: root,
		db:   db,
	}, nil
}

// Prepare 创建一个可写快照 (用于容器运行或镜像解包)
func (o *OverlaySnapshotter) Prepare(ctx context.Context, key, parent string) ([]snapshots.Mount, error) {
	snapDir := filepath.Join(o.root, key)
	if err := os.MkdirAll(snapDir, 0755); err != nil {
		return nil, fmt.Errorf("创建快照目录失败: %w", err)
	}

	diffDir := filepath.Join(snapDir, "diff")
	upperDir := filepath.Join(snapDir, "upper")
	workDir := filepath.Join(snapDir, "work")

	if err := os.MkdirAll(diffDir, 0755); err != nil {
		os.RemoveAll(snapDir)
		return nil, fmt.Errorf("创建 diff 目录失败: %w", err)
	}
	if err := os.MkdirAll(upperDir, 0755); err != nil {
		os.RemoveAll(snapDir)
		return nil, fmt.Errorf("创建 upper 目录失败: %w", err)
	}
	if err := os.MkdirAll(workDir, 0755); err != nil {
		os.RemoveAll(snapDir)
		return nil, fmt.Errorf("创建 work 目录失败: %w", err)
	}

	now := time.Now().Format(constants.TimeFormat)
	info := &metadata.SnapshotInfo{
		Name:      key,
		Parent:    parent,
		Kind:      metadata.KindActive,
		ReadWrite: true,
		CreatedAt: now,
		UpdatedAt: now,
	}

	if err := o.db.Update(func(tx *bolt.Tx) error {
		return metadata.SaveSnapshot(tx, info)
	}); err != nil {
		os.RemoveAll(snapDir)
		return nil, fmt.Errorf("保存快照元数据失败: %w", err)
	}

	return o.mounts(key)
}

// View 创建一个只读视图 (用于 inspect)
func (o *OverlaySnapshotter) View(ctx context.Context, key, parent string) ([]snapshots.Mount, error) {
	snapDir := filepath.Join(o.root, key)
	if err := os.MkdirAll(snapDir, 0755); err != nil {
		return nil, fmt.Errorf("创建快照目录失败: %w", err)
	}

	diffDir := filepath.Join(snapDir, "diff")
	if err := os.MkdirAll(diffDir, 0755); err != nil {
		os.RemoveAll(snapDir)
		return nil, fmt.Errorf("创建 diff 目录失败: %w", err)
	}

	now := time.Now().Format(constants.TimeFormat)
	info := &metadata.SnapshotInfo{
		Name:      key,
		Parent:    parent,
		Kind:      metadata.KindActive,
		ReadWrite: false,
		CreatedAt: now,
		UpdatedAt: now,
	}

	if err := o.db.Update(func(tx *bolt.Tx) error {
		return metadata.SaveSnapshot(tx, info)
	}); err != nil {
		os.RemoveAll(snapDir)
		return nil, fmt.Errorf("保存快照元数据失败: %w", err)
	}

	return o.mounts(key)
}

// Commit 将可写快照提交为只读快照
func (o *OverlaySnapshotter) Commit(ctx context.Context, name, key string) error {
	var snapInfo *metadata.SnapshotInfo
	if err := o.db.View(func(tx *bolt.Tx) error {
		var err error
		snapInfo, err = metadata.LoadSnapshot(tx, key)
		return err
	}); err != nil {
		return fmt.Errorf("加载快照 %s 失败: %w", key, err)
	}

	if snapInfo.Kind != metadata.KindActive {
		return fmt.Errorf("快照 %s 不是 Active 快照，无法 Commit", key)
	}

	snapDir := filepath.Join(o.root, key)
	diffDir := filepath.Join(snapDir, "diff")
	upperDir := filepath.Join(snapDir, "upper")

	if _, err := os.Stat(upperDir); err == nil {
		if err := mergeUpperToDiff(upperDir, diffDir); err != nil {
			return fmt.Errorf("合并 upper 到 diff 失败: %w", err)
		}
		os.RemoveAll(upperDir)
	}

	workDir := filepath.Join(snapDir, "work")
	os.RemoveAll(workDir)

	mergedDir := filepath.Join(snapDir, "merged")
	os.RemoveAll(mergedDir)

	now := time.Now().Format(constants.TimeFormat)
	snapInfo.Kind = metadata.KindCommitted
	snapInfo.ReadWrite = false
	snapInfo.UpdatedAt = now
	if name != key {
		snapInfo.Name = name
	}

	if err := o.db.Update(func(tx *bolt.Tx) error {
		if name != key {
			if err := metadata.DeleteSnapshot(tx, key); err != nil {
				return err
			}
		}
		return metadata.SaveSnapshot(tx, snapInfo)
	}); err != nil {
		return fmt.Errorf("更新快照元数据失败: %w", err)
	}

	if name != key {
		newDir := filepath.Join(o.root, name)
		if err := os.Rename(snapDir, newDir); err != nil {
			return fmt.Errorf("重命名快照目录失败: %w", err)
		}
	}

	return nil
}

// Mounts 获取快照的挂载信息
func (o *OverlaySnapshotter) Mounts(ctx context.Context, key string) ([]snapshots.Mount, error) {
	return o.mounts(key)
}

// Remove 删除快照
func (o *OverlaySnapshotter) Remove(ctx context.Context, key string) error {
	if err := o.db.Update(func(tx *bolt.Tx) error {
		return metadata.DeleteSnapshot(tx, key)
	}); err != nil {
		return fmt.Errorf("删除快照元数据失败: %w", err)
	}

	snapDir := filepath.Join(o.root, key)
	if err := os.RemoveAll(snapDir); err != nil {
		return fmt.Errorf("删除快照目录失败: %w", err)
	}

	return nil
}

// Walk 遍历所有快照
func (o *OverlaySnapshotter) Walk(ctx context.Context, fn func(snapshots.Info) error) error {
	return o.db.View(func(tx *bolt.Tx) error {
		return metadata.WalkSnapshots(tx, func(info *metadata.SnapshotInfo) error {
			return fn(snapshots.Info{
				Name:      info.Name,
				Parent:    info.Parent,
				Kind:      snapshots.Kind(info.Kind),
				ReadWrite: info.ReadWrite,
				CreatedAt: info.CreatedAt,
				UpdatedAt: info.UpdatedAt,
				Labels:    info.Labels,
			})
		})
	})
}

// Close 关闭 Snapshotter（目前为空操作）
func (o *OverlaySnapshotter) Close() error {
	return nil
}

// mounts 构建快照的挂载信息
func (o *OverlaySnapshotter) mounts(key string) ([]snapshots.Mount, error) {
	var snapInfo *metadata.SnapshotInfo
	if err := o.db.View(func(tx *bolt.Tx) error {
		var err error
		snapInfo, err = metadata.LoadSnapshot(tx, key)
		return err
	}); err != nil {
		return nil, fmt.Errorf("加载快照 %s 失败: %w", key, err)
	}

	snapDir := filepath.Join(o.root, snapInfo.Name)
	diffDir := filepath.Join(snapDir, "diff")

	if snapInfo.Kind == metadata.KindCommitted {
		return []snapshots.Mount{
			{
				Type:   "bind",
				Source: diffDir,
				Options: []string{
					"ro",
					"bind",
				},
			},
		}, nil
	}

	var options []string

	if snapInfo.Parent != "" {
		lowers, err := o.lowerDirs(snapInfo.Parent)
		if err != nil {
			return nil, fmt.Errorf("收集 lowerdir 失败: %w", err)
		}
		if len(lowers) > 0 {
			options = append(options, fmt.Sprintf("lowerdir=%s", strings.Join(lowers, ":")))
		}
	}

	if snapInfo.ReadWrite {
		upperDir := filepath.Join(snapDir, "upper")
		workDir := filepath.Join(snapDir, "work")
		options = append(options,
			fmt.Sprintf("upperdir=%s", upperDir),
			fmt.Sprintf("workdir=%s", workDir),
		)
	}

	return []snapshots.Mount{
		{
			Type:    "overlay",
			Source:  "overlay",
			Options: options,
		},
	}, nil
}

// lowerDirs 递归收集所有父快照的 diff/ 目录，从底层到顶层
// OverlayFS 的 lowerdir 顺序：最远祖先在前，最近父在后
func (o *OverlaySnapshotter) lowerDirs(key string) ([]string, error) {
	var dirs []string
	current := key

	for current != "" {
		var snapInfo *metadata.SnapshotInfo
		if err := o.db.View(func(tx *bolt.Tx) error {
			var err error
			snapInfo, err = metadata.LoadSnapshot(tx, current)
			return err
		}); err != nil {
			return nil, fmt.Errorf("加载快照 %s 失败: %w", current, err)
		}

		diffDir := filepath.Join(o.root, snapInfo.Name, "diff")
		dirs = append(dirs, diffDir)
		current = snapInfo.Parent
	}

	for i, j := 0, len(dirs)-1; i < j; i, j = i+1, j-1 {
		dirs[i], dirs[j] = dirs[j], dirs[i]
	}

	return dirs, nil
}

// mergeUpperToDiff 将 upper 层的文件合并到 diff 目录
// 对齐 image.go 中的 mergeUpperToDiff 逻辑
func mergeUpperToDiff(upperDir, diffDir string) error {
	return filepath.Walk(upperDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}

		rel, relErr := filepath.Rel(upperDir, path)
		if relErr != nil || rel == "." {
			return nil
		}

		targetPath := filepath.Join(diffDir, rel)

		if info.IsDir() {
			os.MkdirAll(targetPath, info.Mode())
			return nil
		}

		if info.Mode()&os.ModeSymlink != 0 {
			linkTarget, readErr := os.Readlink(path)
			if readErr != nil {
				return nil
			}
			os.MkdirAll(filepath.Dir(targetPath), 0755)
			os.Remove(targetPath)
			os.Symlink(linkTarget, targetPath)
			return nil
		}

		os.MkdirAll(filepath.Dir(targetPath), 0755)

		srcFile, srcErr := os.Open(path)
		if srcErr != nil {
			return nil
		}
		defer srcFile.Close()

		dstFile, dstErr := os.OpenFile(targetPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode())
		if dstErr != nil {
			return nil
		}
		defer dstFile.Close()

		if _, err := io.Copy(dstFile, srcFile); err != nil {
			return fmt.Errorf("复制文件 %s 失败: %w", rel, err)
		}

		return nil
	})
}
