package images

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"mini-docker/constants"
	"mini-docker/containerd/content"
	"mini-docker/containerd/metadata"
	"mini-docker/containerd/snapshots"
	"mini-docker/containerd/snapshots/overlay"
	"mini-docker/utils"
)

/*
=======================================================================
  镜像管理 —— 对齐 containerd 的 content-addressable 分层存储模型
=======================================================================

  本文件只负责镜像的 OCI 下载/解压。
  元数据（manifest / tag / layer 注册）由 images.Service 通过 boltdb 维护。

  镜像存储结构（对齐 containerd）：
  /var/lib/mini-docker/
  ├── metadata.db                         ← boltdb 数据库（镜像/层/标签/快照/租约/内容元数据）
  ├── content/sha256/                     ← Content Store（blob 存储：manifest/config/layer 原始数据）
  │   └── <hex>                           ← 以 digest 的 hex 部分命名
  └── snapshots/overlay/                  ← OverlayFS Snapshotter（解压后的层快照）
      └── <cache-id>/
          └── diff/                       ← 该层解压后的文件

  与之前版本的差异：
  - 废弃了 images/<name>/rootfs/ 预构建目录
  - 废弃了 overlay2/ + layerdb/ + blobs/ 冗余存储
  - 容器运行时通过 OverlayFS 动态合并各层 diff/ 目录，无需预构建 rootfs
  - 与真实 containerd 的差异：containerd 运行时通过 Snapshotter.Prepare() 返回 overlay mount，
    mini-docker 运行时通过 Snapshotter.Mounts() 或 LayerDiffDir() 收集各层 diff 路径构建 overlay lowerdir
  - 镜像拉取时通过 Snapshotter.UnpackLayer 统一完成"解压+元数据注册"，替代之前的
    LayerStore.StoreLayer + Snapshotter.RegisterCommitted 两步操作

=======================================================================
*/

// ---------------------------------------------------------------------------
// 核心数据结构（对齐 containerd 的 content-addressable 模型）
// ---------------------------------------------------------------------------

// ImageInfo 镜像元数据（对外展示）
// 对齐 containerd: containerd images list 输出的每行对应一个 ImageInfo
type ImageInfo struct {
	Name        string   `json:"name"`
	Tag         string   `json:"tag"`
	ImageID     string   `json:"image_id"`
	Size        string   `json:"size"`
	CreatedAt   string   `json:"created_at"`
	RootFS      string   `json:"rootfs"`
	SnapshotKey string   `json:"snapshot_key"` // 镜像最顶层的快照标识（cacheID），用于容器运行时 PrepareSnapshot 的 parent
	Layers      []string `json:"layers"`
}

// ImageManifest 镜像清单（对齐 OCI Image Manifest）
// 类型统一定义在 metadata 包，此处通过类型别名重导出，保持包级 API 兼容
type ImageManifest = metadata.ImageManifest

// ImageConfig 镜像配置（对齐 OCI Image Config）
// 类型统一定义在 metadata 包，此处通过类型别名重导出，保持包级 API 兼容
type ImageConfig = metadata.ImageConfig

// LayerInfo 层元数据（对齐 containerd: 层信息存储在 boltdb 中）
// 类型统一定义在 metadata 包，此处通过类型别名重导出，保持包级 API 兼容
type LayerInfo = metadata.LayerInfo

/*
=======================================================================
  pull —— 拉取/构建镜像（仅做 OCI 工作，不写元数据）
=======================================================================

  模式1: 从 Registry 拉取（检测到镜像名含 / 或 . 时触发）
  ─────────────────────────────────────────────────
  1. 解析镜像引用为 registry/repository:tag
  2. 连接 Registry，获取 Bearer Token
  3. GET /v2/<name>/manifests/<tag> 获取 OCI Manifest
  4. 解析 Manifest 获取层列表
  5. 逐层下载 blob（GET /v2/<name>/blobs/<digest>）到 content/sha256/
  6. 每层 SHA256 校验
  7. 解压 tar.gz 到 snapshots/overlay/<cache-id>/diff/
  8. 处理 whiteout 文件
  注意：不再预构建 rootfs，容器运行时通过 OverlayFS 动态合并各层

  模式2: 本地构建（简单镜像名，不含 / 或 .）
  ─────────────────────────────────────────────────
  1. 创建 rootfs 目录结构
  2. 安装 busybox 等基础工具
  3. 计算层 digest
  4. 创建 rootfs

  镜像引用判断逻辑（对齐 Docker）：
  - "alpine"              → 本地构建
  - "alpine:3.18"         → 本地构建（简单名 + tag）
  - "library/alpine"      → Registry 拉取（含 /）
  - "docker.io/alpine"    → Registry 拉取（含 .）
  - "myreg.com/myapp:v1"  → Registry 拉取（含 . 和 /）

  注意：本函数不写任何元数据。调用方（images.Service）负责把结果同步到 boltdb。
=======================================================================
*/

// ProgressFunc 进度回调函数类型
type ProgressFunc func(status, message string)

// pull 拉取/构建镜像，返回 ImageInfo。失败时返回 error。
// 注意：本函数不进行元数据持久化，调用方需自行注册到 boltdb。
// contentStore: blob 存储接口，传递给 RegistryClient 以确保 blob 写入通过 contentStore 完成
// snap: Snapshotter 接口，通过 UnpackLayer 原子完成"解压+元数据注册"，避免分步操作导致不一致
func pull(imageRef string, progress ProgressFunc, contentStore content.Store, snap snapshots.Snapshotter) (*ImageInfo, error) {
	if isRegistryRef(imageRef) {
		return pullFromRegistry(imageRef, progress, contentStore, snap)
	}

	// 对齐 Docker: 先检查本地是否已有该镜像（通过 boltdb 元数据查询）
	// 之前的实现通过 os.Stat 检查 snapshots/overlay/<name>/diff 目录，
	// 但 Registry 拉取的镜像存储路径是 snapshots/overlay/<cacheID>/diff，
	// 导致已拉取的镜像无法被识别，每次都重新拉取
	localRootFS := filepath.Join(constants.SnapshotterDir, imageRef, "diff")
	if _, err := os.Stat(localRootFS); os.IsNotExist(err) {
		registryRef := "library/" + imageRef
		return pullFromRegistry(registryRef, progress, contentStore, snap)
	}

	return pullLocal(imageRef, progress, snap)
}

// isRegistryRef 判断镜像引用是否指向 Registry
// 对齐 Docker: 含有 / 或 . 的镜像名视为 Registry 引用
func isRegistryRef(imageRef string) bool {
	name := imageRef
	if idx := strings.LastIndex(imageRef, ":"); idx > 0 {
		name = imageRef[:idx]
	}
	if strings.Contains(name, "/") || strings.Contains(name, ".") {
		return true
	}
	return false
}

// pullFromRegistry 从 Docker Registry 拉取镜像
// 对齐 containerd: 当配置了 registry-mirrors 时，优先从 mirror 拉取，失败后回退到原始地址
func pullFromRegistry(imageRef string, progress ProgressFunc, contentStore content.Store, snap snapshots.Snapshotter) (*ImageInfo, error) {
	registry, repository, tag := ResolveImageRef(imageRef)

	// 对齐 Docker: 如果目标是 Docker Hub 且配置了 mirror，先尝试 mirror
	if registry == constants.DefaultRegistryHost {
		mirrors := GetRegistryMirrors()
		for _, mirror := range mirrors {
			mirror = strings.TrimSuffix(mirror, "/")
			progress("downloading", fmt.Sprintf("尝试从 mirror 拉取: %s/%s:%s", mirror, repository, tag))
			info, err := doPullFromRegistry(mirror, repository, tag, progress, contentStore, snap)
			if err == nil {
				return info, nil
			}
			progress("error", fmt.Sprintf("mirror %s 拉取失败: %v", mirror, err))
		}
		if len(mirrors) > 0 {
			progress("downloading", "所有 mirror 均失败，回退到原始 Registry...")
		}
	}

	return doPullFromRegistry(registry, repository, tag, progress, contentStore, snap)
}

// doPullFromRegistry 实际执行从指定 registry 拉取镜像的逻辑
func doPullFromRegistry(registry, repository, tag string, progress ProgressFunc, contentStore content.Store, snap snapshots.Snapshotter) (*ImageInfo, error) {
	progress("downloading", fmt.Sprintf("从 Registry 拉取镜像: %s/%s:%s", registry, repository, tag))

	client := NewRegistryClient(registry, contentStore)

	progress("downloading", "获取镜像 manifest...")
	// 对齐 containerd: 使用 DownloadManifest 而非 GetManifest，确保 manifest blob 落盘到 Content Store
	// 这样 content-addressable 存储完整（manifest/config/layer 全部持久化），支持 image push 和离线 inspect
	manifest, _, err := client.DownloadManifest(repository, tag)
	if err != nil {
		return nil, fmt.Errorf("获取 manifest 失败: %w", err)
	}
	progress("downloading", fmt.Sprintf("Manifest 获取成功, %d 层", len(manifest.Layers)))

	progress("downloading", "获取镜像配置...")
	ociConfig, err := client.GetImageConfig(repository, &manifest.Config)
	if err != nil {
		progress("warning", fmt.Sprintf("获取镜像配置失败: %v", err))
		ociConfig = &OCIImageConfig{}
	}

	var layerDigests []string
	// 遍历镜像中的每一层
	// 对齐 containerd: 通过 Snapshotter.UnpackLayer 统一完成"解压+元数据注册"，
	// 保证文件与 BoltDB 元数据的一致性，崩溃恢复时不会留下孤儿目录
	var parentSnapKey string
	for i, layerDesc := range manifest.Layers {
		digestShort := layerDesc.Digest
		if len(digestShort) > 19 {
			digestShort = digestShort[:19]
		}
		progress("downloading", fmt.Sprintf("下载层 %d/%d (%s, %d bytes)...", i+1, len(manifest.Layers), digestShort, layerDesc.Size))

		// blob 下载进度回调：将下载字节数转换为可读进度信息
		blobProgress := func(total, completed int64) {
			if total > 0 {
				pct := completed * 100 / total
				progress("downloading", fmt.Sprintf("下载层 %d/%d: %d/%d bytes (%d%%)", i+1, len(manifest.Layers), completed, total, pct))
			} else {
				progress("downloading", fmt.Sprintf("下载层 %d/%d: %d bytes", i+1, len(manifest.Layers), completed))
			}
		}
		// 下载这个镜像的这一层 layer 的数据，使用 Digest 唯一标识（镜像中的唯一标识）去下载，返回存储路径
		blobPath, err := client.DownloadBlob(repository, layerDesc.Digest, blobProgress)
		if err != nil {
			// 下载失败时清理已下载的层快照和 blob，避免孤儿数据残留
			cleanupPullLayers(context.Background(), snap, contentStore, layerDigests)
			return nil, fmt.Errorf("下载层 %d 失败: %w", i+1, err)
		}

		diffID := ""
		if ociConfig != nil && i < len(ociConfig.RootFS.DiffIDs) {
			diffID = ociConfig.RootFS.DiffIDs[i] //解压后的每一层的 SHA-256 哈希值
		}

		progress("extracting", fmt.Sprintf("解压层 %d/%d...", i+1, len(manifest.Layers)))
		// 对齐 containerd: 通过 Snapshotter.UnpackLayer 统一完成"解压+元数据注册"
		// 替代之前的 LayerStore.StoreLayer + Snapshotter.RegisterCommitted 两步操作，
		// 避免分两步执行时崩溃导致"文件存在但元数据缺失"的不一致状态
		cacheID, err := snap.UnpackLayer(context.Background(), blobPath, layerDesc.Digest, diffID, parentSnapKey)
		if err != nil {
			// 解压失败时清理已下载的层快照和 blob，避免孤儿数据残留
			cleanupPullLayers(context.Background(), snap, contentStore, layerDigests)
			return nil, fmt.Errorf("解压并注册层 %d 失败: %w", i+1, err)
		}
		parentSnapKey = cacheID

		_ = cacheID // cacheID 已通过 content.DigestToCacheID 与 layerDesc.Digest 关联
		layerDigests = append(layerDigests, layerDesc.Digest)
		progress("downloading", fmt.Sprintf("层 %d/%d 完成", i+1, len(manifest.Layers)))
	}

	repoName := repository
	if strings.HasPrefix(repoName, "library/") {
		repoName = strings.TrimPrefix(repoName, "library/")
	}

	// 不再预构建 rootfs，容器运行时通过 OverlayFS 动态合并各层
	// 通过 LayerDiffDir() 可以获取每层的 diff/ 路径用于构建 overlay lowerdir

	// 对齐 containerd: 通过 Snapshotter.DiffPath() 计算层大小，支持可插拔 Snapshotter
	size := calculateLayersSize(snap, layerDigests)

	imageID := manifest.Config.Digest
	if strings.HasPrefix(imageID, "sha256:") {
		imageID = imageID[7:]
	}
	if imageID == "" {
		imageID = computeImageID(repository, tag, "")
	}

	snapshotKey := content.DigestToCacheID(layerDigests[len(layerDigests)-1]) // 最顶层的 snapshotKey，用于容器运行时 PrepareSnapshot 的 parent
	info := &ImageInfo{
		Name:        repoName,
		Tag:         tag,
		ImageID:     imageID,
		Size:        size,
		CreatedAt:   time.Now().Format("2006-01-02 15:04:05"),
		RootFS:      snapshotKey,
		SnapshotKey: snapshotKey,
		Layers:      layerDigests,
	}

	progress("complete", fmt.Sprintf("镜像 %s:%s (%s) 拉取成功", repoName, tag, size))

	return info, nil
}

// cleanupPullLayers 清理镜像拉取过程中已下载但失败的层
// 当 pullFromRegistry 中途失败时调用，避免留下孤儿快照和 blob
func cleanupPullLayers(ctx context.Context, snap snapshots.Snapshotter, contentStore content.Store, layerDigests []string) {
	for _, digest := range layerDigests {
		cacheID := content.DigestToCacheID(digest)
		if snap != nil {
			snap.Remove(ctx, cacheID)
		}
		if contentStore != nil {
			contentStore.Delete(ctx, digest)
		}
	}
}

// pullLocal 本地构建镜像（创建 busybox rootfs）
// 对齐 containerd: 构建完成后通过 Snapshotter.RegisterCommitted 注册快照元数据，
// 确保 GC 和 lowerDirs() 能正确感知本地构建的镜像层
func pullLocal(imageRef string, progress ProgressFunc, snap snapshots.Snapshotter) (*ImageInfo, error) {
	name, tag := utils.ParseImageTag(imageRef)

	if tag == "" {
		tag = "latest"
	}

	// 本地构建的镜像存储在 snapshotter 目录下
	snapDir := filepath.Join(constants.SnapshotterDir, name)
	rootFSPath := filepath.Join(snapDir, "diff")

	progress("building", fmt.Sprintf("构建镜像 %s:%s...", name, tag))

	progress("building", "创建 rootfs 目录结构...")
	if err := createRootFSDirs(rootFSPath); err != nil {
		os.RemoveAll(snapDir)
		return nil, fmt.Errorf("创建目录失败: %w", err)
	}

	progress("building", "创建配置文件...")
	if err := createEtcFiles(rootFSPath); err != nil {
		os.RemoveAll(snapDir)
		return nil, fmt.Errorf("创建配置文件失败: %w", err)
	}

	if err := createDevNodes(rootFSPath); err != nil {
		progress("warning", fmt.Sprintf("创建设备节点失败: %v", err))
	}

	progress("building", "安装 busybox（提供基础命令）...")
	if err := setupBusybox(rootFSPath); err != nil {
		progress("warning", fmt.Sprintf("busybox 安装失败: %v", err))
		progress("warning", fmt.Sprintf("镜像已创建但缺少基础命令，请手动安装 busybox 到 %s/bin/", rootFSPath))
	}

	size := CalculateRootFSSize(rootFSPath)
	imageID := computeImageID(name, tag, rootFSPath)
	layerDigest := computeLayerDigest(rootFSPath)

	// 对齐 containerd: 本地构建的镜像也需要注册快照元数据到 boltdb
	// 这样 GC 能跟踪该快照，容器运行时 lowerDirs() 能沿 parent 链递归构建 lowerdir
	// 本地构建的镜像使用 name 作为 snapshot key（与 Registry 拉取使用 cacheID 不同）
	if snap != nil {
		if err := snap.RegisterCommitted(context.Background(), name, ""); err != nil {
			progress("warning", fmt.Sprintf("注册本地镜像快照失败: %v", err))
		}
	}

	info := &ImageInfo{
		Name:        name,
		Tag:         tag,
		ImageID:     imageID,
		Size:        size,
		CreatedAt:   time.Now().Format("2006-01-02 15:04:05"),
		RootFS:      name, // 本地构建镜像的 RootFS 使用 name 作为 snapshotKey
		SnapshotKey: name, // 本地构建镜像的 snapshotKey 与 name 一致
		Layers:      []string{layerDigest},
	}

	progress("complete", fmt.Sprintf("镜像 %s:%s (%s) 构建成功", name, tag, size))

	return info, nil
}

// ---------------------------------------------------------------------------
// 标签/引用解析工具
// ---------------------------------------------------------------------------

// parseImageTag 已废弃，直接使用 utils.ParseImageTag 即可
// 保留此注释以说明镜像引用的解析规则

// ---------------------------------------------------------------------------
// 运行时 OverlayFS 层路径构建
// ---------------------------------------------------------------------------

// LayerDiffDir 已迁移到 oci_linux.go（统一使用 content.DigestToCacheID 转换 digest）
// 新代码应优先使用 Snapshotter.DiffPath()，仅在无法获取 Snapshotter 实例时使用 LayerDiffDir

// ---------------------------------------------------------------------------
// 工具函数
// ---------------------------------------------------------------------------

// computeImageID 计算镜像 ID（对齐 Docker: SHA256(配置JSON)）
// 确定性计算：相同输入必定产生相同 ID，满足 content-addressable 原则
func computeImageID(name, tag, rootFSPath string) string {
	h := sha256.New()
	h.Write([]byte(name + ":" + tag))
	h.Write([]byte(rootFSPath))
	return fmt.Sprintf("%x", h.Sum(nil))
}

// computeLayerDigest 计算目录的层摘要（对齐 Docker: SHA256(层内容)）
// 遍历整个目录树，对每个文件（跳过目录）将 相对路径 + 大小 喂给 SHA-256。
// 与 Docker 的对齐：Docker 对整个 tar 包做 SHA-256；本项目只哈希路径+大小（已知简化）。
func computeLayerDigest(dir string) string {
	h := sha256.New()
	filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(dir, path)
		h.Write([]byte(rel))
		h.Write([]byte(fmt.Sprintf("%d", info.Size())))
		return nil
	})
	return fmt.Sprintf("%x", h.Sum(nil))[:64]
}

// calculateRootFSSizeBytes 计算目录总大小（字节数）
// 对齐 Docker: 硬链接只计算一次，避免 busybox 等镜像因大量硬链接导致大小虚高
func calculateRootFSSizeBytes(rootFSPath string) int64 {
	var size int64
	seenInodes := make(map[[2]uint64]struct{}) // dev+inode 去重
	filepath.Walk(rootFSPath, func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			return nil
		}
		// 硬链接去重：同一 dev+inode 只计算一次
		if stat, ok := info.Sys().(*syscallStat); ok {
			inodeKey := [2]uint64{stat.Dev, stat.Ino}
			if _, seen := seenInodes[inodeKey]; seen {
				return nil
			}
			seenInodes[inodeKey] = struct{}{}
		}
		size += info.Size()
		return nil
	})
	return size
}

// CalculateRootFSSize 计算目录大小（人类可读格式）
func CalculateRootFSSize(rootFSPath string) string {
	size := calculateRootFSSizeBytes(rootFSPath)
	return formatSize(size)
}

// formatSize 将字节数格式化为人类可读字符串
func formatSize(sizeBytes int64) string {
	const (
		KB = 1024
		MB = 1024 * KB
		GB = 1024 * MB
	)

	switch {
	case sizeBytes >= GB:
		return fmt.Sprintf("%.1fg", float64(sizeBytes)/float64(GB))
	case sizeBytes >= MB:
		return fmt.Sprintf("%.1fm", float64(sizeBytes)/float64(MB))
	case sizeBytes >= KB:
		return fmt.Sprintf("%.1fk", float64(sizeBytes)/float64(KB))
	default:
		return fmt.Sprintf("%db", sizeBytes)
	}
}

// calculateLayersSize 通过 Snapshotter.DiffPath() 计算多个层的总大小（人类可读格式）
// 对齐 containerd: 优先使用 Snapshotter.DiffPath() 获取层路径，支持可插拔 Snapshotter
// Service.CalculateLayersSize 和 pullFromRegistry 共享此函数，消除重复的大小计算逻辑
func calculateLayersSize(snap snapshots.Snapshotter, layerDigests []string) string {
	var totalSize int64
	for _, digest := range layerDigests {
		var diffDir string
		cacheID := content.DigestToCacheID(digest)
		if snap != nil {
			if path, err := snap.DiffPath(context.Background(), cacheID); err == nil {
				diffDir = path
			}
		}
		if diffDir == "" {
			diffDir = LayerDiffDir(digest)
		}
		totalSize += calculateRootFSSizeBytes(diffDir)
	}
	return formatSize(totalSize)
}

// CreateLayerFromDir 从 upper 目录创建一个镜像层
// 对齐 containerd: 构建器中每条 RUN/COPY 指令生成新层。
// upperDir: OverlayFS upper 层的路径（包含该层的文件差异）
// parentDigest: 父层的 digest（用于建立 parent 链），为空表示基础层
// snap: Snapshotter 接口，通过 DiffPath 获取 diff 目录路径，并通过 RegisterCommitted 注册快照元数据
// 返回该层的 digest（同时也是 cacheID），由调用方负责把 digest 注册到 metadata.DB。
func CreateLayerFromDir(upperDir string, parentDigest string, snap snapshots.Snapshotter) (string, error) {
	digest := computeLayerDigest(upperDir)

	var diffDir string
	if snap != nil {
		ctx := context.Background()
		var err error
		diffDir, err = snap.DiffPath(ctx, digest)
		if err != nil {
			return "", fmt.Errorf("获取 diff 目录路径失败: %w", err)
		}
	} else {
		// 回退：直接拼接路径（兼容无法获取 Snapshotter 的场景）
		diffDir = filepath.Join(constants.SnapshotterDir, digest, "diff")
	}
	if err := os.MkdirAll(diffDir, 0755); err != nil {
		return "", fmt.Errorf("创建层 diff 目录失败: %w", err)
	}

	// 对齐 containerd: 使用 overlay 包统一的 MergeUpperToDiff 实现，避免重复代码
	if err := overlay.MergeUpperToDiff(upperDir, diffDir); err != nil {
		return "", fmt.Errorf("合并 upper 到 diff 失败: %w", err)
	}

	// 对齐 containerd: 通过 Snapshotter.RegisterCommitted 注册快照元数据到 boltdb
	// 建立 parent 链，确保 GC 和 lowerDirs() 能正确感知该层
	if snap != nil {
		parentKey := ""
		if parentDigest != "" {
			parentKey = content.DigestToCacheID(parentDigest)
		}
		if err := snap.RegisterCommitted(context.Background(), digest, parentKey); err != nil {
			// 注册失败不阻断构建流程，但记录警告（可能已注册过）
			fmt.Printf("  警告: 注册层快照元数据失败: %v\n", err)
		}
	}

	return digest, nil
}

// ---------------------------------------------------------------------------
// rootfs 构建辅助函数（声明，Linux 实现在 busybox_linux.go / dev_linux.go）
// ---------------------------------------------------------------------------

func createRootFSDirs(rootFSPath string) error {
	requiredDirs := []string{
		"bin", "sbin",
		"usr", "usr/bin", "usr/sbin", "usr/lib", "usr/local", "usr/local/bin",
		"lib", "lib64",
		"etc", "etc/init.d",
		"proc", "sys", "dev", "dev/pts", "dev/shm",
		"tmp", "root", "run",
		"var", "var/log", "var/run", "var/tmp", "var/cache", "var/lib",
		"opt", "home",
	}

	for _, dir := range requiredDirs {
		if err := os.MkdirAll(filepath.Join(rootFSPath, dir), 0755); err != nil {
			return fmt.Errorf("创建目录 %s 失败: %w", dir, err)
		}
	}

	os.Chmod(filepath.Join(rootFSPath, "root"), 0700)
	os.Chmod(filepath.Join(rootFSPath, "tmp"), 01777)

	return nil
}

func createEtcFiles(rootFSPath string) error {
	etcFiles := map[string]string{
		"hostname":      "mini-docker",
		"resolv.conf":   "nameserver 8.8.8.8\nnameserver 8.8.4.4\n",
		"hosts":         "127.0.0.1\tlocalhost\n::1\t\tlocalhost\n",
		"passwd":        "root:x:0:0:root:/root:/bin/sh\nnobody:x:65534:65534:nobody:/nonexistent:/bin/false\n",
		"group":         "root:x:0:\nnogroup:x:65534:\n",
		"shadow":        "root::0:0:99999:7:::\nnobody:*:0:0:99999:7:::\n",
		"os-release":    "NAME=\"MiniDocker\"\nVERSION=\"1.0\"\nID=minidocker\nPRETTY_NAME=\"MiniDocker Container\"\n",
		"nsswitch.conf": "passwd:         files\ngroup:          files\nshadow:         files\nhosts:          files dns\nnetworks:       files\nprotocols:      files\nservices:       files\n",
		"profile":       "export PATH=/bin:/sbin:/usr/bin:/usr/sbin:/usr/local/bin\nexport HOME=/root\nexport PS1='# '\nexport TERM=linux\n",
		"fstab":         "proc            /proc   proc    defaults        0 0\ntmpfs           /tmp    tmpfs   defaults,nosuid,nodev 0 0\ndevpts          /dev/pts devpts  defaults        0 0\n",
		"issue":         "Welcome to MiniDocker Container\n",
		"shells":        "/bin/sh\n/bin/ash\n/bin/bash\n",
		"protocols":     "ip\t0\tIP\nicmp\t1\tICMP\ntcp\t6\tTCP\nudp\t17\tUDP\n",
		"services":      "ssh\t22/tcp\nhttp\t80/tcp\nhttps\t443/tcp\n",
	}

	for filename, content := range etcFiles {
		filePath := filepath.Join(rootFSPath, "etc", filename)
		if err := os.WriteFile(filePath, []byte(content), 0644); err != nil {
			return fmt.Errorf("创建 %s 失败: %w", filename, err)
		}
	}

	os.Chmod(filepath.Join(rootFSPath, "etc", "shadow"), 0640)

	return nil
}

func createDevNodes(rootFSPath string) error {
	devDir := filepath.Join(rootFSPath, "dev")

	nullPath := filepath.Join(devDir, "null")
	if err := createDevNull(nullPath); err != nil {
		return err
	}

	return nil
}
