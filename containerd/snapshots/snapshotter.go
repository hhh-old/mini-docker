package snapshots

import "context"

// Mount 挂载信息（对齐 containerd 的 mount.Mount）
type Mount struct {
	Type    string   // "overlay", "bind" 等
	Source  string   // 挂载源
	Options []string // 挂载选项 (如 lowerdir=...,upperdir=...,workdir=...)
}

// Kind 快照类型
type Kind int

const (
	KindActive   Kind = iota // 可写快照 (容器运行中)
	KindCommitted            // 只读快照 (镜像层)
)

// Info 快照元信息
type Info struct {
	Name      string            `json:"name"`
	Parent    string            `json:"parent,omitempty"`
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

	// View 创建一个只读视图 (用于 inspect)
	View(ctx context.Context, key, parent string) ([]Mount, error)

	// Commit 将可写快照提交为只读快照
	// 对齐 Docker: docker commit 的底层实现
	Commit(ctx context.Context, name, key string) error

	// Mounts 获取快照的挂载信息
	Mounts(ctx context.Context, key string) ([]Mount, error)

	// Remove 删除快照
	Remove(ctx context.Context, key string) error

	// Walk 遍历所有快照
	Walk(ctx context.Context, fn func(Info) error) error

	// Close 关闭 Snapshotter
	Close() error
}
