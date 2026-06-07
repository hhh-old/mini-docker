//go:build !linux

package containerd

import (
	"fmt"
	"os/exec"
)

/*
=======================================================================
  shim 进程管理（非 Linux 平台桩实现）
=======================================================================

  对应 shim_manager_linux.go 中 shim 进程管理相关函数。
  在非 Linux 平台上提供桩实现，让 daemon 包能跨平台编译。
  真实 shim 进程依赖 cgroup/namespace/seccomp 等 Linux 内核特性，
  因此这些函数在非 Linux 上都返回空值/错误。
=======================================================================
*/

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

// newShimCommand 创建 shim 子进程命令
// Linux 平台实现见 shim_manager_linux.go（含 Setsid 等平台特定配置）
func newShimCommand(args []string) *exec.Cmd {
	return exec.Command("/proc/self/exe", args...)
}
