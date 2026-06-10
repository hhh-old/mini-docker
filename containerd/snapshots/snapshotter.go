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
type Snapshotter interface {
	// Prepare 创建一个可写快照 (用于容器运行或镜像解包)
	// key: 快照唯一标识 (如 container-id 或 unpack-session-id)
	// parent: 父快照的 key (空则无父)
	Prepare(ctx context.Context, key, parent string) ([]Mount, error)

	// Commit 将可写快照提交为只读快照
	// 对齐 Docker: docker commit 的底层实现
	Commit(ctx context.Context, name, key string) error

	// UnpackLayer 解压 tar.gz blob 到快照目录并注册为 Committed 快照
	// 对齐 containerd: 镜像解压（Unpack）时，将"文件解压"和"元数据注册"合并为原子操作，
	// 避免分两步执行时崩溃导致"文件存在但元数据缺失"的不一致状态。
	// blobPath: tar.gz 文件的路径
	// digest: 该层的压缩 digest (sha256:...)，用于生成 cacheID
	// diffID: 该层的未压缩 digest (sha256:...)，用于校验解压后数据的完整性
	// parent: 父快照的 key（上一层的 cacheID，基础层为空）
	// 返回值: cacheID（层的快照标识），由调用方用于关联层 digest
	UnpackLayer(ctx context.Context, blobPath, digest, diffID, parent string) (string, error)

	// RegisterCommitted 注册一个已存在的目录为 Committed 快照
	// 对齐 containerd: 镜像解压（Unpack）时，层已解压到 snapshots/overlay/<key>/diff/，
	// 但 boltdb 中没有对应的 SnapshotInfo。本方法补注册 SnapshotInfo，建立 parent 链，
	// 使 Snapshotter 的 lowerDirs() 能递归构建多层 lowerdir。
	// 注意：新代码应优先使用 UnpackLayer，RegisterCommitted 仅用于兼容已有 diff/ 目录的补注册场景
	// key: 快照名称（通常为层的 cacheID）
	// parent: 父快照名称（上一层的 cacheID，基础层为空）
	RegisterCommitted(ctx context.Context, key, parent string) error

	// DiffPath 返回指定快照的 diff 目录路径
	// 用于外部需要直接访问层文件内容的场景（如镜像层解压、容器运行时构建 overlay lowerdir）
	// 对齐 containerd: 通过 Snapshotter 接口获取层路径，而非直接拼接常量路径
	// 这样 image 包不再需要知道底层是 overlay 实现，支持可插拔 Snapshotter
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
