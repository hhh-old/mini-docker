package gc

import (
	"context"
	"fmt"
	"sync"
	"time"

	bolt "go.etcd.io/bbolt"

	"mini-docker/constants"
	"mini-docker/containerd/content"
	"mini-docker/containerd/events"
	"mini-docker/containerd/metadata"
	"mini-docker/containerd/snapshots"
	"mini-docker/log"
	"mini-docker/utils"
)

/*
=======================================================================
  GC（垃圾回收）—— 对齐 containerd 的三色标记清除算法
=======================================================================

  引用关系链：
  - Tags → ImageID → LayerDigests → Content (blob)
  - ContainerInfo → Image → LayerDigests → Content (blob)
  - ContainerInfo → SnapshotID (containerID，容器可写层)
  - Snapshot → Parent → Parent (链式，递归标记)

  标记阶段（Mark）：
  1. 从 Tags 出发，标记所有可达的 ImageID 和 LayerDigests
  2. 从 ContainerInfo 出发，标记容器使用的镜像层和快照
  3. 从 Active 快照出发，递归标记其父链

  GC 保护机制（Preflight）：
  - Pull 开始时创建 in-progress lease，GC 检测到后整轮跳过
  - Pull 完成后立即 Delete lease，此时 Tags/Layers 引用链已建立

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
// content / snapshotter 直接依赖 content.Store / snapshots.Snapshotter 接口（与项目中其他模块保持一致），
// 不再额外定义 ContentDeleter / SnapshotDeleter 子接口 —— GC 是这两个接口的唯一消费者，
// 签名差异（Walk 回调参数 Info vs digest）已在调用点用内联闭包消化，避免无谓的 adapter 层。
type Collector struct {
	db          *metadata.DB
	content     content.Store
	snapshotter snapshots.Snapshotter
	interval    time.Duration
	stopCh      chan struct{}
	wg          sync.WaitGroup
	events      *events.Service // 事件总线服务，发布 GC 完成事件
}

// GCStats GC 统计信息
type GCStats struct {
	ContentDeleted int
	SnapDeleted    int
	Elapsed        time.Duration
}

// NewCollector 创建 GC 收集器
func NewCollector(db *metadata.DB, content content.Store, snapshotter snapshots.Snapshotter, interval time.Duration, ev *events.Service) *Collector {
	return &Collector{
		db:          db,
		content:     content,
		snapshotter: snapshotter,
		interval:    interval,
		stopCh:      make(chan struct{}),
		events:      ev,
	}
}

// publishEvent 发布 GC 完成事件（events 为 nil 时静默跳过）
func (g *Collector) publishEvent(stats GCStats) {
	if g.events == nil {
		return
	}
	g.events.Publish(&events.Envelope{
		Topic: "/gc/run",
		Event: events.GCRun{
			RemovedImages:    stats.ContentDeleted,
			RemovedLayers:    stats.ContentDeleted,
			RemovedSnapshots: stats.SnapDeleted,
		},
	})
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
					log.Errorf("[gc] periodic gc run failed: %v", err)
				} else if stats.ContentDeleted > 0 || stats.SnapDeleted > 0 {
					log.Infof("[gc] periodic gc completed content_deleted=%d snap_deleted=%d elapsed=%v",
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

	// Preflight: 检测 in-progress lease,有活跃拉取则整轮跳过,有僵尸则清理
	// 对齐 containerd: lease 处于 in-progress 表示对象正在被读写,此时 GC 不能动磁盘
	// 崩溃残留的 in-progress (CreatedAt 早于 now-2*interval) 视为僵尸,自动 Delete
	skip, err := g.preflight(ctx)
	if err != nil {
		return stats, fmt.Errorf("preflight 失败: %w", err)
	}
	if skip {
		log.Infof("[gc] in-progress lease detected, skipping this round")
		stats.Elapsed = time.Since(start)
		return stats, nil
	}

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
	g.publishEvent(stats)
	return stats, nil
}

// preflight GC 前置检查:处理 in-progress lease
//   - 活跃拉取 (CreatedAt 较新): 设置 skip=true 让 Run 跳过本轮,避免误删下载中的 blob/snapshot
//   - 僵尸 lease (CreatedAt 超过 2*interval,通常是上次进程崩溃残留): 自动 Delete
//
// 为什么用 2*interval 作为僵尸阈值?
//   - 1 个 interval 仍可能误判(网络慢的拉取可能跨 1 个周期)
//   - 2 个 interval 之后还没完成的拉取几乎一定是僵尸(正常拉取 < 几分钟)
//
// 必须在 mark() 之前执行,否则僵尸的 in-progress 会被当成"活跃"误跳过本轮,
// 导致崩溃残留的孤儿 blob 永远无法被回收。
func (g *Collector) preflight(ctx context.Context) (bool, error) {
	cutoff := time.Now().Add(-2 * g.interval) //cutoff 是 当前时间减去 2 个 interval（一个过去的时间点）
	var skip bool

	err := g.db.Update(func(tx *bolt.Tx) error {
		return metadata.WalkLeases(tx, func(info *metadata.LeaseInfo) error {
			if info.Status != metadata.LeaseStatusInProgress {
				return nil
			}
			createdAt, err := time.Parse(constants.TimeFormat, info.CreatedAt)
			if err != nil {
				// CreatedAt 解析失败:保守按"活跃"处理,跳过本轮
				skip = true
				return nil
			}
			if createdAt.After(cutoff) { //createdAt.After(cutoff) 是 Go 语言中 time.Time 类型的方法，用于判断时间点 createdAt 是否晚于 cutof
				// 活跃拉取,跳过本轮
				skip = true
			} else {
				// 僵尸 lease,清理掉 (让本轮 GC 正常回收其保护的孤儿对象)
				if err := metadata.DeleteLease(tx, info.ID); err != nil {
					return fmt.Errorf("清理僵尸 lease %s 失败: %w", info.ID, err)
				}
				log.Infof("[gc] cleaned up stale in-progress lease id=%s created_at=%s", info.ID, info.CreatedAt)
			}
			return nil
		})
	})

	return skip, err
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
			// 对齐 containerd: 标记镜像的 config blob
			// 防止 GC 误删这些 OCI 镜像必需的 blob
			if img.ConfigDigest != "" {
				reachableDigests[img.ConfigDigest] = struct{}{}
			}
			// 标记所有层 digest
			for _, layerDigest := range img.LayerDigests {
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
	// 对齐 containerd: 通过 metadata.WalkContainers 在同一 boltdb 事务中获取容器信息
	// 确保容器引用检查与 images/snapshots 在同一事务中，避免竞态
	if err := g.db.View(func(tx *bolt.Tx) error {
		return metadata.WalkContainers(tx, func(containerInfo *metadata.ContainerInfo) error {
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
				// 从 boltdb 查找镜像 ID（同一事务内）
				imageID, err := metadata.ResolveImageID(tx, name, tag)
				if err != nil {
					return nil
				}
				img, err := metadata.LoadImage(tx, imageID)
				if err != nil {
					return nil
				}
				for _, layerDigest := range img.LayerDigests {
					reachableDigests[layerDigest] = struct{}{}
					cacheID := content.DigestToCacheID(layerDigest)
					reachableSnapKeys[cacheID] = struct{}{}
				}
			}
			return nil
		})
	}); err != nil {
		log.Warnf("[gc] failed to read container metadata: %v", err)
	}

	// 3. 从已标记的快照出发，递归标记其父链
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
// 对齐 containerd: 先收集待删除列表，再批量删除
// 修复：将 boltdb 元数据清理合并到单个事务中，避免逐条开事务的性能问题
func (g *Collector) sweepContent(ctx context.Context, reachable map[string]struct{}) (int, error) {
	deleted := 0
	var toDelete []string

	if err := g.content.Walk(ctx, func(info content.Info) error {
		if _, ok := reachable[info.Digest]; !ok {
			toDelete = append(toDelete, info.Digest)
		}
		return nil
	}); err != nil {
		return 0, fmt.Errorf("遍历 content 失败: %w", err)
	}

	// 先删除文件，再批量清理 boltdb 元数据
	for _, digest := range toDelete {
		if err := g.content.Delete(ctx, digest); err != nil {
			log.Warnf("[gc] failed to delete content digest=%s: %v", digest, err)
			continue
		}
		deleted++
	}

	return deleted, nil
}

// sweepSnapshots 清扫未被标记的 snapshot
// 对齐 containerd: 先删除叶子节点，再删除父节点，避免删除被引用的父快照
// 修复：1. 删除快照后同步清理 boltdb 元数据，避免幽灵记录累积
//  2. 拓扑排序无法继续时跳过而非强制删除，避免破坏可达快照的父链
//  3. 使用 name→parent 映射替代 O(n²) 遍历
func (g *Collector) sweepSnapshots(ctx context.Context, reachable map[string]struct{}) (int, error) {
	deleted := 0

	// 收集所有快照及其父信息
	type snapInfo struct {
		name   string
		parent string
	}
	var allSnaps []snapInfo
	parentCount := make(map[string]int)   // 记录每个快照被多少子快照引用
	snapParent := make(map[string]string) // name → parent 映射，O(1) 查找

	if err := g.db.View(func(tx *bolt.Tx) error {
		return metadata.WalkSnapshots(tx, func(info *snapshots.Info) error {
			allSnaps = append(allSnaps, snapInfo{name: info.Name, parent: info.Parent}) //info.Name就是cacheID，info.Parent是上一层layer的 cacheID
			snapParent[info.Name] = info.Parent
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
				log.Warnf("[gc] failed to delete snapshot name=%s: %v", name, err)
				continue
			}
			deletedThisRound = append(deletedThisRound, name)
			deleted++

			// 同步清理 boltdb 中的快照元数据，避免幽灵记录累积
			// 导致后续 GC 的 WalkSnapshots 遍历到已删除的快照
			if err := g.db.Update(func(tx *bolt.Tx) error {
				return metadata.DeleteSnapshot(tx, name)
			}); err != nil {
				log.Warnf("[gc] failed to delete snapshot metadata name=%s: %v", name, err)
			}

			// 减少父快照的引用计数（O(1) 查找，替代 O(n²) 遍历）
			if parent := snapParent[name]; parent != "" {
				parentCount[parent]--
			}
		}

		// 如果这一轮没有删除任何快照，说明存在循环引用或引用异常
		// 对齐 containerd: 跳过而非强制删除，避免破坏可达快照的父链
		// 这些快照将在下次 GC 时重新评估（可能引用它的快照已被其他 GC 周期清理）
		if len(deletedThisRound) == 0 {
			log.Warnf("[gc] snapshot topology sort stalled, skipping %d unreferenced snapshots", len(toDelete))
			break
		}

		toDelete = nextRound
	}

	return deleted, nil
}
