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
	"mini-docker/containerd/content"
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
      └── merged/     ← OverlayFS merged (仅 Active，挂载点)

  OverlayFS 挂载原理：
  - lowerdir: 只读层，多个用 ":" 分隔，从左到右优先级递增
    （最远祖先在前，最近父在后）
  - upperdir: 可写层，所有修改写入此目录
  - workdir: OverlayFS 内部使用的工作目录

  Commit 语义（对齐 containerd）：
  - Commit(ctx, name, key) 创建一个新的 Committed 快照（name），
    源 Active 快照（key）保持不变
  - 调用方决定是否 Remove 源快照
  - 这保证了：
    1. 同一个 Active 快照可以 commit 多次
    2. commit 失败不影响源快照
    3. 支持快照分支（同一 parent 创建多个子快照）

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
// 给定容器 ID + 镜像顶层层的 cacheID，在 overlay 上创建了三个目录（diff/upper/work）
func (o *OverlaySnapshotter) Prepare(ctx context.Context, key, parent string) ([]snapshots.Mount, error) {
	snapDir := filepath.Join(o.root, key) //key是容器id
	if err := os.MkdirAll(snapDir, 0755); err != nil {
		return nil, fmt.Errorf("创建快照目录失败: %w", err)
	}
	//在 <root>/<containerID>/ 下创建 overlay 三件套 目录（diff/upper/work）
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
	//parent 参数的意义
	//调用 Prepare(containerID, parent) 时传入的 parent 是 镜像最顶层的 cacheID （ content.DigestToCacheID(layerDigests[len-1]) ）。它至少承担三个职责：
	//1. 构造 lowerdir 链 — Snapshotter.Mounts(ctx, containerID) 会沿 parent 链向上递归，把所有镜像层的 diff/ 路径拼起来作为 lowerdir=
	//2. GC 跟踪 — GC 看到容器的 snapshot Parent=<顶层镜像层 cacheID> ，就知道"这个容器在用这条 layer 链，删镜像时这些层要保护"
	//3. 关联回退路径 — markSnapshotParents 在 GC 时会沿 parent 链把所有祖先都标为可达
	now := time.Now().Format(constants.TimeFormat)
	info := &snapshots.Info{
		Name:      key,    //key是容器id
		Parent:    parent, //Parent=<顶层镜像层 cacheID>
		Kind:      snapshots.KindActive,
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

// Commit 将可写快照提交为只读快照
// 对齐 containerd: 创建新的 Committed 快照，源 Active 快照保持不变
// name: 新快照的名称
// key: 源 Active 快照的名称
// 调用方决定是否 Remove 源快照（key）
func (o *OverlaySnapshotter) Commit(ctx context.Context, name, key string) error {
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

	// 创建新的 Committed 快照目录
	newDir := filepath.Join(o.root, name)
	newDiffDir := filepath.Join(newDir, "diff")
	if err := os.MkdirAll(newDiffDir, 0755); err != nil {
		return fmt.Errorf("创建 Committed 快照目录失败: %w", err)
	}

	// 将源快照的 diff/ 内容复制到新快照的 diff/
	srcDiffDir := filepath.Join(o.root, key, "diff")
	if err := mergeDir(srcDiffDir, newDiffDir, false); err != nil {
		os.RemoveAll(newDir)
		return fmt.Errorf("复制 diff 目录失败: %w", err)
	}

	// 将源快照的 upper/ 内容合并到新快照的 diff/
	srcUpperDir := filepath.Join(o.root, key, "upper")
	if _, err := os.Stat(srcUpperDir); err == nil {
		if err := MergeUpperToDiff(srcUpperDir, newDiffDir); err != nil {
			os.RemoveAll(newDir)
			return fmt.Errorf("合并 upper 到 diff 失败: %w", err)
		}
	}

	// 保存新快照的元数据
	now := time.Now().Format(constants.TimeFormat)
	committedInfo := &snapshots.Info{
		Name:      name,
		Parent:    snapInfo.Parent,
		Kind:      snapshots.KindCommitted,
		ReadWrite: false,
		CreatedAt: now,
		UpdatedAt: now,
		Labels:    snapInfo.Labels,
	}

	if err := o.db.Update(func(tx *bolt.Tx) error {
		return metadata.SaveSnapshot(tx, committedInfo)
	}); err != nil {
		os.RemoveAll(newDir)
		return fmt.Errorf("保存 Committed 快照元数据失败: %w", err)
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
		return metadata.WalkSnapshots(tx, func(info *snapshots.Info) error {
			return fn(*info)
		})
	})
}

// DiffPath 返回指定快照的 diff 目录路径
// 对齐 containerd: 通过 Snapshotter 接口获取层路径，而非直接拼接常量路径
func (o *OverlaySnapshotter) DiffPath(ctx context.Context, key string) (string, error) {
	return filepath.Join(o.root, key, "diff"), nil
}

// Close 关闭 Snapshotter（目前为空操作）
func (o *OverlaySnapshotter) Close() error {
	return nil
}

// RegisterCommitted 注册一个已存在的目录为 Committed 快照
// 对齐 containerd: 镜像解压（Unpack）时，StoreLayer 已将层解压到 snapshots/overlay/<key>/diff/，
// 但 boltdb 中没有对应的 SnapshotInfo。本方法补注册 SnapshotInfo，建立 parent 链，
// 使 Snapshotter 的 lowerDirs() 能递归构建多层 lowerdir。
// 注意：新代码应优先使用 UnpackLayer，RegisterCommitted 仅用于兼容已有 diff/ 目录的补注册场景
// key: 快照名称（通常为层的 cacheID）
// parent: 父快照名称（上一层的 cacheID，基础层为空）
func (o *OverlaySnapshotter) RegisterCommitted(ctx context.Context, key, parent string) error {
	// 验证 diff/ 目录存在
	diffDir := filepath.Join(o.root, key, "diff")
	if _, err := os.Stat(diffDir); err != nil {
		return fmt.Errorf("快照 %s 的 diff 目录不存在: %w", key, err)
	}

	now := time.Now().Format(constants.TimeFormat)
	info := &snapshots.Info{
		Name:      key,
		Parent:    parent,
		Kind:      snapshots.KindCommitted,
		ReadWrite: false,
		CreatedAt: now,
		UpdatedAt: now,
	}

	if err := o.db.Update(func(tx *bolt.Tx) error {
		return metadata.SaveSnapshot(tx, info)
	}); err != nil {
		return fmt.Errorf("注册 Committed 快照 %s 失败: %w", key, err)
	}

	return nil
}

// UnpackLayer 解压 tar.gz blob 到快照目录并注册为 Committed 快照
// 对齐 containerd: 将"文件解压"和"元数据注册"合并为原子操作，
// 避免分两步执行时崩溃导致"文件存在但元数据缺失"的不一致状态。
// 替代之前的 LayerStore.StoreLayer + Snapshotter.RegisterCommitted 两步操作。
// blobPath: tar.gz 文件的路径
// digest: 该层的压缩 digest (sha256:...)，用于生成 cacheID
// diffID: 该层的未压缩 digest (sha256:...)，用于校验解压后数据的完整性
// parent: 父快照的 key（上一层的 cacheID，基础层为空）
// 返回值: cacheID（层的快照标识），由调用方用于关联层 digest
func (o *OverlaySnapshotter) UnpackLayer(ctx context.Context, blobPath, digest, diffID, parent string) (string, error) {
	cacheID := content.DigestToCacheID(digest)
	diffDir := filepath.Join(o.root, cacheID, "diff")

	// 缓存命中：diff/ 目录已存在且元数据已注册
	if _, err := os.Stat(diffDir); err == nil {
		// 检查元数据是否也已存在（可能之前崩溃导致文件存在但元数据缺失）
		var metaExists bool
		o.db.View(func(tx *bolt.Tx) error {
			_, err := metadata.LoadSnapshot(tx, cacheID)
			metaExists = err == nil
			return nil
		})
		if metaExists {
			return cacheID, nil
		}
		// 文件存在但元数据缺失，补注册元数据
		now := time.Now().Format(constants.TimeFormat)
		info := &snapshots.Info{
			Name:      cacheID,
			Parent:    parent, //这个层快照的父快照的cacheID（上一层的 cacheID，基础层为空）
			Kind:      snapshots.KindCommitted,
			ReadWrite: false,
			CreatedAt: now,
			UpdatedAt: now,
		}
		if err := o.db.Update(func(tx *bolt.Tx) error {
			return metadata.SaveSnapshot(tx, info)
		}); err != nil {
			return "", fmt.Errorf("补注册 Committed 快照 %s 失败: %w", cacheID, err)
		}
		return cacheID, nil
	}

	// 创建 diff/ 目录
	if err := os.MkdirAll(diffDir, 0755); err != nil {
		return "", fmt.Errorf("创建层 diff 目录失败: %w", err)
	}

	// 解压 tar.gz 到 diff/，同时计算 DiffID 校验
	actualDiffID, err := extractLayerBlob(blobPath, diffDir)
	if err != nil {
		os.RemoveAll(filepath.Join(o.root, cacheID))
		return "", fmt.Errorf("解压层失败: %w", err)
	}

	// 对齐 containerd: 校验解压后的 DiffID，确保数据完整性
	// diffID 必传：上层调用方负责从 OCI Config 的 RootFS.DiffIDs 拿
	if diffID == "" {
		os.RemoveAll(filepath.Join(o.root, cacheID))
		return "", fmt.Errorf("UnpackLayer: diffID 不能为空（必须从 OCI Config 的 RootFS.DiffIDs 提供，用于解压后完整性校验）")
	}
	if actualDiffID != diffID {
		os.RemoveAll(filepath.Join(o.root, cacheID))
		return "", fmt.Errorf("DiffID 校验失败: 期望 %s, 实际 %s", diffID, actualDiffID)
	}

	// 解压成功后立即注册元数据，保证文件与元数据的一致性
	now := time.Now().Format(constants.TimeFormat)
	info := &snapshots.Info{
		Name:      cacheID,
		Parent:    parent,
		Kind:      snapshots.KindCommitted,
		ReadWrite: false,
		CreatedAt: now,
		UpdatedAt: now,
	}

	if err := o.db.Update(func(tx *bolt.Tx) error {
		return metadata.SaveSnapshot(tx, info)
	}); err != nil {
		// 元数据注册失败，回滚文件以保持一致性
		os.RemoveAll(filepath.Join(o.root, cacheID))
		return "", fmt.Errorf("注册 Committed 快照 %s 失败: %w", cacheID, err)
	}

	return cacheID, nil
}

// mounts 构建快照的挂载信息
func (o *OverlaySnapshotter) mounts(key string) ([]snapshots.Mount, error) {
	var snapInfo *snapshots.Info
	if err := o.db.View(func(tx *bolt.Tx) error {
		var err error
		snapInfo, err = metadata.LoadSnapshot(tx, key)
		return err
	}); err != nil {
		return nil, fmt.Errorf("加载快照 %s 失败: %w", key, err)
	}

	snapDir := filepath.Join(o.root, snapInfo.Name)
	diffDir := filepath.Join(snapDir, "diff")

	if snapInfo.Kind == snapshots.KindCommitted {
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
			options = append(options, fmt.Sprintf("lowerdir=%s", strings.Join(lowers, ":"))) //把镜像层的目录用：拼接到了一起
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
		var snapInfo *snapshots.Info
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

// mergeDir 递归合并目录内容
// 对齐 containerd: Commit 创建新快照时需要复制源快照的 diff/ 内容
// continueOnError: true 时遇到错误静默跳过（用于 upper→diff 合并），false 时返回错误（用于快照复制）
func mergeDir(srcDir, dstDir string, continueOnError bool) error {
	return filepath.Walk(srcDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			if continueOnError {
				return nil
			}
			return err
		}

		rel, relErr := filepath.Rel(srcDir, path)
		if relErr != nil || rel == "." {
			return nil
		}

		targetPath := filepath.Join(dstDir, rel)

		if info.IsDir() {
			return os.MkdirAll(targetPath, info.Mode())
		}

		if info.Mode()&os.ModeSymlink != 0 {
			linkTarget, readErr := os.Readlink(path)
			if readErr != nil {
				if continueOnError {
					return nil
				}
				return readErr
			}
			os.MkdirAll(filepath.Dir(targetPath), 0755)
			os.Remove(targetPath)
			return os.Symlink(linkTarget, targetPath)
		}

		os.MkdirAll(filepath.Dir(targetPath), 0755)

		srcFile, srcErr := os.Open(path)
		if srcErr != nil {
			if continueOnError {
				return nil
			}
			return srcErr
		}
		defer srcFile.Close()

		dstFile, dstErr := os.OpenFile(targetPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode())
		if dstErr != nil {
			if continueOnError {
				return nil
			}
			return dstErr
		}
		defer dstFile.Close()

		if _, err := io.Copy(dstFile, srcFile); err != nil {
			return fmt.Errorf("复制文件 %s 失败: %w", rel, err)
		}

		return nil
	})
}

// MergeUpperToDiff 将 upper 层的文件合并到 diff 目录
// 对齐 containerd: Commit 时将 Active 快照的 upper 层内容合并到新快照的 diff/
// 导出此函数供 image 包的 CreateLayerFromDir 等场景复用，避免重复实现
func MergeUpperToDiff(upperDir, diffDir string) error {
	return mergeDir(upperDir, diffDir, true)
}
