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

  核心生命周期（对齐 containerd 的 Prepare-Apply-Commit 循环）：
  1. Prepare(key, parent) → 创建 Active 快照（diff/ + upper/ + work/）
  2. Apply(digest, diffID, blobPath, key) → 将 tar.gz 解压到 Active 快照的 diff/，
     处理 whiteout 文件（.wh.* 删除对应文件，.wh..wh..opq 清空目录）
  3. Commit(key) → 原地提交：Active → Committed，删除 upper/work
  4. CommitAs(name, key) → 创建新快照：从 Active 快照创建新的 Committed 快照

  Commit 语义：
  - Commit(ctx, key) 原地提交（Pull/Build 流程）：
    合并 upper/ → diff/（如有），删除 upper/ + work/，更新元数据
    适用于镜像解包和构建流程，Active 快照直接变为 Committed
  - CommitAs(ctx, name, key) 创建新快照（容器 commit 场景）：
    创建新的 Committed 快照，源 Active 快照保持不变
    调用方决定是否 Remove 源快照

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
	snapDir := filepath.Join(o.root, key)
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

	now := time.Now().Format(constants.TimeFormat)
	info := &snapshots.Info{
		Name:      key,
		Parent:    parent,
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

// Apply 将层差异应用到 Active 快照
// 对齐 containerd: diff.Applier.Apply —— 将 blob 解压到 active snapshot 的 diff/ 目录
// 同时处理 OCI whiteout 文件：
//   - .wh.<name> → 删除父层中对应的文件/目录
//   - .wh..wh..opq → 不透明白化，清空父层中该目录的所有内容
func (o *OverlaySnapshotter) Apply(ctx context.Context, digest, diffID, blobPath, key string) error {
	// 验证快照存在且为 Active
	var snapInfo *snapshots.Info
	if err := o.db.View(func(tx *bolt.Tx) error {
		var err error
		snapInfo, err = metadata.LoadSnapshot(tx, key)
		return err
	}); err != nil {
		return fmt.Errorf("加载快照 %s 失败: %w", key, err)
	}

	if snapInfo.Kind != snapshots.KindActive {
		return fmt.Errorf("快照 %s 不是 Active 快照，无法 Apply", key)
	}

	diffDir := filepath.Join(o.root, key, "diff")

	// 收集父层的 diff 目录路径，用于处理 whiteout
	var parentDiffDirs []string
	if snapInfo.Parent != "" {
		var err error
		parentDiffDirs, err = o.lowerDirs(snapInfo.Parent)
		if err != nil {
			return fmt.Errorf("收集父层路径失败: %w", err)
		}
	}

	// 解压 tar.gz 到 diff/，同时计算 DiffID 校验
	actualDiffID, err := extractLayerBlob(blobPath, diffDir)
	if err != nil {
		return fmt.Errorf("解压层失败: %w", err)
	}

	// 校验 DiffID
	if diffID != "" && actualDiffID != diffID {
		return fmt.Errorf("DiffID 校验失败: 期望 %s, 实际 %s", diffID, actualDiffID)
	}

	// 处理 whiteout 文件
	if err := processWhiteouts(diffDir, parentDiffDirs); err != nil {
		return fmt.Errorf("处理 whiteout 文件失败: %w", err)
	}

	return nil
}

// Commit 原地提交：将 Active 快照转为 Committed 快照
// 对齐 containerd: 原地提交，只更新元数据（Kind → Committed），删除 upper/work
// 用于 Pull 流程和 Build 流程
func (o *OverlaySnapshotter) Commit(ctx context.Context, key string) ([]snapshots.Mount, error) {
	var snapInfo *snapshots.Info
	if err := o.db.View(func(tx *bolt.Tx) error {
		var err error
		snapInfo, err = metadata.LoadSnapshot(tx, key)
		return err
	}); err != nil {
		return nil, fmt.Errorf("加载快照 %s 失败: %w", key, err)
	}

	if snapInfo.Kind != snapshots.KindActive {
		return nil, fmt.Errorf("快照 %s 不是 Active 快照，无法 Commit", key)
	}

	srcDir := filepath.Join(o.root, key)
	srcDiffDir := filepath.Join(srcDir, "diff")
	srcUpperDir := filepath.Join(srcDir, "upper")
	srcWorkDir := filepath.Join(srcDir, "work")

	// 合并 upper/ → diff/（如果有），然后删除 upper/ 和 work/
	if dirHasFiles(srcUpperDir) {
		if err := mergeDir(srcUpperDir, srcDiffDir, true); err != nil {
			return nil, fmt.Errorf("合并 upper 到 diff 失败: %w", err)
		}
	}
	os.RemoveAll(srcUpperDir)
	os.RemoveAll(srcWorkDir)

	// 更新元数据：Active → Committed
	committedInfo := &snapshots.Info{
		Name:      key,
		Parent:    snapInfo.Parent,
		Kind:      snapshots.KindCommitted,
		ReadWrite: false,
		CreatedAt: snapInfo.CreatedAt,
		UpdatedAt: time.Now().Format(constants.TimeFormat),
		Labels:    snapInfo.Labels,
	}
	if err := o.db.Update(func(tx *bolt.Tx) error {
		// 先删旧的 Active 元数据，再存 Committed 元数据
		metadata.DeleteSnapshot(tx, key)
		return metadata.SaveSnapshot(tx, committedInfo)
	}); err != nil {
		return nil, fmt.Errorf("更新快照元数据失败: %w", err)
	}

	return o.mounts(key)
}

// CommitAs 创建新快照：从 Active 快照创建新的 Committed 快照
// 用于容器 commit（docker commit 等场景）
// name: 新 Committed 快照的名称
// key: 源 Active 快照的名称（由调用方负责 Remove）
func (o *OverlaySnapshotter) CommitAs(ctx context.Context, name, key string) ([]snapshots.Mount, error) {
	var snapInfo *snapshots.Info
	if err := o.db.View(func(tx *bolt.Tx) error {
		var err error
		snapInfo, err = metadata.LoadSnapshot(tx, key)
		return err
	}); err != nil {
		return nil, fmt.Errorf("加载快照 %s 失败: %w", key, err)
	}

	if snapInfo.Kind != snapshots.KindActive {
		return nil, fmt.Errorf("快照 %s 不是 Active 快照，无法 Commit", key)
	}

	srcDir := filepath.Join(o.root, key)
	srcDiffDir := filepath.Join(srcDir, "diff")
	srcUpperDir := filepath.Join(srcDir, "upper")

	// 创建新的 Committed 快照目录
	newDir := filepath.Join(o.root, name)
	if err := os.MkdirAll(newDir, 0755); err != nil {
		return nil, fmt.Errorf("创建 Committed 快照目录失败: %w", err)
	}

	newDiffDir := filepath.Join(newDir, "diff")

	if dirHasFiles(srcUpperDir) {
		// 容器 commit 场景：upper/ 有运行时修改，rename upper/ → 新 diff/
		if err := os.Rename(srcUpperDir, newDiffDir); err != nil {
			os.RemoveAll(newDir)
			return nil, fmt.Errorf("rename upper 到新 diff 失败: %w", err)
		}
		// 合并源 diff/ 到新 diff/
		if err := mergeDir(srcDiffDir, newDiffDir, true); err != nil {
			os.RemoveAll(newDir)
			return nil, fmt.Errorf("合并 diff 到新快照失败: %w", err)
		}
	} else {
		// 镜像解包场景：数据在 diff/ 中，rename diff/ → 新 diff/
		if err := os.Rename(srcDiffDir, newDiffDir); err != nil {
			os.RemoveAll(newDir)
			return nil, fmt.Errorf("rename diff 到新快照失败: %w", err)
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
		return nil, fmt.Errorf("保存 Committed 快照元数据失败: %w", err)
	}

	return o.mounts(name)
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
func (o *OverlaySnapshotter) DiffPath(ctx context.Context, key string) (string, error) {
	return filepath.Join(o.root, key, "diff"), nil
}

// Close 关闭 Snapshotter（目前为空操作）
func (o *OverlaySnapshotter) Close() error {
	return nil
}

// mounts 构建快照的挂载信息
// 对齐 containerd: committed 快照返回 overlay mount（lowerdir 指向 diff/），
// 而非 bind mount。这样 committed 快照才能正确作为子 overlay 的 parent。
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
		// 对齐 containerd: committed 快照返回 overlay mount
		// lowerdir 指向自己的 diff/，这样它可以作为子 overlay 的 parent
		var options []string
		options = append(options, fmt.Sprintf("lowerdir=%s", diffDir))
		return []snapshots.Mount{
			{
				Type:    "overlay",
				Source:  "overlay",
				Options: options,
			},
		}, nil
	}

	// Active 快照：完整的 overlay mount
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

// mergeDir 递归合并目录内容
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
			if continueOnError {
				return nil
			}
			return fmt.Errorf("复制文件 %s 失败: %w", rel, err)
		}

		return nil
	})
}

// MergeUpperToDiff 将 upper 层的文件合并到 diff 目录
// 对齐 containerd: Commit 时将 Active 快照的 upper 层内容合并到新快照的 diff/
// 导出此函数供 image 包的 CreateLayerFromDir 等场景复用
func MergeUpperToDiff(upperDir, diffDir string) error {
	return mergeDir(upperDir, diffDir, true)
}

// processWhiteouts 处理 OCI whiteout 文件
// 对齐 containerd: diff.Applier 中的 whiteout 处理逻辑
//   - .wh.<name> → 删除父层中对应的文件/目录
//   - .wh..wh..opq → 不透明白化，删除父层中该目录下的所有内容
func processWhiteouts(diffDir string, parentDiffDirs []string) error {
	return filepath.Walk(diffDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}

		base := filepath.Base(path)
		dir := filepath.Dir(path)

		// 处理不透明白化 (.wh..wh..opq)
		// 表示该目录下的所有父层内容都应被隐藏
		if base == ".wh..wh..opq" {
			// 删除 whiteout 标记文件本身
			os.Remove(path)

			// 获取该目录在 diff 中的相对路径
			relDir, relErr := filepath.Rel(diffDir, dir)
			if relErr != nil {
				return nil
			}

			// 从所有父层中删除该目录下的内容
			for _, parentDir := range parentDiffDirs {
				parentPath := filepath.Join(parentDir, relDir)
				if parentPath == parentDir {
					// 根目录的 opaque whiteout：删除父层根目录下的所有内容
					cleanDirContents(parentPath)
				} else {
					cleanDirContents(parentPath)
				}
			}
			return nil
		}

		// 处理普通 whiteout (.wh.<name>)
		if strings.HasPrefix(base, ".wh.") && len(base) > 4 {
			targetName := base[4:] // 去掉 ".wh." 前缀
			os.Remove(path)        // 删除 whiteout 标记文件本身

			// 获取 whiteout 文件所在目录的相对路径
			relDir, relErr := filepath.Rel(diffDir, dir)
			if relErr != nil {
				return nil
			}

			// 从所有父层中删除对应的文件/目录
			for _, parentDir := range parentDiffDirs {
				var targetPath string
				if relDir == "." {
					targetPath = filepath.Join(parentDir, targetName)
				} else {
					targetPath = filepath.Join(parentDir, relDir, targetName)
				}
				os.RemoveAll(targetPath)
			}
		}

		return nil
	})
}

// cleanDirContents 删除目录下的所有内容，但保留目录本身
func cleanDirContents(dir string) {
	d, err := os.Open(dir)
	if err != nil {
		return
	}
	defer d.Close()

	names, err := d.Readdirnames(-1)
	if err != nil {
		return
	}

	for _, name := range names {
		os.RemoveAll(filepath.Join(dir, name))
	}
}

// DigestToCacheID 将 digest 转换为 cacheID（去掉 sha256: 前缀）
// 保留此函数供外部使用
var DigestToCacheID = content.DigestToCacheID
