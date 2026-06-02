//go:build !linux

package containerd

import (
	"fmt"
	"os/exec"
	"time"

	"mini-docker/types"
)

// ExitInfo 退出信息类型别名
type ExitInfo = types.ExitInfo

// Containerd containerd 独立进程（非 Linux 平台桩实现）
type Containerd struct{}

// NewContainerd 创建 containerd 实例
func NewContainerd() *Containerd {
	return &Containerd{}
}

// Start 启动 containerd（非 Linux 平台不支持）
func (c *Containerd) Start() error {
	return fmt.Errorf("containerd 独立进程仅支持 Linux 平台")
}

// Stop 停止 containerd
func (c *Containerd) Stop() {}

// IsContainerdRunning 检查 containerd 是否在运行
func IsContainerdRunning() bool {
	return false
}

// WaitForContainerd 等待 containerd 就绪
func WaitForContainerd(timeout time.Duration) error {
	return fmt.Errorf("containerd 独立进程仅支持 Linux 平台")
}

// IsShimAlive 检查 shim 是否存活
func IsShimAlive(containerID string) bool {
	return false
}

// ReadShimPID 读取 shim PID
func ReadShimPID(containerID string) int {
	return 0
}

// ReadExitInfo 读取退出信息
func ReadExitInfo(containerID string) (*ExitInfo, error) {
	return nil, fmt.Errorf("仅支持 Linux")
}

func newShimCommand(args []string) *exec.Cmd {
	return exec.Command("/proc/self/exe", args...)
}
