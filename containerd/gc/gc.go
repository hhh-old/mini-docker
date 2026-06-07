package gc

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	bolt "go.etcd.io/bbolt"

	"mini-docker/containerd/content"
	"mini-docker/containerd/metadata"
	"mini-docker/containerd/snapshots"
	"mini-docker/containerstore"
	"mini-docker/utils"
)

/*
=======================================================================
  GC（垃圾回收）—— 对齐 containerd 的三色标记清除算法
=======================================================================

  引用关系链：
  - Tags → ImageID → LayerDigests → Content (blob)
  - ContainerInfo → Image → LayerDigests → Content (blob)
  - ContainerInfo → SnapshotKey (containerID，容器可写层)
  - Snapshot → Parent → Parent (链式，递归标记)
  - Lease → Content/Snapshot (保护机制)

  标记阶段（Mark）：
  1. 从 Tags 出发，标记所有可达的 ImageID 和 LayerDigests
  2. 从 ContainerInfo 出发，标记容器使用的镜像层和快照
  3. 从 Lease 出发，标记被保护的对象
  4. 从 Active 快照出发，递归标记其父链

  清扫阶段（Sweep）：
  1. 遍历所有 content，删除未被标记的
  2. 遍历所有 snapshot，删除未被标记的（注意：先删除叶子节点，再删除父节点）

  与之前版本的差异：
  - 快照不再全部标记为可达，而是根据引用关系精确标记
  - 增加了对容器引用的跟踪
  - 删除快照时考虑父子关系，避免删除被引用的父快照

=======================================================================
*/

// Collector GC 收集器
type Collector struct {
	db          *metadata.DB
	content     ContentDeleter
	snapshotter SnapshotDeleter
	interval    time.Duration
	stopCh      chan struct{}
	wg          sync.WaitGroup
}

// ContentDeleter Content 删除接口
type ContentDeleter interface {
	Delete(ctx context.Context, digest string) error
	Walk(ctx context.Context, fn func(digest string, size int64) error) error
}

// SnapshotDeleter Snapshot 删除接口
type SnapshotDeleter interface {
	Remove(ctx context.Context, key string) error
	Walk(ctx context.Context, fn func(name string) error) error
}

// GCStats GC 统计信息
type GCStats struct {
	ContentDeleted int
	SnapDeleted    int
	Elapsed        time.Duration
}

// NewCollector 创建 GC 收集器
func NewCollector(db *metadata.DB, content ContentDeleter, snapshotter SnapshotDeleter, interval time.Duration) *Collector {
	return &Collector{
		db:          db,
		content:     content,
		snapshotter: snapshotter,
		interval:    interval,
		stopCh:      make(chan struct{}),
	}
}

// Start 启动周期性 GC
func (g *Collector) Start() {
	g.wg.Add(1)
	go func() {
		defer g.wg.Done()
		ticker := time.NewTicker(g.interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				stats, err := g.Run(ctx)
				cancel()
				if err != nil {
					log.Printf("[gc] 周期性 GC 执行失败: %v\n", err)
				} else if stats.ContentDeleted > 0 || stats.SnapDeleted > 0 {
					log.Printf("[gc] 周期性 GC 完成: 删除 content=%d, snapshot=%d, 耗时=%v\n",
						stats.ContentDeleted, stats.SnapDeleted, stats.Elapsed)
				}
			case <-g.stopCh:
				return
			}
		}
	}()
}

// Stop 停止 GC
func (g *Collector) Stop() {
	close(g.stopCh)
	g.wg.Wait()
}

// Run 执行一次 GC
func (g *Collector) Run(ctx context.Context) (GCStats, error) {
	start := time.Now()
	var stats GCStats

	reachableDigests, reachableSnapKeys, err := g.mark(ctx)
	if err != nil {
		return stats, fmt.Errorf("标记阶段失败: %w", err)
	}

	contentDeleted, err := g.sweepContent(ctx, reachableDigests)
	if err != nil {
		return stats, fmt.Errorf("清扫 content 失败: %w", err)
	}
	stats.ContentDeleted = contentDeleted

	snapDeleted, err := g.sweepSnapshots(ctx, reachableSnapKeys)
	if err != nil {
		return stats, fmt.Errorf("清扫 snapshot 失败: %w", err)
	}
	stats.SnapDeleted = snapDeleted

	stats.Elapsed = time.Since(start)
	return stats, nil
}

// mark 标记阶段：找出所有可达的 content 和 snapshot
// 对齐 containerd: 从多个根出发，构建完整的引用图
func (g *Collector) mark(ctx context.Context) (map[string]struct{}, map[string]struct{}, error) {
	reachableDigests := make(map[string]struct{})
	reachableSnapKeys := make(map[string]struct{})

	// 1. 从 Tags 出发，标记所有可达的 ImageID 和 LayerDigests
	if err := g.db.View(func(tx *bolt.Tx) error {
		tagMap, err := metadata.ListTags(tx)
		if err != nil {
			return fmt.Errorf("读取标签失败: %w", err)
		}

		reachableImageIDs := make(map[string]struct{})
		for _, tags := range tagMap {
			for _, imageID := range tags {
				reachableImageIDs[imageID] = struct{}{}
			}
		}

		for imageID := range reachableImageIDs {
			img, err := metadata.LoadImage(tx, imageID)
			if err != nil {
				continue
			}
			// 标记镜像 ID（作为 content 存储）
			reachableDigests[imageID] = struct{}{}
			// 标记所有层 digest
			for _, layerDigest := range img.Layers {
				reachableDigests[layerDigest] = struct{}{}
				// 层 digest 对应的快照也应该被标记
				// 层的 cacheID 就是 digest 的 hex 部分，也是快照的 key
				cacheID := content.DigestToCacheID(layerDigest)
				reachableSnapKeys[cacheID] = struct{}{}
			}
		}

		return nil
	}); err != nil {
		return nil, nil, err
	}

	// 2. 从 ContainerInfo 出发，标记容器使用的镜像层和快照
	// 对齐 containerd: 通过 containerstore 包获取容器信息，而非直接读 JSON 文件
	// 这样保证了访问方式的一致性，容器存储格式变化时只需修改 containerstore 包
	if containers, err := containerstore.ListContainers(); err == nil {
		for _, containerInfo := range containers {
			// 标记容器的快照（容器 ID 即为快照 key）
			if containerInfo.ID != "" {
				reachableSnapKeys[containerInfo.ID] = struct{}{}
			}
			// 标记容器使用的镜像的层
			if containerInfo.Image != "" {
				// 解析镜像名:tag
				name, tag := utils.ParseImageTag(containerInfo.Image)
				if tag == "" {
					tag = "latest"
				}
				// 从 boltdb 查找镜像 ID
				g.db.View(func(tx *bolt.Tx) error {
					imageID, err := metadata.ResolveImageID(tx, name, tag)
					if err != nil {
						return nil
					}
					img, err := metadata.LoadImage(tx, imageID)
					if err != nil {
						return nil
					}
					for _, layerDigest := range img.Layers {
						reachableDigests[layerDigest] = struct{}{}
						cacheID := content.DigestToCacheID(layerDigest)
						reachableSnapKeys[cacheID] = struct{}{}
					}
					return nil
				})
			}
		}
	}

	// 3. 从 Lease 出发，标记被保护的对象
	// 对齐 containerd: lease 对象有类型标识（content/snapshot），GC 根据类型分别标记
	if err := g.db.View(func(tx *bolt.Tx) error {
		return metadata.WalkLeases(tx, func(info *metadata.LeaseInfo) error {
			for _, obj := range info.Objects {
				switch obj.Type {
				case metadata.LeaseObjectContent:
					reachableDigests[obj.ID] = struct{}{}
				case metadata.LeaseObjectSnapshot:
					reachableSnapKeys[obj.ID] = struct{}{}
				}
			}
			return nil
		})
	}); err != nil {
		return nil, nil, err
	}

	// 4. 从已标记的快照出发，递归标记其父链
	// 对齐 containerd: 快照的父快照也应该被保护
	if err := g.markSnapshotParents(reachableSnapKeys); err != nil {
		return nil, nil, err
	}

	return reachableDigests, reachableSnapKeys, nil
}

// markSnapshotParents 递归标记快照的父链
// 对齐 containerd: 如果一个快照被引用，其所有祖先快照也应该被保护
func (g *Collector) markSnapshotParents(reachableSnapKeys map[string]struct{}) error {
	// 收集所有需要处理的快照
	toProcess := make([]string, 0, len(reachableSnapKeys))
	for key := range reachableSnapKeys {
		toProcess = append(toProcess, key)
	}

	// BFS 遍历父链
	processed := make(map[string]struct{})
	for len(toProcess) > 0 {
		key := toProcess[0]
		toProcess = toProcess[1:]

		if _, done := processed[key]; done {
			continue
		}
		processed[key] = struct{}{}

		var snapInfo *snapshots.Info
		if err := g.db.View(func(tx *bolt.Tx) error {
			var err error
			snapInfo, err = metadata.LoadSnapshot(tx, key)
			return err
		}); err != nil {
			// 快照不存在，跳过
			continue
		}

		// 标记父快照
		if snapInfo.Parent != "" {
			if _, ok := reachableSnapKeys[snapInfo.Parent]; !ok {
				reachableSnapKeys[snapInfo.Parent] = struct{}{}
				toProcess = append(toProcess, snapInfo.Parent)
			}
		}
	}

	return nil
}

// sweepContent 清扫未被标记的 content
func (g *Collector) sweepContent(ctx context.Context, reachable map[string]struct{}) (int, error) {
	deleted := 0
	var toDelete []string

	if err := g.content.Walk(ctx, func(digest string, size int64) error {
		if _, ok := reachable[digest]; !ok {
			toDelete = append(toDelete, digest)
		}
		return nil
	}); err != nil {
		return 0, fmt.Errorf("遍历 content 失败: %w", err)
	}

	for _, digest := range toDelete {
		if err := g.content.Delete(ctx, digest); err != nil {
			log.Printf("[gc] 删除 content %s 失败: %v\n", digest, err)
			continue
		}
		// 同时删除 boltdb 中的层元数据
		g.db.Update(func(tx *bolt.Tx) error {
			metadata.DeleteLayer(tx, digest)
			return nil
		})
		deleted++
	}

	return deleted, nil
}

// sweepSnapshots 清扫未被标记的 snapshot
// 对齐 containerd: 先删除叶子节点，再删除父节点，避免删除被引用的父快照
func (g *Collector) sweepSnapshots(ctx context.Context, reachable map[string]struct{}) (int, error) {
	deleted := 0

	// 收集所有快照及其父信息
	type snapInfo struct {
		name   string
		parent string
	}
	var allSnaps []snapInfo
	parentCount := make(map[string]int) // 记录每个快照被多少子快照引用

	if err := g.db.View(func(tx *bolt.Tx) error {
		return metadata.WalkSnapshots(tx, func(info *snapshots.Info) error {
			allSnaps = append(allSnaps, snapInfo{name: info.Name, parent: info.Parent})
			if info.Parent != "" {
				parentCount[info.Parent]++
			}
			return nil
		})
	}); err != nil {
		return 0, fmt.Errorf("遍历 snapshot 元数据失败: %w", err)
	}

	// 找出未被标记的快照
	var toDelete []string
	for _, snap := range allSnaps {
		if _, ok := reachable[snap.name]; !ok {
			toDelete = append(toDelete, snap.name)
		}
	}

	// 按拓扑排序删除：先删除没有子快照的（叶子节点）
	// 重复多轮，直到没有可删除的
	for len(toDelete) > 0 {
		var nextRound []string
		var deletedThisRound []string

		for _, name := range toDelete {
			// 检查是否还有子快照引用它
			if parentCount[name] > 0 {
				// 还有子快照引用，留到下一轮
				nextRound = append(nextRound, name)
				continue
			}

			// 可以安全删除
			if err := g.snapshotter.Remove(ctx, name); err != nil {
				log.Printf("[gc] 删除 snapshot %s 失败: %v\n", name, err)
				continue
			}
			deletedThisRound = append(deletedThisRound, name)
			deleted++

			// 减少父快照的引用计数
			for _, snap := range allSnaps {
				if snap.name == name && snap.parent != "" {
					parentCount[snap.parent]--
				}
			}
		}

		// 如果这一轮没有删除任何快照，说明存在循环引用或其他问题
		if len(deletedThisRound) == 0 {
			// 强制删除剩余的（可能是孤立快照）
			for _, name := range toDelete {
				if err := g.snapshotter.Remove(ctx, name); err != nil {
					log.Printf("[gc] 强制删除 snapshot %s 失败: %v\n", name, err)
				} else {
					deleted++
				}
			}
			break
		}

		toDelete = nextRound
	}

	return deleted, nil
}
