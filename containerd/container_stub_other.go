//go:build !linux

package containerd

import (
	"fmt"
	"time"
)

/*
=======================================================================
  containerd 进程主体（非 Linux 平台桩实现）
=======================================================================

  对应 server_linux.go 中 Containerd 结构体及相关方法。
  在非 Linux 平台上提供桩实现，让其他包（如 daemon、main）能跨平台编译。
  真实 containerd 是 Linux-only 项目（依赖 cgroup/namespace/overlayfs），
  这里对齐这一现实：非 Linux 平台上 NewContainerd/Start 都返回错误。

  拆分说明：本文件只关心 Containerd 进程级别的桩实现。
  shim 相关的桩实现（IsShimAlive/ReadShimPID/ReadExitInfo/newShimCommand）
  分离到 shim_stub_other.go，避免 kitchen-sink 反模式。
=======================================================================
*/

// Containerd containerd 独立进程（非 Linux 平台桩实现）
type Containerd struct{}

// NewContainerd 创建 containerd 实例
func NewContainerd() (*Containerd, error) {
	return &Containerd{}, nil
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
