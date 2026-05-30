// Package libcontainer 提供容器运行时核心实现，对标 runc/libcontainer
//
// libcontainer 是 Docker 容器运行时的核心库，负责：
// - 容器生命周期管理（创建、启动、停止、删除）
// - Namespace 隔离（PID、Network、Mount、IPC、UTS）
// - Cgroup 资源限制（CPU、内存、PID 数等）
// - Rootfs 设置（OverlayFS、pivot_root、挂载）
// - Capability 安全控制
// - Seccomp 系统调用过滤
package libcontainer

import (
	"fmt"
	"os"

	"mini-docker/libcontainer/cgroups"
	"mini-docker/libcontainer/configs"
)

// Status 容器状态
type Status = string

const (
	StatusCreated  Status = "created"
	StatusCreating Status = "creating"
	StatusRunning  Status = "running"
	StatusPaused   Status = "paused"
	StatusStopped  Status = "stopped"
)

// containerRunState 容器运行时状态（纯内存态，对标 runc 的 containerState 接口）
// 仅包含 ID、Pid、Status 等运行时可变信息，不含配置字段
// 与 ContainerState 的区别：ContainerState 用于序列化到 state.json（包含 Bundle/Rootfs 等配置快照）
// containerRunState 仅用于内存中的快速读写，序列化时通过 toContainerState() 组装完整状态
type containerRunState struct {
	ID         string
	Pid        int
	Status     Status
	OCIVersion string
}

// Container 容器接口（对标 libcontainer/container.go）
type Container interface {
	// ID 返回容器 ID
	ID() string

	// Status 返回容器状态
	Status() (Status, error)

	// Config 返回容器配置
	Config() configs.Config

	// Start 启动容器进程（fork init 进程，设置 namespace、rootfs、cgroup）
	Start(process *Process) error

	// ExecStart 启动已创建的容器（对标 runc start：发送 start 信号，从 created → running）
	ExecStart() error

	// Run 创建并启动容器（Start + ExecStart 的便捷封装）
	Run(process *Process) error

	// Destroy 销毁容器，清理所有资源
	Destroy() error

	// Pause 暂停容器（cgroup freezer）
	Pause() error

	// Resume 恢复暂停的容器
	Resume() error

	// Signal 向容器主进程发送信号
	Signal(sig int) error

	// Exec 在容器内执行新进程
	Exec(process *Process) error

	// AddProcessToCgroup 将指定 PID 加入容器的 cgroup
	// 对标 Docker/containerd: exec 进程需要加入容器的 cgroup 以受资源限制
	AddProcessToCgroup(pid int) error

	// Stats 获取容器统计信息
	Stats() (*Stats, error)

	// Pid 返回容器主进程 PID
	Pid() int

	// State 返回完整的容器状态（对标 OCI runtime-spec State）
	State() *ContainerState

	// Set 设置容器资源限制
	Set(config configs.Resources) error

	// SetStatus 更新容器运行时状态并持久化
	SetStatus(status Status) error
}

// Process 启动进程的配置
type Process struct {
	// Args 进程参数（第一个为可执行文件）
	Args []string

	// Env 环境变量
	Env []string

	// Cwd 工作目录
	Cwd string

	// User 用户信息 (UID:GID)
	User string

	// Stdin 标准输入
	Stdin *os.File

	// Stdout 标准输出
	Stdout *os.File

	// Stderr 标准错误
	Stderr *os.File

	// Terminal 是否分配终端
	Terminal bool

	// ConsoleFile 终端设备文件（pty slave），由 runtime 传入
	// 对齐 runc 的 --console-socket 机制：runtime 打开 pty slave 并传给 init 进程
	ConsoleFile *os.File

	// ExtraFiles 额外文件描述符
	ExtraFiles []*os.File
}

// Stats 容器统计信息
type Stats struct {
	CgroupStats  *cgroups.Stats
	NetworkStats map[string]NetworkStats
}

// NetworkStats 网络统计
type NetworkStats struct {
	RxBytes   uint64
	TxBytes   uint64
	RxPackets uint64
	TxPackets uint64
	RxErrors  uint64
	TxErrors  uint64
	RxDropped uint64
	TxDropped uint64
}

// New 创建新的容器实例
func New(id string, config *configs.Config) (Container, error) {
	return newLinuxContainer(id, config)
}

// Load 加载已有容器
func Load(id string) (Container, error) {
	return loadLinuxContainer(id)
}

// Validate 验证容器配置
func Validate(config *configs.Config) error {
	if config.Rootfs == "" {
		return fmt.Errorf("rootfs 路径不能为空")
	}

	// 验证 namespace 类型
	validNsTypes := map[string]bool{
		"pid": true, "network": true, "mount": true,
		"ipc": true, "uts": true, "user": true, "cgroup": true,
	}
	for _, ns := range config.Namespaces {
		if !validNsTypes[ns.Type] {
			return fmt.Errorf("无效的 namespace 类型: %s", ns.Type)
		}
	}

	return nil
}
