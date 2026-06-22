package overlay

import (
	"context"
	"fmt"
	"log"
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
  OverlayFS Snapshotter —— 对齐真实 containerd 的 overlay snapshotter
=======================================================================

  存储结构：
  /var/lib/mini-docker/snapshots/overlay/
  └── snapshots/
      └── <id>/          ← 数字 ID 命名（1, 2, 3, ...）
          ├── fs/        ← 文件系统内容
          └── work/      ← OverlayFS work 目录（仅 Active 快照）

  fs/ 的双重用途：
  - Active 有 parent: 作为 upperdir（overlay 可写层）
  - Active 无 parent: 作为 bind mount 源（rw）
  - Committed: 包含层差异内容（提交时 upperdir 的内容）

  内部 ID 系统：
  - 每个快照分配自增数字 ID（通过 metadata.NextSnapshotID）
  - 目录以 ID 命名：<root>/snapshots/<id>/
  - key→ID 映射存储在元数据中（Info.Name=key, Info.ID=id）
  - Commit 是纯元数据操作：只改变 key 映射，目录不变

  核心生命周期（对齐 containerd 的 Prepare-Apply-Commit 循环）：
  1. Prepare(key, parent) → 创建 Active 可写快照，返回 mount 信息
  2. 外部 Applier 将层差异应用到 Active 快照的挂载点
  3. Commit(name, key) → 将 Active 快照提交为 Committed 快照（元数据操作，目录不变）

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
	// 创建 snapshots 子目录
	snapshotsDir := filepath.Join(root, "snapshots")
	if err := os.MkdirAll(snapshotsDir, 0755); err != nil {
		return nil, fmt.Errorf("创建 snapshots 子目录失败: %w", err)
	}
	return &OverlaySnapshotter{
		root: root,
		db:   db,
	}, nil
}

// fsPath 返回快照的 fs 目录路径：<root>/snapshots/<id>/fs
func (o *OverlaySnapshotter) fsPath(id string) string {
	return filepath.Join(o.root, "snapshots", id, "fs")
}

// workPath 返回快照的 work 目录路径：<root>/snapshots/<id>/work
func (o *OverlaySnapshotter) workPath(id string) string {
	return filepath.Join(o.root, "snapshots", id, "work")
}

// snapDir 返回快照目录路径：<root>/snapshots/<id>
func (o *OverlaySnapshotter) snapDir(id string) string {
	return filepath.Join(o.root, "snapshots", id)
}

// Prepare 创建一个可写快照 (KindActive)，用于容器运行或镜像解包
// key: 快照唯一标识 (如 container-id 或 cacheID)
// parent: 父快照的 key (空则无父)
// opts: 可选参数（如 WithLabels）
func (o *OverlaySnapshotter) Prepare(ctx context.Context, key, parent string, opts ...snapshots.Opt) ([]snapshots.Mount, error) {
	return o.createSnapshot(ctx, snapshots.KindActive, key, parent, opts...)
}

// View 创建一个只读活跃快照 (KindView)，无 upperdir/workdir
// 用于挂载查看镜像内容等只读场景
func (o *OverlaySnapshotter) View(ctx context.Context, key, parent string, opts ...snapshots.Opt) ([]snapshots.Mount, error) {
	return o.createSnapshot(ctx, snapshots.KindView, key, parent, opts...)
}

// createSnapshot 内部完成三件事：
// 1. 创建目录 ： <root>/snapshots/<id>/ 下生成 fs/ （可写层/内容层）和 work/ （overlay 内部工作目录）；
// 2. 写入 BoltDB ：注册一条 SnapshotInfo ，关键字段是 Parent = topLayerSnapshotID ，这就把容器层"挂"到镜像层链上；
// 3. 返回 Mount ：描述如何在宿主机上把上层可写 + 下层只读组合挂载成 rootfs。
func (o *OverlaySnapshotter) createSnapshot(ctx context.Context, kind snapshots.Kind, key, parent string, opts ...snapshots.Opt) ([]snapshots.Mount, error) {
	// 构建快照信息
	now := time.Now().Format(constants.TimeFormat)
	info := &snapshots.Info{
		Name:      key,
		Parent:    parent,
		Kind:      kind,
		CreatedAt: now,
		UpdatedAt: now,
	}
	// 应用选项
	for _, opt := range opts {
		if err := opt(info); err != nil {
			return nil, fmt.Errorf("应用快照选项失败: %w", err)
		}
	}

	var parentIDs []string

	// 查找父快照信息，获取 ParentIDs
	if parent != "" {
		var parentInfo *snapshots.Info
		if err := o.db.View(func(tx *bolt.Tx) error {
			var err error
			parentInfo, err = metadata.LoadSnapshot(tx, parent)
			return err
		}); err != nil {
			return nil, fmt.Errorf("加载父快照 %s 失败: %w", parent, err)
		}
		// 继承父快照的 ParentIDs，并把父快照自身的 ID 放在最前面。
		// ParentIDs 顺序与 containerd 一致：最近父层在前，最远祖先在后，
		// 这样直接 join 即可得到 overlayfs 要求的 lowerdir（越靠前越上层）。
		parentIDs = append([]string{parentInfo.ID}, parentInfo.ParentIDs...)
	}

	// 在 boltDB 事务中分配 ID 并保存元数据
	var id string
	if err := o.db.Update(func(tx *bolt.Tx) error {
		// 检查 key 是否已存在，避免覆盖（对齐 containerd: ErrAlreadyExists）
		if _, err := metadata.LoadSnapshot(tx, key); err == nil {
			return fmt.Errorf("快照 %s 已存在", key)
		}

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

	// 使用临时目录 + rename 实现原子性
	// 先在 snapshots/ 下创建临时目录
	snapshotsDir := filepath.Join(o.root, "snapshots")
	tmpDir, err := os.MkdirTemp(snapshotsDir, "tmp-")
	if err != nil {
		// 元数据已保存，尝试清理
		o.db.Update(func(tx *bolt.Tx) error {
			metadata.DeleteSnapshot(tx, key)
			return nil
		})
		return nil, fmt.Errorf("创建临时目录失败: %w", err)
	}

	// 创建 fs/ 目录
	if err := os.MkdirAll(filepath.Join(tmpDir, "fs"), 0755); err != nil {
		os.RemoveAll(tmpDir)
		o.db.Update(func(tx *bolt.Tx) error {
			metadata.DeleteSnapshot(tx, key)
			return nil
		})
		return nil, fmt.Errorf("创建 fs 目录失败: %w", err)
	}

	// Active 快照需要 work/ 目录
	if kind == snapshots.KindActive {
		if err := os.MkdirAll(filepath.Join(tmpDir, "work"), 0755); err != nil {
			os.RemoveAll(tmpDir)
			o.db.Update(func(tx *bolt.Tx) error {
				metadata.DeleteSnapshot(tx, key)
				return nil
			})
			return nil, fmt.Errorf("创建 work 目录失败: %w", err)
		}
	}

	// 原子重命名：临时目录 → 最终路径 <root>/snapshots/<id>/
	finalDir := o.snapDir(id)
	if err := os.Rename(tmpDir, finalDir); err != nil {
		os.RemoveAll(tmpDir)
		o.db.Update(func(tx *bolt.Tx) error {
			metadata.DeleteSnapshot(tx, key)
			return nil
		})
		return nil, fmt.Errorf("重命名快照目录失败: %w", err)
	}

	return o.mounts(key)
}

// Commit 将 Active 快照提交为 Committed 快照
// 对齐 containerd: 纯元数据操作，目录不变
// name: 新 Committed 快照的名称
// key: 源 Active 快照的名称（提交后该 key 被消费，不再可用）
func (o *OverlaySnapshotter) Commit(ctx context.Context, name, key string, opts ...snapshots.Opt) error {
	var snapInfo *snapshots.Info
	if err := o.db.View(func(tx *bolt.Tx) error {
		var err error
		snapInfo, err = metadata.LoadSnapshot(tx, key)
		return err
	}); err != nil {
		return fmt.Errorf("加载快照 %s 失败: %w", key, err)
	}

	if snapInfo.Kind != snapshots.KindActive {
		return fmt.Errorf("快照 %s 不是 Active 快照，无法 Commit", key)
	}

	// 构建提交后的快照信息
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
	// 应用选项（可覆盖 labels 等）
	for _, opt := range opts {
		if err := opt(committedInfo); err != nil {
			return fmt.Errorf("应用 Commit 选项失败: %w", err)
		}
	}

	// 计算磁盘使用量并保存
	usage, err := o.diskUsage(committedInfo.ID)
	if err == nil {
		// 将使用量存入 labels（containerd 也类似地在元数据中保存 usage）
		if committedInfo.Labels == nil {
			committedInfo.Labels = make(map[string]string)
		}
		committedInfo.Labels["usage.size"] = fmt.Sprintf("%d", usage.Size)
		committedInfo.Labels["usage.inodes"] = fmt.Sprintf("%d", usage.Inodes)
	}

	// 纯元数据操作：删除旧 key 映射，创建新 key 映射
	// 目录完全不变
	if err := o.db.Update(func(tx *bolt.Tx) error {
		// 检查目标 name 是否已存在，避免覆盖（对齐 containerd: ErrAlreadyExists）
		if _, err := metadata.LoadSnapshot(tx, name); err == nil {
			return fmt.Errorf("快照 %s 已存在", name)
		}

		metadata.DeleteSnapshot(tx, key)
		return metadata.SaveSnapshot(tx, committedInfo)
	}); err != nil {
		return fmt.Errorf("提交快照元数据失败: %w", err)
	}

	return nil
}

// Mounts 获取快照的挂载信息
func (o *OverlaySnapshotter) Mounts(ctx context.Context, key string) ([]snapshots.Mount, error) {
	return o.mounts(key)
}

// mounts 构建快照的挂载信息
// 对齐 containerd 的挂载语义：
//   - Active 有 parent: overlay mount (lowerdir + upperdir + workdir)
//   - Active 无 parent: bind mount (rw)
//   - Committed 有 parent: overlay mount (lowerdir only)
//   - Committed 无 parent: bind mount (ro)
//   - View 有 parent: overlay mount (lowerdir only, read-only)
//   - View 无 parent: bind mount (ro)
func (o *OverlaySnapshotter) mounts(key string) ([]snapshots.Mount, error) {
	var snapInfo *snapshots.Info
	if err := o.db.View(func(tx *bolt.Tx) error {
		var err error
		snapInfo, err = metadata.LoadSnapshot(tx, key)
		return err
	}); err != nil {
		return nil, fmt.Errorf("加载快照 %s 失败: %w", key, err)
	}

	switch snapInfo.Kind {
	case snapshots.KindActive:
		if snapInfo.Parent != "" {
			// Active 有 parent: overlay mount (rw)
			lowerDirs := o.parentDirs(snapInfo)
			var options []string
			options = append(options, fmt.Sprintf("lowerdir=%s", strings.Join(lowerDirs, ":")))
			options = append(options, fmt.Sprintf("upperdir=%s", o.fsPath(snapInfo.ID)))
			options = append(options, fmt.Sprintf("workdir=%s", o.workPath(snapInfo.ID)))
			return []snapshots.Mount{
				{
					Type:    "overlay",
					Source:  "overlay",
					Options: options,
				},
			}, nil
		}
		// Active 无 parent: bind mount (rw)
		return []snapshots.Mount{
			{
				Type:    "bind",
				Source:  o.fsPath(snapInfo.ID),
				Options: []string{"rw", "rbind"},
			},
		}, nil

	case snapshots.KindCommitted:
		if snapInfo.Parent != "" {
			// Committed 有 parent: overlay mount (lowerdir only)
			// 自身 fs/ 作为最上层 lowerdir，父链按 ParentIDs 顺序（近→远）跟在后面。
			lowerDirs := append([]string{o.fsPath(snapInfo.ID)}, o.parentDirs(snapInfo)...)
			var options []string
			options = append(options, fmt.Sprintf("lowerdir=%s", strings.Join(lowerDirs, ":")))
			return []snapshots.Mount{
				{
					Type:    "overlay",
					Source:  "overlay",
					Options: options,
				},
			}, nil
		}
		// Committed 无 parent: bind mount (ro)
		return []snapshots.Mount{
			{
				Type:    "bind",
				Source:  o.fsPath(snapInfo.ID),
				Options: []string{"ro", "rbind"},
			},
		}, nil

	case snapshots.KindView:
		if snapInfo.Parent != "" {
			// View 有 parent: overlay mount (lowerdir only, read-only)
			lowerDirs := o.parentDirs(snapInfo)
			var options []string
			options = append(options, fmt.Sprintf("lowerdir=%s", strings.Join(lowerDirs, ":")))
			return []snapshots.Mount{
				{
					Type:    "overlay",
					Source:  "overlay",
					Options: options,
				},
			}, nil
		}
		// View 无 parent: bind mount (ro)
		return []snapshots.Mount{
			{
				Type:    "bind",
				Source:  o.fsPath(snapInfo.ID),
				Options: []string{"ro", "rbind"},
			},
		}, nil

	default:
		return nil, fmt.Errorf("未知的快照类型: %d", snapInfo.Kind)
	}
}

// parentDirs 使用预计算的 ParentIDs 构建父链 fs/ 目录列表
// 返回顺序：最近父层在前，最远祖先在后（OverlayFS lowerdir 的要求）
// 无需递归遍历父链，直接使用 ParentIDs 即可
func (o *OverlaySnapshotter) parentDirs(info *snapshots.Info) []string {
	if len(info.ParentIDs) == 0 {
		return nil
	}
	dirs := make([]string, len(info.ParentIDs))
	for i, id := range info.ParentIDs {
		dirs[i] = o.fsPath(id)
	}
	return dirs
}

// Remove 删除快照
// 先检查是否有子快照引用，再删除元数据，最后删除磁盘目录；
// 目录删除失败时由 Cleanup 后续清理，避免元数据残留导致快照处于僵尸状态。
// 对齐真实 containerd: 元数据先移除，磁盘资源可异步清理。
func (o *OverlaySnapshotter) Remove(ctx context.Context, key string) error {
	var snapInfo *snapshots.Info
	var childCount int
	if err := o.db.View(func(tx *bolt.Tx) error {
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

	// 先删除元数据（对齐 containerd）
	if err := o.db.Update(func(tx *bolt.Tx) error {
		return metadata.DeleteSnapshot(tx, key)
	}); err != nil {
		return fmt.Errorf("删除快照 %s 元数据失败: %w", key, err)
	}

	// 再删除磁盘目录；失败时记录日志，由 Cleanup 异步清理
	snapPath := o.snapDir(snapInfo.ID)
	if err := os.RemoveAll(snapPath); err != nil {
		log.Printf("警告: 快照 %s 元数据已删除，但目录 %s 清理失败: %v；将由 Cleanup 处理", key, snapPath, err)
	}

	return nil
}

// Stat 返回指定快照的元信息
func (o *OverlaySnapshotter) Stat(ctx context.Context, key string) (snapshots.Info, error) {
	var snapInfo *snapshots.Info
	if err := o.db.View(func(tx *bolt.Tx) error {
		var err error
		snapInfo, err = metadata.LoadSnapshot(tx, key)
		return err
	}); err != nil {
		return snapshots.Info{}, fmt.Errorf("加载快照 %s 失败: %w", key, err)
	}
	return *snapInfo, nil
}

// Update 更新快照的元信息（如 labels 等）
// fieldpaths: 指定要更新的字段路径，为空则更新所有可变字段
func (o *OverlaySnapshotter) Update(ctx context.Context, info snapshots.Info, fieldpaths ...string) (snapshots.Info, error) {
	var snapInfo *snapshots.Info
	if err := o.db.Update(func(tx *bolt.Tx) error {
		var err error
		snapInfo, err = metadata.LoadSnapshot(tx, info.Name)
		if err != nil {
			return err
		}

		// 根据 fieldpaths 更新字段
		if len(fieldpaths) == 0 {
			// 无指定字段时，更新所有可变字段
			snapInfo.Labels = info.Labels
		} else {
			// 按指定字段更新
			for _, path := range fieldpaths {
				switch path {
				case "labels":
					snapInfo.Labels = info.Labels
				case "labels.":
					snapInfo.Labels = info.Labels
				default:
					if strings.HasPrefix(path, "labels.") {
						key := strings.TrimPrefix(path, "labels.")
						if snapInfo.Labels == nil {
							snapInfo.Labels = make(map[string]string)
						}
						if info.Labels != nil {
							if v, ok := info.Labels[key]; ok {
								snapInfo.Labels[key] = v
							} else {
								delete(snapInfo.Labels, key)
							}
						} else {
							delete(snapInfo.Labels, key)
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
// Active: 扫描 fs/ 目录计算磁盘使用量
// Committed: 返回保存在 labels 中的使用量
func (o *OverlaySnapshotter) Usage(ctx context.Context, key string) (snapshots.Usage, error) {
	var snapInfo *snapshots.Info
	if err := o.db.View(func(tx *bolt.Tx) error {
		var err error
		snapInfo, err = metadata.LoadSnapshot(tx, key)
		return err
	}); err != nil {
		return snapshots.Usage{}, fmt.Errorf("加载快照 %s 失败: %w", key, err)
	}

	if snapInfo.Kind == snapshots.KindCommitted {
		// Committed: 从 labels 中读取保存的使用量
		usage := snapshots.Usage{}
		if v, ok := snapInfo.Labels["usage.size"]; ok {
			fmt.Sscanf(v, "%d", &usage.Size)
		}
		if v, ok := snapInfo.Labels["usage.inodes"]; ok {
			fmt.Sscanf(v, "%d", &usage.Inodes)
		}
		return usage, nil
	}

	// Active/View: 实时扫描 fs/ 目录
	return o.diskUsage(snapInfo.ID)
}

// diskUsage 扫描快照的 fs/ 目录，计算磁盘使用量
func (o *OverlaySnapshotter) diskUsage(id string) (snapshots.Usage, error) {
	fsDir := o.fsPath(id)
	var usage snapshots.Usage
	err := filepath.Walk(fsDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // 跳过无法访问的文件
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
// fn: 对每个快照调用的回调函数
// filters: 过滤条件（暂未实现，预留接口）
func (o *OverlaySnapshotter) Walk(ctx context.Context, fn snapshots.WalkFunc, filters ...string) error {
	return o.db.View(func(tx *bolt.Tx) error {
		return metadata.WalkSnapshots(tx, func(info *snapshots.Info) error {
			return fn(ctx, *info)
		})
	})
}

// Cleanup 清理已移除/废弃快照的磁盘资源
// 对齐 containerd: 扫描 <root>/snapshots/ 目录，
// 将不在 ID 映射中的目录视为孤立目录并删除
func (o *OverlaySnapshotter) Cleanup(ctx context.Context) error {
	// 获取数据库中所有有效的快照 ID
	idMap := make(map[string]struct{})
	if err := o.db.View(func(tx *bolt.Tx) error {
		m, err := metadata.GetSnapshotIDMap(tx)
		if err != nil {
			return err
		}
		idMap = m
		return nil
	}); err != nil {
		return fmt.Errorf("获取快照 ID 映射失败: %w", err)
	}

	// 扫描磁盘上的 snapshots/ 目录
	snapshotsDir := filepath.Join(o.root, "snapshots")
	entries, err := os.ReadDir(snapshotsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("读取 snapshots 目录失败: %w", err)
	}

	// 删除不在 ID 映射中的孤立目录
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		// 跳过临时目录（以 "tmp-" 开头）
		if strings.HasPrefix(entry.Name(), "tmp-") {
			continue
		}
		// 如果目录名不在 ID 映射中，视为孤立目录
		if _, exists := idMap[entry.Name()]; !exists {
			orphanPath := filepath.Join(snapshotsDir, entry.Name())
			if err := os.RemoveAll(orphanPath); err != nil {
				// 记录错误但继续清理其他孤立目录
				fmt.Printf("警告: 清理孤立目录 %s 失败: %v\n", orphanPath, err)
			}
		}
	}

	return nil
}

// Close 关闭 Snapshotter（目前为空操作）
func (o *OverlaySnapshotter) Close() error {
	return nil
}

// dirHasFiles 检查目录是否有文件（非空）
func dirHasFiles(dir string) bool {
	f, err := os.Open(dir)
	if err != nil {
		return false
	}
	defer f.Close()

	_, err = f.Readdirnames(1)
	return err == nil
}
