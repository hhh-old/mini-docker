package snapshots

import "context"

// Mount 挂载信息（对齐 containerd 的 mount.Mount）
type Mount struct {
	Type    string   `json:"type"`    // "overlay", "bind" 等
	Source  string   `json:"source"`  // 挂载源
	Options []string `json:"options"` // 挂载选项 (如 lowerdir=...,upperdir=...,workdir=...)
}

// Kind 快照类型，go中定义枚举类型的方式
type Kind int

const (
	KindActive    Kind = iota // 可写快照 (容器运行中)
	KindCommitted             // 只读快照 (镜像层)
)

// Info 快照元信息
type Info struct {
	Name      string            `json:"name"`             //这个其实就是 cacheID
	Parent    string            `json:"parent,omitempty"` //上一层layer的 cacheID
	Kind      Kind              `json:"kind"`
	ReadWrite bool              `json:"read_write"`
	CreatedAt string            `json:"created_at"`
	UpdatedAt string            `json:"updated_at"`
	Labels    map[string]string `json:"labels,omitempty"`
}

// Snapshotter 可插拔快照接口
// 对齐 containerd: snapshots.Snapshotter
// 负责管理镜像层和容器可写层的文件系统快照
//
// 核心生命周期（对齐 containerd 的 Prepare-Apply-Commit 循环）：
//  1. Prepare(key, parent) → 创建 Active 快照，返回 mount 信息
//  2. Apply(digest, diffID, blobPath, key) → 将层差异应用到 Active 快照
//  3. Commit(key) → 将 Active 快照原地提交为 Committed 快照
//
// 镜像拉取时，每层执行一次 Prepare-Apply-Commit：
//
//	for each layer:
//	  Prepare(cacheID, parentCacheID)  → 创建可写快照
//	  Apply(digest, diffID, blob, cacheID)  → 解压 tar + 处理 whiteout
//	  Commit(cacheID)  → 原地提交为只读快照
//
// 容器运行时，只需 Prepare：
//
//	Prepare(containerID, topLayerSnapshotID)  → 创建容器可写层
type Snapshotter interface {
	// Prepare 创建一个可写快照 (用于容器运行或镜像解包)
	// key: 快照唯一标识 (如 container-id 或 unpack-session-id)
	// parent: 父快照的 key (空则无父)
	Prepare(ctx context.Context, key, parent string) ([]Mount, error)

	// Apply 将层差异应用到 Active 快照
	// 对齐 containerd: diff.Applier.Apply —— 将 blob 解压到 active snapshot 的挂载点
	// digest: 该层的压缩 digest (sha256:...)，用于生成 cacheID
	// diffID: 该层的未压缩 digest (sha256:...)，用于校验解压后数据的完整性
	// blobPath: tar.gz 文件的路径
	// key: Active 快照的 key（由 Prepare 创建）
	Apply(ctx context.Context, digest, diffID, blobPath, key string) error

	// Commit 原地提交：将 Active 快照转为 Committed 快照
	// 合并 upper/ → diff/（如有），删除 upper/ + work/，更新元数据
	// key: Active 快照的名称，提交后该快照变为 Committed
	// 用于 Pull 流程和 Build 流程
	Commit(ctx context.Context, key string) ([]Mount, error)

	// CommitAs 创建新快照：从 Active 快照创建新的 Committed 快照
	// 源 Active 快照保持不变，由调用方决定是否 Remove
	// 用于容器 commit（docker commit 等场景）
	// name: 新 Committed 快照的名称
	// key: 源 Active 快照的名称
	CommitAs(ctx context.Context, name, key string) ([]Mount, error)

	// DiffPath 返回指定快照的 diff 目录路径
	// 用于外部需要直接访问层文件内容的场景（如镜像层解压、容器运行时构建 overlay lowerdir）
	DiffPath(ctx context.Context, key string) (string, error)

	// Mounts 获取快照的挂载信息
	Mounts(ctx context.Context, key string) ([]Mount, error)

	// Remove 删除快照
	Remove(ctx context.Context, key string) error

	// Walk 遍历所有快照
	Walk(ctx context.Context, fn func(Info) error) error

	// Close 关闭 Snapshotter
	Close() error
}
