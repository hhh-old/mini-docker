package images

import (
	"context"
	"fmt"

	bolt "go.etcd.io/bbolt"

	"mini-docker/containerd/content"
	"mini-docker/containerd/gc"
	"mini-docker/containerd/metadata"
	"mini-docker/containerd/snapshots"
	"mini-docker/utils"
)

/*
=======================================================================
  Image Service —— 对齐 containerd 的 Image Service 架构
=======================================================================

  Service 是镜像管理的核心入口，统一管理 Content Store、Metadata DB、
  Snapshotter 和 GC。所有镜像操作（Pull/List/Remove/Inspect/Resolve/Register）
  都通过 Service 执行。

  架构对齐 containerd:
  ┌─────────────────────────────────────────────────────────────┐
  │  Image Service                                              │
  │  ├── metadata.DB    ← 镜像/层/标签/快照元数据 (boltdb)       │
  │  ├── content.Store  ← blob 存储 (manifest/config/layer)     │
  │  ├── Snapshotter    ← 文件系统快照 (OverlayFS)              │
  │  └── LeaseManager   ← GC 保护机制                           │
  └─────────────────────────────────────────────────────────────┘

  Service 是镜像元数据的唯一持久化入口；底层 OCI 下载/解压逻辑由包内私有
  pull/pullFromRegistry/pullLocal 完成，Service 负责把结果写入 boltdb。
=======================================================================
*/

// Service 镜像管理服务
// 对齐 containerd 的 Image Service，统一管理 Content Store、Metadata DB、Snapshotter
// 所有镜像操作（Pull/List/Remove/Inspect/Resolve/Register）都通过 Service 执行
type Service struct {
	meta        *metadata.DB
	content     content.Store
	snapshotter snapshots.Snapshotter // 对齐 containerd: 依赖接口而非具体类型，支持可插拔 Snapshotter
	leaseMgr    *gc.LeaseManager
}

// 创建镜像管理服务
// 4 个依赖均为必传；任一为 nil 立即 panic。
func NewService(meta *metadata.DB, contentStore content.Store, snap snapshots.Snapshotter, leaseMgr *gc.LeaseManager) *Service {
	if meta == nil {
		panic("images.NewService: meta is required")
	}
	if contentStore == nil {
		panic("images.NewService: contentStore is required")
	}
	if snap == nil {
		panic("images.NewService: snap is required")
	}
	if leaseMgr == nil {
		panic("images.NewService: leaseMgr is required")
	}
	return &Service{
		meta:        meta,
		content:     contentStore,
		snapshotter: snap,
		leaseMgr:    leaseMgr,
	}
}

// Snapshotter 返回 Snapshotter 接口，供 builder 等外部包使用
// 对齐 containerd: 外部包通过 Service 获取 Snapshotter，而非直接依赖 overlay 具体类型
func (s *Service) Snapshotter() snapshots.Snapshotter {
	return s.snapshotter
}

// Pull 拉取镜像（支持 Registry 远程拉取 + 本地构建双模式）
// 流程:
//  1. 创建 Lease 保护（防止 GC 在拉取过程中清理）
//  2. 调用私有 pull 函数（执行 OCI 下载/解压/rootfs 组装）
//  3. 解压后即时通过 Snapshotter 注册每层为 Committed 快照（建立 parent 链）
//  4. 同步到 metadata.DB（写入 image/layer/tag）
//  5. 删除 Lease
func (s *Service) Pull(ctx context.Context, imageRef string, progress ProgressFunc) (*metadata.Image, error) {
	// 对齐 Docker: 先检查本地是否已有该镜像，已有则直接返回
	name, tag := utils.ParseImageTag(imageRef)
	if tag == "" {
		tag = "latest"
	}
	if existing, err := s.Resolve(ctx, name+":"+tag); err == nil {
		progress(StatusComplete, fmt.Sprintf("镜像 %s:%s 已存在本地", name, tag))
		return existing, nil
	}
	//创建 Lease（GC 保护机制）
	//为什么需要 Lease : 拉取过程中会创建 blob、写入快照、注册元数据。如果此时 GC 触发，可能把这些"半成品"误清理。Lease 用于告诉 GC: "这些对象正在被使用，不要清理"。
	leaseID, err := s.leaseMgr.Create(ctx)
	if err != nil {
		return nil, fmt.Errorf("创建 lease 失败: %w", err)
	}
	defer s.leaseMgr.Delete(ctx, leaseID) //defer Delete 保证流程结束（无论成功或失败）都会删除 lease，解除保护。

	info, err := pull(imageRef, progress, s.content, s.snapshotter)
	if err != nil {
		return nil, err
	}

	progress(StatusRegistering, "同步元数据到 boltdb...")
	if err := s.Register(ctx, info); err != nil {
		progress(StatusWarning, fmt.Sprintf("同步到 metadata.DB 失败: %v", err))
	}
	progress(StatusRegistering, "元数据同步完成")

	return info, nil
}

// Register 把 metadata.Image 同步到 boltdb。
// Size 字段因 omitempty 不会被写入。
func (s *Service) Register(ctx context.Context, info *metadata.Image) error {
	return s.meta.Update(func(tx *bolt.Tx) error {
		if err := metadata.SaveImage(tx, info); err != nil {
			return fmt.Errorf("保存镜像元数据失败: %w", err)
		}

		if err := metadata.SaveTag(tx, info.Name, info.Tag, info.ImageID); err != nil {
			return fmt.Errorf("保存标签失败: %w", err)
		}

		return nil
	})
}

// List 列出本地镜像
// 仅从 metadata.DB 读取，Size 通过 Snapshotter.DiffPath() 实时计算后写入 Image.Size
// （不写回 boltdb，仅响应时填充）
func (s *Service) List(ctx context.Context) ([]*metadata.Image, error) {
	var images []*metadata.Image
	err := s.meta.View(func(tx *bolt.Tx) error {
		manifests, err := metadata.ListImages(tx)
		if err != nil {
			return err
		}
		for _, m := range manifests {
			// 镜像大小通过 Snapshotter.DiffPath() 获取层路径后计算
			m.Size = s.CalculateLayersSize(context.Background(), m.LayerDigests)
			images = append(images, m)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	return images, nil
}

// Remove 删除本地镜像
// 使用单个 metadata 事务完成所有元数据删除，文件级清理在事务提交成功后执行
// 对齐 containerd: 先标记删除元数据（事务），再清理文件，确保崩溃时不会出现"文件存在但元数据缺失"或"元数据存在但文件缺失"
// 对齐 Docker: 支持 name:tag 和 imageID 两种引用方式（通过 metadata.ResolveImageRef 统一解析）
func (s *Service) Remove(ctx context.Context, imageRef string) error {
	// 收集需要在事务提交后清理的文件/快照信息
	var orphanedLayers []struct {
		digest  string
		cacheID string
	}
	var imageID string

	// 单个事务: 解析引用 → 查找孤儿层 → 删除 image/tag/layer 元数据
	if err := s.meta.Update(func(tx *bolt.Tx) error {
		// 统一解析镜像引用（支持 name:tag 和 imageID 两种格式）
		m, err := metadata.ResolveImageRef(tx, imageRef)
		if err != nil {
			return err
		}
		imageID = m.ImageID

		// 查找孤儿层（只被当前镜像引用的层）
		for _, layerDigest := range m.LayerDigests {
			if !metadata.HasOtherRefs(tx, layerDigest, imageID) {
				cacheID := content.DigestToCacheID(layerDigest)
				orphanedLayers = append(orphanedLayers, struct {
					digest  string
					cacheID string
				}{layerDigest, cacheID})
			}
		}

		// 删除镜像和标签元数据（事务内）
		if err := metadata.DeleteImage(tx, imageID); err != nil {
			return err
		}
		// ResolveImageRef 返回的 *Image 已包含 Name/Tag，直接用于删除 tag 映射
		return metadata.RemoveTag(tx, m.Name, m.Tag)
	}); err != nil {
		return fmt.Errorf("删除镜像元数据失败: %w", err)
	}

	// 事务提交成功后，清理文件（文件删除失败不阻塞，GC 会最终清理）
	for _, layer := range orphanedLayers {
		// 对齐 containerd: 通过 Snapshotter.Remove() 和 contentStore.Delete() 统一删除
		// 确保文件和 BoltDB 元数据在同一操作中清理，避免不一致
		if err := s.snapshotter.Remove(ctx, layer.cacheID); err != nil {
			// 快照可能不存在（未注册），忽略错误继续清理
		}
		s.content.Delete(ctx, layer.digest)
	}

	return nil
}

// Inspect 获取镜像详细信息（boltdb 中的原始元数据，无 Size）
// 对齐 Docker: 支持 name:tag 和 imageID 两种引用方式（通过 metadata.ResolveImageRef 统一解析）
func (s *Service) Inspect(ctx context.Context, imageRef string) (*metadata.Image, error) {
	var manifest *metadata.Image
	err := s.meta.View(func(tx *bolt.Tx) error {
		m, err := metadata.ResolveImageRef(tx, imageRef)
		if err != nil {
			return err
		}
		manifest = m
		return nil
	})
	if err != nil {
		return nil, err
	}
	return manifest, nil
}

// Resolve 解析镜像引用，返回镜像信息（含实时计算的 Size）
// 对齐 Docker: 支持 name:tag 和 imageID 两种引用方式（通过 metadata.ResolveImageRef 统一解析）
func (s *Service) Resolve(ctx context.Context, imageRef string) (*metadata.Image, error) {
	var info *metadata.Image
	err := s.meta.View(func(tx *bolt.Tx) error {
		m, err := metadata.ResolveImageRef(tx, imageRef)
		if err != nil {
			return err
		}

		// 镜像大小通过 Snapshotter.DiffPath() 获取层路径后计算
		m.Size = s.CalculateLayersSize(context.Background(), m.LayerDigests)
		info = m
		return nil
	})
	if err != nil {
		return nil, err
	}
	return info, nil
}

// ---------------------------------------------------------------------------
// Service 扩展方法（对齐 containerd 的 Image Service 接口）
// ---------------------------------------------------------------------------

// CalculateLayersSize 计算多个层的总大小（人类可读格式）
// 对齐 containerd: 通过 Snapshotter.DiffPath() 获取层路径，而非直接拼接常量路径
// 这样支持可插拔 Snapshotter，切换实现时路径自动适配
// 委托到包级 calculateLayersSize 函数，与 pullFromRegistry 共享同一实现
func (s *Service) CalculateLayersSize(ctx context.Context, layerDigests []string) string {
	return calculateLayersSize(s.snapshotter, layerDigests)
}
