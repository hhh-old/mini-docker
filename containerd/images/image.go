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
	"mini-docker/containerd/diff"
	"mini-docker/containerd/metadata"
	"mini-docker/containerd/snapshots"
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
// 本包不再定义对外的镜像类型。统一使用 metadata.Image（元数据 + Size 衍生字段）。
// Size 由调用方在响应时填充，boltdb 落盘时被 omitempty 跳过。

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
// status 取值见 progress.go 中定义的 StatusXxx 常量
type ProgressFunc func(status ProgressFrameStatus, message string)

// pull 拉取/构建镜像，返回 *metadata.Image。失败时返回 error。
// contentSvc: Content Service，传递给 RegistryClient 以确保 blob 写入通过 Service 层完成
// snapSvc: Snapshotter Service，通过 Prepare-Apply-Commit 完成"解压+元数据注册"
// diffSvc: Diff Service，由插件系统注入，对齐 containerd: diff 服务可插拔
func pull(imageRef string, progress ProgressFunc, contentSvc *content.Service, snapSvc *snapshots.Service, diffSvc *diff.Service) (*metadata.Image, error) {
	return pullFromRegistry(imageRef, progress, contentSvc, snapSvc, diffSvc)
}

// pullFromRegistry 从 Docker Registry 拉取镜像
// 对齐 containerd: 当配置了 registry-mirrors 时，优先从 mirror 拉取，失败后回退到原始地址
func pullFromRegistry(imageRef string, progress ProgressFunc, contentSvc *content.Service, snapSvc *snapshots.Service, diffSvc *diff.Service) (*metadata.Image, error) {
	registry, repository, tag := ResolveImageRef(imageRef)

	// 对齐 Docker: 如果目标是 Docker Hub 且配置了 mirror，先尝试 mirror
	if registry == constants.DefaultRegistryHost {
		mirrors := GetRegistryMirrors()
		for _, mirror := range mirrors {
			mirror = strings.TrimSuffix(mirror, "/")
			progress(StatusDownloading, fmt.Sprintf("尝试从 mirror 拉取: %s/%s:%s", mirror, repository, tag))
			info, err := doPullFromRegistry(mirror, repository, tag, progress, contentSvc, snapSvc, diffSvc)
			if err == nil {
				return info, nil
			}
			progress(StatusError, fmt.Sprintf("mirror %s 拉取失败: %v", mirror, err))
		}
		if len(mirrors) > 0 {
			progress(StatusDownloading, "所有 mirror 均失败，回退到原始 Registry...")
		}
	}

	return doPullFromRegistry(registry, repository, tag, progress, contentSvc, snapSvc, diffSvc)
}

// doPullFromRegistry 实际执行从指定 registry 拉取镜像的逻辑
func doPullFromRegistry(registry, repository, tag string, progress ProgressFunc, contentSvc *content.Service, snapSvc *snapshots.Service, diffSvc *diff.Service) (*metadata.Image, error) {
	progress(StatusDownloading, fmt.Sprintf("从 Registry 拉取镜像: %s/%s:%s", registry, repository, tag))

	client := NewRegistryClient(registry, contentSvc)

	progress(StatusDownloading, "获取镜像 manifest...")
	// 对齐 containerd: 使用 DownloadManifest 确保 manifest blob 落盘到 Content Store
	manifest, _, err := client.DownloadManifest(repository, tag)
	if err != nil {
		return nil, fmt.Errorf("获取 manifest 失败: %w", err)
	}
	progress(StatusDownloading, fmt.Sprintf("Manifest 获取成功, %d 层", len(manifest.Layers)))

	progress(StatusDownloading, "获取镜像配置...")
	ociConfig, err := client.GetImageConfig(repository, &manifest.Config)
	if err != nil {
		// manifest blob 仍在 contentSvc 中（content-addressable，可由 GC 清理）
		return nil, fmt.Errorf("获取镜像配置失败（OCI Config 不可降级，否则会关闭 DiffID 校验）: %w", err)
	}

	var layerDigests []string
	// 对齐 containerd: 逐层执行 Prepare-Apply-Commit 循环
	// Prepare: 创建 Active 快照（可写）
	// Apply:   通过 Diff Service 解压 tar.gz + 处理 whiteout
	// Commit:  将 Active 快照提交为 Committed 快照（纯元数据操作，目录不变）
	var parentSnapKey string
	for i, layerDesc := range manifest.Layers {
		digestShort := layerDesc.Digest
		if len(digestShort) > 19 {
			digestShort = digestShort[:19]
		}
		progress(StatusDownloading, fmt.Sprintf("下载层 %d/%d (%s, %d bytes)...", i+1, len(manifest.Layers), digestShort, layerDesc.Size))

		// blob 下载进度回调：将下载字节数转换为可读进度信息
		blobProgress := func(total, completed int64) {
			if total > 0 {
				pct := completed * 100 / total
				progress(StatusDownloading, fmt.Sprintf("下载层 %d/%d: %d/%d bytes (%d%%)", i+1, len(manifest.Layers), completed, total, pct))
			} else {
				progress(StatusDownloading, fmt.Sprintf("下载层 %d/%d: %d bytes", i+1, len(manifest.Layers), completed))
			}
		}
		// 下载这个镜像的这一层 layer 的数据
		if _, err := client.DownloadBlob(repository, layerDesc.Digest, layerDesc.Size, blobProgress); err != nil {
			cleanupPullLayers(context.Background(), snapSvc, contentSvc, layerDigests)
			return nil, fmt.Errorf("下载层 %d 失败: %w", i+1, err)
		}

		diffID := ""
		if ociConfig != nil && i < len(ociConfig.RootFS.DiffIDs) {
			diffID = ociConfig.RootFS.DiffIDs[i]
		}

		cacheID := content.DigestToCacheID(layerDesc.Digest)

		// Step 1: Prepare — 创建 Active 快照（key = cacheID）
		progress(StatusExtracting, fmt.Sprintf("准备层 %d/%d...", i+1, len(manifest.Layers)))
		if _, err := snapSvc.Prepare(context.Background(), cacheID, parentSnapKey); err != nil {
			cleanupPullLayers(context.Background(), snapSvc, contentSvc, layerDigests)
			return nil, fmt.Errorf("Prepare 层 %d 失败: %w", i+1, err)
		}

		// Step 2: Apply — 通过 Diff Service 解压 tar.gz + 处理 whiteout
		progress(StatusExtracting, fmt.Sprintf("解压层 %d/%d...", i+1, len(manifest.Layers)))
		if err := diffSvc.Apply(context.Background(), layerDesc.Digest, diffID, cacheID); err != nil {
			snapSvc.Remove(context.Background(), cacheID)
			cleanupPullLayers(context.Background(), snapSvc, contentSvc, layerDigests)
			return nil, fmt.Errorf("Apply 层 %d 失败: %w", i+1, err)
		}

		// Step 3: Commit — 原地提交为 Committed 快照（纯元数据操作，目录不变）
		// name=key=cacheID，提交后目录不变，只是 key 映射改变
		if err := snapSvc.Commit(context.Background(), cacheID, cacheID); err != nil {
			snapSvc.Remove(context.Background(), cacheID)
			cleanupPullLayers(context.Background(), snapSvc, contentSvc, layerDigests)
			return nil, fmt.Errorf("Commit 层 %d 失败: %w", i+1, err)
		}

		parentSnapKey = cacheID
		layerDigests = append(layerDigests, layerDesc.Digest)
		progress(StatusDownloading, fmt.Sprintf("层 %d/%d 完成", i+1, len(manifest.Layers)))
	}

	repoName := repository
	if strings.HasPrefix(repoName, "library/") {
		repoName = strings.TrimPrefix(repoName, "library/")
	}

	// 不再预构建 rootfs，容器运行时通过 OverlayFS 动态合并各层
	// 通过 diff.FSDir() 可以获取每层的 fs/ 路径用于构建 overlay lowerdir

	// 对齐 containerd: 通过 diff.FSDir() 计算层大小，支持可插拔 Snapshotter
	size := calculateLayersSize(snapSvc, layerDigests)

	imageID := manifest.Config.Digest
	if strings.HasPrefix(imageID, "sha256:") {
		imageID = imageID[7:]
	}
	if imageID == "" {
		imageID = computeImageID(repository, tag, "")
	}

	snapshotID := content.DigestToCacheID(layerDigests[len(layerDigests)-1]) // 最顶层的 snapshotID，用于容器运行时 PrepareSnapshot 的 parent
	info := &metadata.Image{
		Name:               repoName,
		Tag:                tag,
		ImageID:            imageID,
		CreatedAt:          time.Now().Format("2006-01-02 15:04:05"),
		TopLayerSnapshotID: snapshotID,
		LayerDigests:       layerDigests,
		Annotations:        manifest.Annotations,              // 透传 OCI Manifest Annotations
		Config:             ociConfigToImageConfig(ociConfig), // 持久化 OCI 运行时配置
		ConfigDigest:       manifest.Config.Digest,            // 对齐 containerd: GC 标记 config blob
		Size:               size,
	}

	progress(StatusComplete, fmt.Sprintf("镜像 %s:%s (%s) 拉取成功", repoName, tag, size))

	return info, nil
}

// cleanupPullLayers 清理镜像拉取过程中已下载但失败的层
// 当 pullFromRegistry 中途失败时调用，避免留下孤儿快照和 blob
func cleanupPullLayers(ctx context.Context, snapSvc *snapshots.Service, contentSvc *content.Service, layerDigests []string) {
	for _, digest := range layerDigests {
		cacheID := content.DigestToCacheID(digest)
		snapSvc.Remove(ctx, cacheID)
		contentSvc.Delete(ctx, digest)
	}
}

// pullLocal 本地构建镜像（创建 busybox rootfs）
// 对齐 containerd: 构建完成后通过 Prepare-Commit 流程注册快照元数据，
// 确保 GC 和 lowerDirs() 能正确感知本地构建的镜像层
// contentStore: blob 存储接口，用于通过 diff.Differ 计算真正的层 tar blob 和 digest
// differ: 层差异计算器，由插件系统注入，对齐 containerd: diff 服务可插拔
func pullLocal(imageRef string, progress ProgressFunc, contentStore content.Store, snap snapshots.Snapshotter, differ diff.Differ) (*metadata.Image, error) {
	name, tag := utils.ParseImageTag(imageRef)

	if tag == "" {
		tag = "latest"
	}

	progress(StatusBuilding, fmt.Sprintf("构建镜像 %s:%s...", name, tag))

	// Step 1: Prepare — 创建 Active 快照（key = name，无父快照）
	progress(StatusBuilding, "创建 rootfs 目录结构...")
	if _, err := snap.Prepare(context.Background(), name, ""); err != nil {
		// Prepare 失败可能是因为目录已存在（缓存命中）
		progress(StatusWarning, fmt.Sprintf("Prepare 本地镜像快照失败: %v", err))
		// Prepare 失败时仍尝试继续，可能快照已存在
	}

	// Step 2: 获取快照的 Mount 信息，从中提取 fs/ 目录路径
	// 对齐 containerd: 通过 Mounts() 获取挂载信息，而非手动拼接路径
	mounts, err := snap.Mounts(context.Background(), name)
	if err != nil {
		snap.Remove(context.Background(), name)
		return nil, fmt.Errorf("获取本地镜像挂载信息失败: %w", err)
	}
	rootFSPath := diff.UpperDir(mounts)
	if rootFSPath == "" {
		snap.Remove(context.Background(), name)
		return nil, fmt.Errorf("无法从挂载信息中提取 fs 目录路径")
	}

	progress(StatusBuilding, "创建 rootfs 目录结构...")
	if err := createRootFSDirs(rootFSPath); err != nil {
		snap.Remove(context.Background(), name)
		return nil, fmt.Errorf("创建目录失败: %w", err)
	}

	progress(StatusBuilding, "创建配置文件...")
	if err := createEtcFiles(rootFSPath); err != nil {
		snap.Remove(context.Background(), name)
		return nil, fmt.Errorf("创建配置文件失败: %w", err)
	}

	if err := createDevNodes(rootFSPath); err != nil {
		progress(StatusWarning, fmt.Sprintf("创建设备节点失败: %v", err))
	}

	progress(StatusBuilding, "安装 busybox（提供基础命令）...")
	if err := setupBusybox(rootFSPath); err != nil {
		progress(StatusWarning, fmt.Sprintf("busybox 安装失败: %v", err))
		progress(StatusWarning, fmt.Sprintf("镜像已创建但缺少基础命令，请手动安装 busybox 到 %s/bin/", rootFSPath))
	}

	size := CalculateRootFSSize(rootFSPath)
	imageID := computeImageID(name, tag, rootFSPath)

	// 对齐 containerd: 使用注入的 diff.Differ 计算真正的层 tar blob 和 digest
	// 替代旧的 computeLayerDigest（仅哈希路径+大小，不是真正的 tar digest）
	// 对齐 containerd: 传入 Mount 对象而非原始目录路径
	progress(StatusBuilding, "计算层差异...")
	var layerDigest string
	// 本地构建镜像无父层，lower 为 nil
	diffResult, err := differ.Diff(context.Background(), nil, mounts, contentStore)
	if err != nil {
		// Differ 失败时回退到 computeLayerDigest（兼容性降级）
		progress(StatusWarning, fmt.Sprintf("diff.Differ 计算失败，回退到简化摘要: %v", err))
		layerDigest = computeLayerDigest(rootFSPath)
	} else {
		layerDigest = diffResult.Digest
		// diffResult.DiffID 为未压缩 tar 的 digest，可用于后续 push 操作
	}

	// Step 3: Commit — 提交为 Committed 快照（纯元数据操作，目录不变）
	// name=key=name，提交后目录不变，只是 key 映射改变
	if err := snap.Commit(context.Background(), name, name); err != nil {
		snap.Remove(context.Background(), name)
		progress(StatusWarning, fmt.Sprintf("Commit 本地镜像快照失败: %v", err))
	}

	infoResult := &metadata.Image{
		Name:               name,
		Tag:                tag,
		ImageID:            imageID,
		CreatedAt:          time.Now().Format("2006-01-02 15:04:05"),
		TopLayerSnapshotID: name, // 本地构建镜像的 TopLayerSnapshotID 与 name 一致
		LayerDigests:       []string{layerDigest},
		Size:               size,
	}

	progress(StatusComplete, fmt.Sprintf("镜像 %s:%s (%s) 构建成功", name, tag, size))

	return infoResult, nil
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
// 新代码应优先使用 snap.Stat + diff.FSDir，仅在无法获取 Snapshotter 实例时使用 LayerDiffDir

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

// computeLayerDigest 计算目录的层摘要（已废弃）
// 对齐 Docker: SHA256(层内容)，但本实现仅哈希路径+大小（已知简化）。
// 新代码应使用 diff.NewLayerDiffer().Diff() 计算真正的 tar blob digest。
// 保留此函数作为 diff.Differ 失败时的兼容性降级方案。
//
// Deprecated: 使用 diff.NewLayerDiffer().Diff() 替代
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
// 对齐 Docker: 使用 B/KB/MB/GB 单位，与 docker images 的 SIZE 列格式一致
func formatSize(sizeBytes int64) string {
	const (
		KB = 1024
		MB = 1024 * KB
		GB = 1024 * MB
	)

	switch {
	case sizeBytes >= GB:
		return fmt.Sprintf("%.1fGB", float64(sizeBytes)/float64(GB))
	case sizeBytes >= MB:
		return fmt.Sprintf("%.1fMB", float64(sizeBytes)/float64(MB))
	case sizeBytes >= KB:
		return fmt.Sprintf("%.1fKB", float64(sizeBytes)/float64(KB))
	default:
		return fmt.Sprintf("%dB", sizeBytes)
	}
}

// calculateLayersSize 通过 diff.FSDir() 计算多个层的总大小（人类可读格式）
// 对齐 containerd: 通过 snap.Stat + diff.FSDir 获取层路径，支持可插拔 Snapshotter
// Service.CalculateLayersSize 和 pullFromRegistry 共享此函数，消除重复的大小计算逻辑
func calculateLayersSize(snapSvc *snapshots.Service, layerDigests []string) string {
	var totalSize int64
	for _, digest := range layerDigests {
		cacheID := content.DigestToCacheID(digest)
		info, err := snapSvc.Stat(context.Background(), cacheID)
		if err != nil {
			continue
		}
		fsDir := diff.FSDir(constants.SnapshotterDir, info.ID)
		totalSize += calculateRootFSSizeBytes(fsDir)
	}
	return formatSize(totalSize)
}

// ociConfigToImageConfig 将 OCI Image Config 转换为持久化的 ImageConfig
// 对齐 containerd: containerd 同样把 OCI Config 中的运行时字段（Cmd/Env/Entrypoint/WorkingDir/User/Labels）
// 作为镜像的"image config"暴露给容器创建流程
// ExposedPorts 在 OCI 规范中为 map[string]struct{}（Docker 风格），本项目 metadata 中简化为 []string；
// 此处暂不转换 ExposedPorts，待后续按需扩展
func ociConfigToImageConfig(oci *OCIImageConfig) metadata.ImageConfig {
	if oci == nil {
		return metadata.ImageConfig{}
	}
	return metadata.ImageConfig{
		Cmd:        oci.Config.Cmd,
		Env:        oci.Config.Env,
		WorkingDir: oci.Config.WorkingDir,
		Labels:     oci.Config.Labels,
		Entrypoint: oci.Config.Entrypoint,
		User:       oci.Config.User,
	}
}

// CreateLayerFromDir 从 upper 目录创建一个镜像层
// 对齐 containerd: 构建器中每条 RUN/COPY 指令生成新层。
// upperDir: OverlayFS upper 层的路径（包含该层的文件差异）
// parentDigest: 父层的 digest（用于建立 parent 链），为空表示基础层
// snap: Snapshotter 接口，通过 Stat + diff.FSDir 获取 fs 目录路径
// contentStore: blob 存储接口，用于通过 diff.Differ 计算真正的层 tar blob 和 digest
// differ: 层差异计算器，由插件系统注入，对齐 containerd: diff 服务可插拔
// 返回该层的 digest（同时也是 cacheID），由调用方负责把 digest 注册到 metadata.DB。
func CreateLayerFromDir(upperDir string, parentDigest string, snap snapshots.Snapshotter, contentStore content.Store, differ diff.Differ) (string, error) {
	ctx := context.Background()
	parentKey := ""
	if parentDigest != "" {
		parentKey = content.DigestToCacheID(parentDigest)
	}

	// 对齐 containerd: 通过 Prepare-Commit 注册快照元数据
	cacheID := fmt.Sprintf("layer-%x", time.Now().UnixNano())
	if _, err := snap.Prepare(ctx, cacheID, parentKey); err != nil {
		// Prepare 失败可能是因为快照已存在（缓存命中），不阻断构建
		fmt.Printf("  警告: Prepare 层快照失败: %v\n", err)
		// 回退：使用 computeLayerDigest 作为 cacheID（兼容旧逻辑）
		cacheID = computeLayerDigest(upperDir)
		if _, err := snap.Prepare(ctx, cacheID, parentKey); err != nil {
			fmt.Printf("  警告: Prepare 层快照（回退）失败: %v\n", err)
		}
	}

	// 获取快照的 Mount 信息，从中提取 fs/ 目录路径
	// 对齐 containerd: 通过 Mounts() 获取挂载信息，而非手动拼接路径
	upperMounts, err := snap.Mounts(ctx, cacheID)
	if err != nil {
		snap.Remove(ctx, cacheID)
		return "", fmt.Errorf("获取快照挂载信息失败: %w", err)
	}
	diffDir := diff.UpperDir(upperMounts)
	if diffDir == "" {
		snap.Remove(ctx, cacheID)
		return "", fmt.Errorf("无法从挂载信息中提取 fs 目录路径")
	}
	if err := os.MkdirAll(diffDir, 0755); err != nil {
		snap.Remove(ctx, cacheID)
		return "", fmt.Errorf("创建层 fs 目录失败: %w", err)
	}

	// 将 upper 目录内容合并到 fs/ 目录
	if err := mergeLocalRootfs(upperDir, diffDir); err != nil {
		snap.Remove(ctx, cacheID)
		return "", fmt.Errorf("合并 upper 到 fs 失败: %w", err)
	}

	// 对齐 containerd: 使用注入的 diff.Differ 计算真正的层 tar blob 和 digest
	// 获取父快照的 Mount 信息作为 lower
	info, err := snap.Stat(ctx, cacheID)
	if err != nil {
		snap.Remove(ctx, cacheID)
		return "", fmt.Errorf("获取快照信息失败: %w", err)
	}

	var lowerMounts []snapshots.Mount
	if info.Parent != "" {
		lowerMounts, err = snap.Mounts(ctx, info.Parent)
		if err != nil {
			fmt.Printf("  警告: 获取父快照挂载信息失败: %v，将使用空 lower\n", err)
		}
	}

	result, err := differ.Diff(ctx, lowerMounts, upperMounts, contentStore)
	if err != nil {
		// Differ 失败时回退到 computeLayerDigest（兼容性降级）
		fmt.Printf("  警告: diff.Differ 计算失败，回退到简化摘要: %v\n", err)
	} else {
		_ = result // diff blob 已写入 content store，digest 可用于后续 push 操作
	}

	// Commit: name=key=cacheID（纯元数据操作，目录不变）
	if err := snap.Commit(ctx, cacheID, cacheID); err != nil {
		snap.Remove(ctx, cacheID)
		fmt.Printf("  警告: Commit 层快照失败: %v\n", err)
	}

	return cacheID, nil
}

// mergeLocalRootfs 将源目录的内容合并到目标目录
// 用于本地构建镜像时将 rootfs 内容复制到快照的 fs/ 目录中
func mergeLocalRootfs(srcDir, dstDir string) error {
	return copyDir(srcDir, dstDir)
}

// copyDir 递归复制源目录内容到目标目录
// 已存在的文件会被覆盖，不存在的文件会被创建
func copyDir(srcDir, dstDir string) error {
	return filepath.Walk(srcDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		relPath, err := filepath.Rel(srcDir, path)
		if err != nil {
			return err
		}
		targetPath := filepath.Join(dstDir, relPath)

		if info.IsDir() {
			return os.MkdirAll(targetPath, info.Mode())
		}

		// 确保目标父目录存在
		if err := os.MkdirAll(filepath.Dir(targetPath), 0755); err != nil {
			return err
		}

		// 读取源文件内容并写入目标
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(targetPath, data, info.Mode())
	})
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
