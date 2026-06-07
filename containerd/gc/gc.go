package gc

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	bolt "go.etcd.io/bbolt"

	"mini-docker/containerd/metadata"
)

type Collector struct {
	db          *metadata.DB
	content     ContentDeleter
	snapshotter SnapshotDeleter
	interval    time.Duration
	stopCh      chan struct{}
	wg          sync.WaitGroup
}

type ContentDeleter interface {
	Delete(ctx context.Context, digest string) error
	Walk(ctx context.Context, fn func(digest string, size int64) error) error
}

type SnapshotDeleter interface {
	Remove(ctx context.Context, key string) error
	Walk(ctx context.Context, fn func(name string) error) error
}

type GCStats struct {
	ContentDeleted int
	SnapDeleted    int
	Elapsed        time.Duration
}

func NewCollector(db *metadata.DB, content ContentDeleter, snapshotter SnapshotDeleter, interval time.Duration) *Collector {
	return &Collector{
		db:          db,
		content:     content,
		snapshotter: snapshotter,
		interval:    interval,
		stopCh:      make(chan struct{}),
	}
}

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

func (g *Collector) Stop() {
	close(g.stopCh)
	g.wg.Wait()
}

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

func (g *Collector) mark(ctx context.Context) (map[string]struct{}, map[string]struct{}, error) {
	reachableDigests := make(map[string]struct{})
	reachableSnapKeys := make(map[string]struct{})

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
			reachableDigests[imageID] = struct{}{}
			for _, layerDigest := range img.Layers {
				reachableDigests[layerDigest] = struct{}{}
			}
		}

		return nil
	}); err != nil {
		return nil, nil, err
	}

	if err := g.db.View(func(tx *bolt.Tx) error {
		return metadata.WalkLeases(tx, func(info *metadata.LeaseInfo) error {
			for _, obj := range info.Objects {
				reachableDigests[obj] = struct{}{}
			}
			return nil
		})
	}); err != nil {
		return nil, nil, err
	}

	if err := g.db.View(func(tx *bolt.Tx) error {
		return metadata.WalkSnapshots(tx, func(info *metadata.SnapshotInfo) error {
			reachableSnapKeys[info.Name] = struct{}{}
			if info.Kind == metadata.KindActive {
				if info.Parent != "" {
					reachableSnapKeys[info.Parent] = struct{}{}
				}
			}
			return nil
		})
	}); err != nil {
		return nil, nil, err
	}

	return reachableDigests, reachableSnapKeys, nil
}

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
		deleted++
	}

	return deleted, nil
}

func (g *Collector) sweepSnapshots(ctx context.Context, reachable map[string]struct{}) (int, error) {
	deleted := 0
	var toDelete []string

	if err := g.snapshotter.Walk(ctx, func(name string) error {
		if _, ok := reachable[name]; !ok {
			toDelete = append(toDelete, name)
		}
		return nil
	}); err != nil {
		return 0, fmt.Errorf("遍历 snapshot 失败: %w", err)
	}

	for _, name := range toDelete {
		if err := g.snapshotter.Remove(ctx, name); err != nil {
			log.Printf("[gc] 删除 snapshot %s 失败: %v\n", name, err)
			continue
		}
		deleted++
	}

	return deleted, nil
}
