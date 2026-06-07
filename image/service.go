package image

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

// NewService 创建镜像管理服务
func NewService(meta *metadata.DB, contentStore content.Store, snap snapshots.Snapshotter, leaseMgr *gc.LeaseManager) *Service {
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
func (s *Service) Pull(ctx context.Context, imageRef string, progress ProgressFunc) (*ImageInfo, error) {
	// 对齐 Docker: 先检查本地是否已有该镜像，已有则直接返回
	if s.meta != nil {
		name, tag := utils.ParseImageTag(imageRef)
		if tag == "" {
			tag = "latest"
		}
		if existing, err := s.Resolve(ctx, name+":"+tag); err == nil {
			progress("complete", fmt.Sprintf("镜像 %s:%s 已存在本地", name, tag))
			return existing, nil
		}
	}

	if s.leaseMgr != nil {
		leaseID, err := s.leaseMgr.Create(ctx)
		if err != nil {
			return nil, fmt.Errorf("创建 lease 失败: %w", err)
		}
		defer s.leaseMgr.Delete(ctx, leaseID)

		info, err := pull(imageRef, progress, s.content, s.snapshotter)
		if err != nil {
			return nil, err
		}

		progress("registering", "注册镜像元数据...")

		// 对齐 containerd: 层快照已在 pull 流程中通过 Snapshotter.UnpackLayer 原子注册
		// （解压+元数据注册在同一操作中完成），不再需要事后补注册

		progress("registering", "同步元数据到 boltdb...")
		if err := s.register(ctx, info, leaseID); err != nil {
			progress("warning", fmt.Sprintf("同步到 metadata.DB 失败: %v", err))
		}
		progress("registering", "元数据同步完成")

		return info, nil
	}

	return pull(imageRef, progress, s.content, s.snapshotter)
}

// Register 注册一个已构建好的镜像到 metadata.DB
// 供 builder 使用：builder 在本地完成 rootfs/layers 组装后，通过本方法写入元数据。
// leaseID 可为空字符串。
func (s *Service) Register(ctx context.Context, info *ImageInfo) error {
	return s.register(ctx, info, "")
}

// register 把 ImageInfo 同步到 boltdb
// 如果 leaseID 非空，会把每个层 digest 注册到 lease（GC 保护）
func (s *Service) register(ctx context.Context, info *ImageInfo, leaseID string) error {
	if s.meta == nil {
		return nil
	}

	manifest := &metadata.ImageManifest{
		ImageID:    info.ImageID,
		Name:       info.Name,
		Tag:        info.Tag,
		CreatedAt:  info.CreatedAt,
		RootFSPath: info.RootFS,
		Layers:     info.Layers,
		Config:     metadata.ImageConfig{},
	}

	// 收集需要注册到 lease 的对象，在事务外批量注册，避免 boltdb 死锁
	// （boltdb 同一时刻只允许一个读写事务，事务内再开 Update 会死锁）
	var leaseObjects []struct {
		objType metadata.LeaseObjectType
		objID   string
	}

	if err := s.meta.Update(func(tx *bolt.Tx) error {
		if err := metadata.SaveImage(tx, manifest); err != nil {
			return fmt.Errorf("保存镜像元数据失败: %w", err)
		}

		for _, layerDigest := range manifest.Layers {
			// 对齐 containerd: CacheID 是层的快照标识（digest 的 hex 部分），
			// 与 Snapshotter 中的 key 格式一致，而非完整的 digest（sha256:...）
			cacheID := content.DigestToCacheID(layerDigest)
			layerInfo := &metadata.LayerInfo{
				Digest:  layerDigest,
				CacheID: cacheID,
			}
			if err := metadata.SaveLayer(tx, layerInfo); err != nil {
				fmt.Printf("  警告: 保存层 %s 元数据失败: %v\n", layerDigest[:16], err)
			}

			if s.leaseMgr != nil && leaseID != "" {
				// 对齐 containerd: lease 同时保护 content digest 和 snapshot key
				// content digest 保护 blob 文件，snapshot key 保护快照目录和元数据
				// 使用类型标识区分，避免 GC 启发式误判
				leaseObjects = append(leaseObjects,
					struct {
						objType metadata.LeaseObjectType
						objID   string
					}{metadata.LeaseObjectContent, layerDigest},
					struct {
						objType metadata.LeaseObjectType
						objID   string
					}{metadata.LeaseObjectSnapshot, cacheID},
				)
			}
		}

		if err := metadata.SaveTag(tx, info.Name, info.Tag, info.ImageID); err != nil {
			return fmt.Errorf("保存标签失败: %w", err)
		}

		return nil
	}); err != nil {
		return err
	}

	// 在事务外批量注册 lease 对象，避免 boltdb 死锁
	for _, obj := range leaseObjects {
		if err := s.leaseMgr.AddObject(ctx, leaseID, obj.objType, obj.objID); err != nil {
			fmt.Printf("  警告: 添加 lease 对象失败: %v\n", err)
		}
	}

	return nil
}

// List 列出本地镜像
// 仅从 metadata.DB 读取
func (s *Service) List(ctx context.Context) ([]*ImageInfo, error) {
	if s.meta == nil {
		return nil, nil
	}

	var images []*ImageInfo
	err := s.meta.View(func(tx *bolt.Tx) error {
		manifests, err := metadata.ListImages(tx)
		if err != nil {
			return err
		}
		for _, m := range manifests {
			// 镜像大小通过 Snapshotter.DiffPath() 获取层路径后计算，不再直接拼接路径
			size := s.CalculateLayersSize(context.Background(), m.Layers)
			images = append(images, &ImageInfo{
				Name:      m.Name,
				Tag:       m.Tag,
				ImageID:   m.ImageID,
				Size:      size,
				CreatedAt: m.CreatedAt,
				RootFS:    m.RootFSPath,
				Layers:    m.Layers,
			})
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
func (s *Service) Remove(ctx context.Context, imageRef string) error {
	if s.meta == nil {
		return fmt.Errorf("metadata.DB 未初始化")
	}

	name, tag := utils.ParseImageTag(imageRef)
	if tag == "" {
		tag = "latest"
	}

	// 收集需要在事务提交后清理的文件/快照信息
	var orphanedLayers []struct {
		digest  string
		cacheID string
	}
	var imageID string

	// 单个事务: 解析 imageID → 查找孤儿层 → 删除 image/tag/layer 元数据
	if err := s.meta.Update(func(tx *bolt.Tx) error {
		id, err := metadata.ResolveImageID(tx, name, tag)
		if err != nil {
			return err
		}
		imageID = id

		m, err := metadata.LoadImage(tx, imageID)
		if err != nil {
			return err
		}

		// 查找孤儿层（只被当前镜像引用的层）
		for _, layerDigest := range m.Layers {
			if !metadata.HasOtherRefs(tx, layerDigest, imageID) {
				cacheID := content.DigestToCacheID(layerDigest)
				orphanedLayers = append(orphanedLayers, struct {
					digest  string
					cacheID string
				}{layerDigest, cacheID})
				// 删除层元数据（事务内）
				metadata.DeleteLayer(tx, layerDigest)
			}
		}

		// 删除镜像和标签元数据（事务内）
		if err := metadata.DeleteImage(tx, imageID); err != nil {
			return err
		}
		return metadata.RemoveTag(tx, name, tag)
	}); err != nil {
		return fmt.Errorf("删除镜像元数据失败: %w", err)
	}

	// 事务提交成功后，清理文件（文件删除失败不阻塞，GC 会最终清理）
	for _, layer := range orphanedLayers {
		// 对齐 containerd: 通过 Snapshotter.Remove() 和 contentStore.Delete() 统一删除
		// 确保文件和 BoltDB 元数据在同一操作中清理，避免不一致
		if s.snapshotter != nil {
			if err := s.snapshotter.Remove(ctx, layer.cacheID); err != nil {
				// 快照可能不存在（未注册），忽略错误继续清理
			}
		}
		if s.content != nil {
			s.content.Delete(ctx, layer.digest)
		}
	}

	return nil
}

// Inspect 获取镜像详细信息
func (s *Service) Inspect(ctx context.Context, imageRef string) (*ImageManifest, error) {
	if s.meta == nil {
		return nil, fmt.Errorf("metadata.DB 未初始化")
	}

	name, tag := utils.ParseImageTag(imageRef)
	if tag == "" {
		tag = "latest"
	}

	var manifest *metadata.ImageManifest
	err := s.meta.View(func(tx *bolt.Tx) error {
		imageID, err := metadata.ResolveImageID(tx, name, tag)
		if err != nil {
			return err
		}
		m, err := metadata.LoadImage(tx, imageID)
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

// Resolve 解析镜像引用，返回镜像信息
func (s *Service) Resolve(ctx context.Context, imageRef string) (*ImageInfo, error) {
	if s.meta == nil {
		return nil, fmt.Errorf("metadata.DB 未初始化")
	}

	name, tag := utils.ParseImageTag(imageRef)
	if tag == "" {
		tag = "latest"
	}

	var info *ImageInfo
	err := s.meta.View(func(tx *bolt.Tx) error {
		imageID, err := metadata.ResolveImageID(tx, name, tag)
		if err != nil {
			return err
		}
		m, err := metadata.LoadImage(tx, imageID)
		if err != nil {
			return err
		}

		// 镜像大小通过 Snapshotter.DiffPath() 获取层路径后计算，不再直接拼接路径
		size := s.CalculateLayersSize(context.Background(), m.Layers)

		info = &ImageInfo{
			Name:      m.Name,
			Tag:       m.Tag,
			ImageID:   m.ImageID,
			Size:      size,
			CreatedAt: m.CreatedAt,
			RootFS:    m.RootFSPath,
			Layers:    m.Layers,
		}
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
