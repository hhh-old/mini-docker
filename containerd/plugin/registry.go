package plugin

import (
	"fmt"
	"time"

	"mini-docker/constants"
	"mini-docker/containerd/containers"
	"mini-docker/containerd/content"
	"mini-docker/containerd/diff"
	"mini-docker/containerd/diff/walking"
	"mini-docker/containerd/events"
	"mini-docker/containerd/gc"
	"mini-docker/containerd/images"
	"mini-docker/containerd/metadata"
	"mini-docker/containerd/snapshots"
	"mini-docker/containerd/snapshots/native"
	"mini-docker/containerd/snapshots/overlay"
)

/*
=======================================================================
  内置插件注册 —— 对齐 containerd 的插件注册机制
=======================================================================

  真实 containerd 中，所有核心组件通过 init() 函数在程序启动时自动注册。
  mini-docker 简化为显式调用 RegisterBuiltinPlugins，在 containerd 启动前执行。

  插件依赖关系图:

  service.events ────┬──→ service.images
                     ├──→ service.containers
                     ├──→ service.task
                     └──→ service.gc

  metadata.DB ──────┬──→ content.filesys
                    ├──→ snapshotter.overlay
                    ├──→ snapshotter.native
                    ├──→ service.lease
                    └──── service.shim

  content.filesys ───→ service.content
  snapshotter.overlay ┬→ service.snapshotter
  snapshotter.native ─┘
  differ.walking ─────→ service.diff

  service.content + service.snapshotter + service.diff + service.lease
        └──→ service.images (还依赖 service.events)

  service.content + service.snapshotter
        └──→ service.gc (还依赖 service.events)

  service.shim ──────→ service.runtime
  service.containers + service.runtime + service.events ──→ service.task

  初始化顺序（拓扑排序结果）:
  1. service.events      (无依赖，事件总线)
  2. metadata            (无依赖，基础设施)
  3. content.filesys     (依赖 metadata)
  4. differ.walking      (无依赖，纯计算组件)
  5. service.lease       (依赖 metadata)
  6. snapshotter.overlay (依赖 metadata)
  7. snapshotter.native  (依赖 metadata)
  8. service.shim        (依赖 metadata)
  9. service.content     (依赖 content.filesys)
  10. service.snapshotter (依赖 snapshotter.overlay)
  11. service.diff        (依赖 content + snapshotter + differ.walking)
  12. service.runtime     (依赖 shim)
  13. service.gc          (依赖 metadata + service.content + service.snapshotter + events)
  14. service.images      (依赖 metadata + service.content + service.snapshotter + service.lease + service.diff + events)
  15. service.containers  (依赖 metadata + events)
  16. service.task        (依赖 containers + runtime + events)

=======================================================================
*/

// RegisterBuiltinPlugins 注册所有内置插件
// 在 containerd 启动时、Initialize 之前调用
func RegisterBuiltinPlugins(m *Manager) {
	// ---- 0. service.events 插件 ----
	// 事件总线服务，无外部依赖，最早初始化。
	// 所有插件（images/task/containers/gc）都可以通过 Plugin Manager 获取它并发布事件。
	m.Register(&Plugin{
		Type: TypeService,
		ID:   "events",
		Init: func(ic *InitContext) (interface{}, error) {
			return events.NewService(), nil
		},
		Depends: nil,
	})

	// ---- 1. metadata.DB 插件 ----
	// 基础设施插件，所有其他插件都依赖它
	// 调用 metadata.Open(path) 打开 boltdb
	m.Register(&Plugin{
		Type: TypeService,
		ID:   "metadata",
		Init: func(ic *InitContext) (interface{}, error) { //Init返回值是Plugin的instance实例
			// 从配置获取数据库路径，默认使用 constants 中的路径
			path := ic.Config["path"]
			if path == "" {
				path = constants.MiniDockerRoot + "/metadata/metadata.db"
			}
			db, err := metadata.Open(path)
			if err != nil {
				return nil, fmt.Errorf("打开 metadata 数据库失败: %w", err)
			}
			return db, nil
		},
		Depends: nil, // 无依赖，最底层
	})

	// ---- 2. content.filesys 插件 ----
	// 基于文件系统的 Content Store 实现
	// 调用 content.NewFilesystemStore(root, metaDB)
	m.Register(&Plugin{
		Type: TypeContent,
		ID:   "filesys",
		Init: func(ic *InitContext) (interface{}, error) {
			metaDB, err := GetPlugin[*metadata.DB](ic, TypeService, "metadata")
			if err != nil {
				return nil, fmt.Errorf("获取 metadata 插件失败: %w", err)
			}

			root := ic.Config["root"]
			if root == "" {
				root = constants.ContentStoreDir
			}
			store, err := content.NewFilesystemStore(root, metaDB)
			if err != nil {
				return nil, fmt.Errorf("创建 content store 失败: %w", err)
			}
			return store, nil
		},
		Depends: []PluginKey{
			{Type: TypeService, ID: "metadata"},
		},
	})

	// ---- 2.5. service.content 插件 ----
	// Content Store 的 Service 层封装，对外提供统一入口
	// 对齐 containerd: services/content 是 content store 的 gRPC 服务封装
	m.Register(&Plugin{
		Type: TypeService,
		ID:   "content",
		Init: func(ic *InitContext) (interface{}, error) {
			store, err := GetPlugin[content.Store](ic, TypeContent, "filesys")
			if err != nil {
				return nil, fmt.Errorf("获取 content store 插件失败: %w", err)
			}
			return content.NewService(store), nil
		},
		Depends: []PluginKey{
			{Type: TypeContent, ID: "filesys"},
		},
	})

	// ---- 3. snapshotter.overlay 插件 ----
	// OverlayFS Snapshotter 实现
	// 调用 overlay.NewSnapshotter(root, metaDB)
	m.Register(&Plugin{
		Type: TypeSnapshotter,
		ID:   "overlay",
		Init: func(ic *InitContext) (interface{}, error) {
			metaDB, err := GetPlugin[*metadata.DB](ic, TypeService, "metadata")
			if err != nil {
				return nil, fmt.Errorf("获取 metadata 插件失败: %w", err)
			}

			root := ic.Config["root"]
			if root == "" {
				root = constants.SnapshotterDir
			}
			snap, err := overlay.NewSnapshotter(root, metaDB)
			if err != nil {
				return nil, fmt.Errorf("创建 overlay snapshotter 失败: %w", err)
			}
			return snap, nil
		},
		Depends: []PluginKey{
			{Type: TypeService, ID: "metadata"},
		},
	})

	// ---- 4. differ.walking 插件 ----
	// Walking differ 实现，组合了 LayerApplier 和 LayerDiffer
	// 对齐 containerd: 类型定义在 diff/walking 包内，插件注册只引用
	// 无外部依赖，纯计算组件
	m.Register(&Plugin{
		Type: TypeDiffer,
		ID:   "walking",
		Init: func(ic *InitContext) (interface{}, error) {
			return walking.NewWalkingDiff(), nil
		},
		Depends: nil, // 无依赖
	})

	// ---- 5. service.lease 插件 ----
	// Lease 管理器，用于 GC 保护机制
	// 调用 gc.NewLeaseManager(metaDB)
	m.Register(&Plugin{
		Type: TypeService,
		ID:   "lease",
		Init: func(ic *InitContext) (interface{}, error) {
			metaDB, err := GetPlugin[*metadata.DB](ic, TypeService, "metadata")
			if err != nil {
				return nil, fmt.Errorf("获取 metadata 插件失败: %w", err)
			}
			return gc.NewLeaseManager(metaDB), nil
		},
		Depends: []PluginKey{
			{Type: TypeService, ID: "metadata"},
		},
	})

	// ---- 6. service.gc 插件 ----
	// GC 收集器，周期性执行垃圾回收
	// 调用 gc.NewCollector(metaDB, contentStore, snap, interval)
	// 对齐 containerd: GC 使用的 snapshotter 由全局配置 default_snapshotter 决定
	m.Register(&Plugin{
		Type: TypeService,
		ID:   "gc",
		Init: func(ic *InitContext) (interface{}, error) {
			metaDB, err := GetPlugin[*metadata.DB](ic, TypeService, "metadata")
			if err != nil {
				return nil, fmt.Errorf("获取 metadata 插件失败: %w", err)
			}

			contentSvc, err := GetPlugin[*content.Service](ic, TypeService, "content")
			if err != nil {
				return nil, fmt.Errorf("获取 content service 插件失败: %w", err)
			}

			snapSvc, err := GetPlugin[*snapshots.Service](ic, TypeService, "snapshotter")
			if err != nil {
				return nil, fmt.Errorf("获取 snapshotter service 插件失败: %w", err)
			}

			// GC 间隔默认 5 分钟
			interval := 5 * time.Minute
			if v := ic.Config["interval"]; v != "" {
				if d, err := time.ParseDuration(v); err == nil {
					interval = d
				}
			}

			evSvc, err := GetPlugin[*events.Service](ic, TypeService, "events")
			if err != nil {
				return nil, fmt.Errorf("获取 events 插件失败: %w", err)
			}

			// 对齐 Content/Snapshot Service 化：GC 依赖 Service 层，由 Service 根据配置选择底层实现
			collector := gc.NewCollector(metaDB, contentSvc.Store(), snapSvc.Snapshotter(), interval, evSvc)
			collector.Start()
			return collector, nil
		},
		Depends: []PluginKey{
			{Type: TypeService, ID: "metadata"},
			{Type: TypeService, ID: "content"},
			{Type: TypeService, ID: "snapshotter"},
			{Type: TypeService, ID: "events"},
		},
	})

	// ---- 7. service.images 插件 ----
	// 镜像管理服务，统一依赖 Content Service、Snapshotter Service、Diff Service
	// 对齐 containerd: Images 使用的 snapshotter 由全局配置 default_snapshotter 决定
	m.Register(&Plugin{
		Type: TypeService,
		ID:   "images",
		Init: func(ic *InitContext) (interface{}, error) {
			metaDB, err := GetPlugin[*metadata.DB](ic, TypeService, "metadata")
			if err != nil {
				return nil, fmt.Errorf("获取 metadata 插件失败: %w", err)
			}

			contentSvc, err := GetPlugin[*content.Service](ic, TypeService, "content")
			if err != nil {
				return nil, fmt.Errorf("获取 content service 插件失败: %w", err)
			}

			snapSvc, err := GetPlugin[*snapshots.Service](ic, TypeService, "snapshotter")
			if err != nil {
				return nil, fmt.Errorf("获取 snapshotter service 插件失败: %w", err)
			}

			diffSvc, err := GetPlugin[*diff.Service](ic, TypeService, "diff")
			if err != nil {
				return nil, fmt.Errorf("获取 diff service 插件失败: %w", err)
			}

			leaseMgr, err := GetPlugin[*gc.LeaseManager](ic, TypeService, "lease")
			if err != nil {
				return nil, fmt.Errorf("获取 lease 插件失败: %w", err)
			}

			evSvc, err := GetPlugin[*events.Service](ic, TypeService, "events")
			if err != nil {
				return nil, fmt.Errorf("获取 events 插件失败: %w", err)
			}

			return images.NewService(metaDB, contentSvc, snapSvc, diffSvc, leaseMgr, evSvc), nil
		},
		Depends: []PluginKey{
			{Type: TypeService, ID: "metadata"},
			{Type: TypeService, ID: "content"},
			{Type: TypeService, ID: "snapshotter"},
			{Type: TypeService, ID: "diff"},
			{Type: TypeService, ID: "lease"},
			{Type: TypeService, ID: "events"},
		},
	})

	// ---- 8. service.containers 插件 ----
	// 容器管理服务：提供容器元数据 CRUD
	// 对齐 containerd: containers.Service 独立包，registry 只负责注册
	m.Register(&Plugin{
		Type: TypeService,
		ID:   "containers",
		Init: func(ic *InitContext) (interface{}, error) {
			metaDB, err := GetPlugin[*metadata.DB](ic, TypeService, "metadata")
			if err != nil {
				return nil, fmt.Errorf("获取 metadata 插件失败: %w", err)
			}

			evSvc, err := GetPlugin[*events.Service](ic, TypeService, "events")
			if err != nil {
				return nil, fmt.Errorf("获取 events 插件失败: %w", err)
			}

			return containers.NewService(metaDB, evSvc), nil
		},
		Depends: []PluginKey{
			{Type: TypeService, ID: "metadata"},
			{Type: TypeService, ID: "events"},
		},
	})

	// ---- 9. snapshotter.native 插件 ----
	// Native Snapshotter 实现，使用简单目录拷贝替代 OverlayFS
	// 对齐 containerd: native snapshotter 是最简单的快照实现，适合不支持 OverlayFS 的环境
	m.Register(&Plugin{
		Type: TypeSnapshotter,
		ID:   "native",
		Init: func(ic *InitContext) (interface{}, error) {
			metaDB, err := GetPlugin[*metadata.DB](ic, TypeService, "metadata")
			if err != nil {
				return nil, fmt.Errorf("获取 metadata 插件失败: %w", err)
			}

			root := ic.Config["root"]
			if root == "" {
				root = constants.SnapshotterDir + "-native"
			}
			snap, err := native.NewSnapshotter(root, metaDB)
			if err != nil {
				return nil, fmt.Errorf("创建 native snapshotter 失败: %w", err)
			}
			return snap, nil
		},
		Depends: []PluginKey{
			{Type: TypeService, ID: "metadata"},
		},
	})

	// ---- 9.5. service.snapshotter 插件 ----
	// Snapshotter 的 Service 层封装，对外提供统一入口
	// 对齐 containerd: services/snapshots 是 snapshotter 的 gRPC 服务封装
	m.Register(&Plugin{
		Type: TypeService,
		ID:   "snapshotter",
		Init: func(ic *InitContext) (interface{}, error) {
			snapID := "overlay"
			if ic.Global != nil && ic.Global.DefaultSnapshotter != "" {
				snapID = ic.Global.DefaultSnapshotter
			}
			snap, err := GetPlugin[snapshots.Snapshotter](ic, TypeSnapshotter, snapID)
			if err != nil {
				return nil, fmt.Errorf("获取 snapshotter 插件 %s 失败: %w", snapID, err)
			}
			return snapshots.NewService(snap), nil
		},
		Depends: []PluginKey{
			{Type: TypeSnapshotter, ID: "overlay"},
			{Type: TypeSnapshotter, ID: "native"},
		},
	})

	// ---- 9.8. service.diff 插件 ----
	// Diff 的 Service 层封装，协调 Content Store、Snapshotter Service、Differ/Applier
	// 对齐 containerd: services/diff 是 diff 的 gRPC 服务封装
	// 依赖 service.snapshotter 而非直接依赖具体 snapshotter 实现，由 Snapshotter Service 根据配置选择默认实现
	m.Register(&Plugin{
		Type: TypeService,
		ID:   "diff",
		Init: func(ic *InitContext) (interface{}, error) {
			contentStore, err := GetPlugin[content.Store](ic, TypeContent, "filesys")
			if err != nil {
				return nil, fmt.Errorf("获取 content store 插件失败: %w", err)
			}

			snapSvc, err := GetPlugin[*snapshots.Service](ic, TypeService, "snapshotter")
			if err != nil {
				return nil, fmt.Errorf("获取 snapshotter service 插件失败: %w", err)
			}

			wd, err := GetPlugin[*walking.WalkingDiff](ic, TypeDiffer, "walking")
			if err != nil {
				return nil, fmt.Errorf("获取 differ 插件失败: %w", err)
			}

			return diff.NewService(contentStore, snapSvc.Snapshotter(), wd.Applier, wd.Differ), nil
		},
		Depends: []PluginKey{
			{Type: TypeContent, ID: "filesys"},
			{Type: TypeService, ID: "snapshotter"},
			{Type: TypeDiffer, ID: "walking"},
		},
	})

	// ---- Linux 特有插件由 registerLinuxPlugins 注册 ----
	registerLinuxPlugins(m)
}
