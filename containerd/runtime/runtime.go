package runtime

import (
	"context"
	"net"
	"time"

	"mini-docker/containerd/metadata"
	"mini-docker/containerd/shim"
	"mini-docker/containerstore"
	"mini-docker/spec"
)

// RuntimeType OCI Runtime 类型标识
type RuntimeType string

const (
	// TypeRunc 标准 runc runtime（默认）
	TypeRunc RuntimeType = "runc"
)

// Runtime 定义容器运行时的抽象接口。
// 每一种 RuntimeType 对应一个实现（如 runc/kata/gvisor），
// 由 runtime.Service（Manager）按容器配置的 Runtime 字段分发调用。
type Runtime interface {
	// Type 返回该 Runtime 实现对应的 runtime 类型
	Type() RuntimeType

	// BuildOCISpec 根据容器信息生成 OCI Runtime Spec
	BuildOCISpec(info *containerstore.ContainerInfo, cgroupName string) *spec.Spec

	// CreateTask 创建并启动任务，返回 shim PID
	CreateTask(ctx context.Context, info *containerstore.ContainerInfo, cgroupName string) (shimPID int, err error)

	// Start 启动容器内进程
	Start(ctx context.Context, containerID string) error

	// Kill 向容器发送信号
	Kill(ctx context.Context, containerID string, signal int) error

	// Delete 删除容器任务
	Delete(ctx context.Context, containerID string) error

	// Pause / Resume 暂停/恢复容器
	Pause(ctx context.Context, containerID string) error
	Resume(ctx context.Context, containerID string) error

	// GetState 从 shim/runtime 获取实时状态（不缓存）
	GetState(ctx context.Context, containerID string) (*metadata.TaskState, error)

	// List 列出该 runtime 管理的所有任务
	List(ctx context.Context) ([]*metadata.TaskState, error)

	// GetExitInfo 获取容器退出信息
	GetExitInfo(ctx context.Context, containerID string) (*shim.ExitInfo, error)

	// ShutdownShim 仅关闭 shim（用于自然退出的容器）
	ShutdownShim(ctx context.Context, containerID string)

	// RestartShim 重启 shim 以接管已有容器
	RestartShim(ctx context.Context, containerID string, containerPID int) (int, error)

	// WaitForCreate 等待容器创建完成
	WaitForCreate(ctx context.Context, containerID string, timeout time.Duration) (int, error)

	// Attach 连接到容器 TTY
	Attach(ctx context.Context, containerID string) (net.Conn, error)

	// ExecStream 在容器内执行命令，返回与 shim 通信的连接
	ExecStream(ctx context.Context, containerID string, args []string, tty bool) (net.Conn, error)

	// Resize 调整容器终端大小
	Resize(ctx context.Context, containerID string, rows, cols uint16) error

	// IsShimAlive 检查 shim 进程是否存活
	IsShimAlive(ctx context.Context, containerID string) bool

	// ReadShimPID 读取 shim PID
	ReadShimPID(ctx context.Context, containerID string) int

	// ReadExitInfo 从磁盘读取退出信息
	ReadExitInfo(ctx context.Context, containerID string) (*shim.ExitInfo, error)
}
