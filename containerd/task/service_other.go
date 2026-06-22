//go:build !linux

package task

/*
=======================================================================
  Task Service（非 Linux 平台桩实现）

  对应 service_linux.go 中 Service 结构体及相关方法。
  在非 Linux 平台上提供桩实现，让 plugin/registry.go 能跨平台编译。

=======================================================================
*/

import (
	"fmt"
	"net"
	"time"

	"mini-docker/containerd/metadata"
)

// Service Task 生命周期管理服务（非 Linux 平台桩实现）
type Service struct{}

// NewService 创建 Task Service
func NewService(containersSvc interface{}, shimMgr interface{}) *Service {
	return &Service{}
}

func (s *Service) Create(info interface{}, cgroupName string) (int, error) {
	return 0, errNotLinux
}

func (s *Service) Start(containerID string) error {
	return errNotLinux
}

func (s *Service) Kill(containerID string, signal int) error {
	return errNotLinux
}

func (s *Service) Delete(containerID string) error {
	return errNotLinux
}

func (s *Service) ShutdownShim(containerID string) {}

func (s *Service) RestartShim(containerID string, containerPID int) (int, error) {
	return 0, errNotLinux
}

func (s *Service) GetState(containerID string) (*metadata.TaskState, error) {
	return nil, errNotLinux
}

func (s *Service) GetExitInfo(containerID string) (interface{}, error) {
	return nil, errNotLinux
}

func (s *Service) WaitForCreate(containerID string, timeout time.Duration) (int, error) {
	return 0, errNotLinux
}

func (s *Service) List() ([]*metadata.TaskState, error) {
	return nil, errNotLinux
}

func (s *Service) Pause(containerID string) error {
	return errNotLinux
}

func (s *Service) Resume(containerID string) error {
	return errNotLinux
}

func (s *Service) Attach(containerID string) (net.Conn, error) {
	return nil, errNotLinux
}

func (s *Service) ExecStream(containerID string, args []string, tty bool) (net.Conn, error) {
	return nil, errNotLinux
}

func (s *Service) Resize(containerID string, rows, cols uint16) error {
	return errNotLinux
}

func (s *Service) IsShimAlive(containerID string) bool {
	return false
}

func (s *Service) ReadShimPID(containerID string) int {
	return 0
}

func (s *Service) ReadExitInfo(containerID string) (interface{}, error) {
	return nil, errNotLinux
}

// ParseSignal 解析信号字符串为整数
func ParseSignal(signalStr string) (int, error) {
	return 0, fmt.Errorf("仅支持 Linux 平台")
}

// RelayStream 双向转发字节流
func RelayStream(daemonConn, shimConn net.Conn) {}

var errNotLinux = fmt.Errorf("仅支持 Linux 平台")
