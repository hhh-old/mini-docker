package snapshots

import "context"

// Mount 挂载信息（对齐 containerd 的 mount.Mount）
type Mount struct {
	Type    string   `json:"type"`    // "overlay", "bind" 等
	Source  string   `json:"source"`  // 挂载源
	Options []string `json:"options"` // 挂载选项 (如 lowerdir=...,upperdir=...,workdir=...)
}

// Kind 快照类型
type Kind int

const (
	KindActive    Kind = iota // 可写快照 (容器运行中)
	KindCommitted             // 只读快照 (镜像层)
	KindView                  // 只读活跃快照 (用于挂载查看，无 upperdir/workdir)
)

// Info 快照元信息
type Info struct {
	ID        string            `json:"id"`                   // 内部数字 ID，作为目录名使用（如 "1", "2", "3"）
	Name      string            `json:"name"`                 // 快照名称（即 key，如 cacheID）
	Parent    string            `json:"parent,omitempty"`     // 父快照的名称
	ParentIDs []string          `json:"parent_ids,omitempty"` // 预计算的父链 ID 列表，用于高效构建 lowerdir
	Kind      Kind              `json:"kind"`
	CreatedAt string            `json:"created_at"`
	UpdatedAt string            `json:"updated_at"`
	Labels    map[string]string `json:"labels,omitempty"`
}

// Usage 快照资源使用量
type Usage struct {
	Inodes int64 `json:"inodes"` // inode 数量
	Size   int64 `json:"size"`   // 字节大小
}

// Add 累加另一个 Usage
func (u *Usage) Add(other Usage) {
	u.Inodes += other.Inodes
	u.Size += other.Size
}

// Opt 快照选项函数
type Opt func(*Info) error

// WithLabels 返回设置标签的选项
func WithLabels(labels map[string]string) Opt {
	return func(info *Info) error {
		info.Labels = labels
		return nil
	}
}

// WalkFunc 遍历快照时的回调函数类型
type WalkFunc func(ctx context.Context, info Info) error

// Snapshotter 可插拔快照接口
// 对齐 containerd: snapshots.Snapshotter
// 负责管理镜像层和容器可写层的文件系统快照
//
// 核心生命周期（对齐 containerd 的 Prepare-Apply-Commit 循环）：
//  1. Prepare(key, parent) → 创建 Active 可写快照，返回 mount 信息
//  2. 外部 Applier 将层差异应用到 Active 快照的挂载点
//  3. Commit(name, key) → 将 Active 快照提交为 Committed 快照（元数据操作，目录不变）
//
// 镜像拉取时，每层执行一次 Prepare-Apply-Commit：
//
//	for each layer:
//	  Prepare(cacheID, parentCacheID)  → 创建可写快照
//	  外部 Applier 应用层差异          → 解压 tar + 处理 whiteout
//	  Commit(cacheID, cacheID)         → 提交为只读快照
//
// 容器运行时：
//
//	Prepare(containerID, topLayerSnapshotID)  → 创建容器可写层
//
// 只读查看镜像内容：
//
//	View(key, parent)  → 创建只读活跃快照，无 upperdir/workdir
type Snapshotter interface {
	// Prepare 创建一个可写快照 (KindActive)，用于容器运行或镜像解包
	// key: 快照唯一标识 (如 container-id 或 unpack-session-id)
	// parent: 父快照的 key (空则无父)
	// opts: 可选参数（如 WithLabels）
	Prepare(ctx context.Context, key, parent string, opts ...Opt) ([]Mount, error)

	// View 创建一个只读活跃快照 (KindView)，无 upperdir/workdir
	// 用于挂载查看镜像内容等只读场景
	// key: 快照唯一标识
	// parent: 父快照的 key (空则无父)
	// opts: 可选参数（如 WithLabels）
	View(ctx context.Context, key, parent string, opts ...Opt) ([]Mount, error)

	// Commit 将 Active 快照提交为 Committed 快照
	// 这是元数据操作：目录不变，只是 key 映射改变，源 Active 快照的 key 不再可用
	// name: 新 Committed 快照的名称
	// key: 源 Active 快照的名称（提交后该 key 被消费，不再可用）
	Commit(ctx context.Context, name, key string, opts ...Opt) error

	// Mounts 获取快照的挂载信息
	Mounts(ctx context.Context, key string) ([]Mount, error)

	// Remove 删除快照
	Remove(ctx context.Context, key string) error

	// Stat 返回指定快照的元信息
	Stat(ctx context.Context, key string) (Info, error)

	// Update 更新快照的元信息（如 labels 等）
	// fieldpaths: 指定要更新的字段路径，为空则更新所有可变字段
	Update(ctx context.Context, info Info, fieldpaths ...string) (Info, error)

	// Usage 返回快照的资源使用量
	Usage(ctx context.Context, key string) (Usage, error)

	// Walk 遍历所有快照
	// fn: 对每个快照调用的回调函数
	// filters: 过滤条件（如 "labels.key==value"）
	Walk(ctx context.Context, fn WalkFunc, filters ...string) error

	// Cleanup 清理已移除/废弃快照的磁盘资源
	Cleanup(ctx context.Context) error

	// Close 关闭 Snapshotter
	Close() error
}
