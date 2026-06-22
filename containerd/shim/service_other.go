//go:build !linux

package shim

/*
=======================================================================
  Shim Manager 服务（非 Linux 平台桩实现）

  对应 service_linux.go 中 ShimManager 结构体及相关方法。
  在非 Linux 平台上提供桩实现，让 plugin/registry.go 能跨平台编译。

=======================================================================
*/

import (
	"fmt"
	"net"
	"os/exec"
	"time"

	"mini-docker/containerd/metadata"
)

// ExitInfo 退出信息类型别名
type ExitInfo = struct {
	ExitCode  int    `json:"exit_code"`
	ExitedAt  string `json:"exited_at"`
	OOMKilled bool   `json:"oom_killed"`
}

// ShimManager 管理 shim 进程的生命周期（非 Linux 平台桩实现）
type ShimManager struct{}

// NewShimManager 创建 ShimManager 实例
func NewShimManager(metaDB interface{}) *ShimManager {
	return &ShimManager{}
}

func (m *ShimManager) Connect(containerID string) (net.Conn, error) {
	return nil, errNotLinux
}

func (m *ShimManager) Call(containerID string, req interface{}) error {
	return errNotLinux
}

func (m *ShimManager) CallWithData(containerID string, req interface{}, result interface{}) error {
	return errNotLinux
}

func (m *ShimManager) ReadExitInfoFromFile(containerID string) (*ExitInfo, error) {
	return nil, errNotLinux
}

func (m *ShimManager) WaitForSocket(path string, timeout time.Duration) error {
	return errNotLinux
}

func (m *ShimManager) BuildOCISpec(info interface{}, cgroupName string) interface{} {
	return nil
}

func (m *ShimManager) IsAlive(containerID string) bool {
	return false
}

func (m *ShimManager) ReadPID(containerID string) int {
	return 0
}

func (m *ShimManager) Delete(containerID string) error {
	return errNotLinux
}

func (m *ShimManager) Shutdown(containerID string) {}

func (m *ShimManager) Restart(containerID string, containerPID int) (int, error) {
	return 0, errNotLinux
}

func (m *ShimManager) WaitForCreate(containerID string, timeout time.Duration) (int, error) {
	return 0, errNotLinux
}

func (m *ShimManager) Attach(containerID string) (net.Conn, error) {
	return nil, errNotLinux
}

func (m *ShimManager) ExecStream(containerID string, args []string, tty bool) (net.Conn, error) {
	return nil, errNotLinux
}

func (m *ShimManager) NewCommand(args []string) *exec.Cmd {
	return exec.Command("/proc/self/exe", args...)
}

func (m *ShimManager) GetState(containerID string) (*metadata.TaskState, error) {
	return nil, errNotLinux
}

func (m *ShimManager) ListStates() ([]*metadata.TaskState, error) {
	return nil, errNotLinux
}

var errNotLinux = error(fmt.Errorf("仅支持 Linux 平台"))
