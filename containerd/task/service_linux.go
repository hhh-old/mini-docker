//go:build linux

package task

/*
=======================================================================
  Task Service —— 对齐 containerd 的 Task Service 插件

  真实 containerd 中，Task Service 是独立的 gRPC 服务，管理容器的运行时生命周期：
  - 创建/启动/删除容器任务
  - 信号发送（kill）
  - 暂停/恢复
  - 状态查询
  - 流式连接（attach/exec）
  - 终端大小调整

  Task Service 依赖 Shim Manager 执行底层操作，依赖 Container Service 获取元数据。
  这对齐了真实 containerd 的分层架构：
  Container Service (元数据) → Task Service (运行时) → Shim Manager (进程管理) → runc

=======================================================================
*/

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"sync"
	"time"

	"mini-docker/containerd/containers"
	"mini-docker/containerd/events"
	"mini-docker/containerd/metadata"
	"mini-docker/containerd/runtime"
	"mini-docker/containerd/shim"
	"mini-docker/containerstore"
)

// Service Task 生命周期管理服务
// 对齐 containerd: Task Service 是容器运行时的核心服务
// 不缓存 TaskState——对齐真实 containerd：shim 是 source of truth，每次从 runtime 实时获取
// Task Service 不再直接依赖 ShimManager，而是通过 Runtime Service 调用具体 runtime 实现
type Service struct {
	containers *containers.Service
	runtime    *runtime.Service
	events     *events.Service // 事件总线服务，发布 task 生命周期事件
}

// NewService 创建 Task Service
// 对齐 containerd: Task Service 依赖 Runtime Service（runtime.v2 抽象层），不直接操作 shim
func NewService(containersSvc *containers.Service, rt *runtime.Service, ev *events.Service) *Service {
	return &Service{
		containers: containersSvc,
		runtime:    rt,
		events:     ev,
	}
}

// publishEvent 发布事件（events 为 nil 时静默跳过）
func (s *Service) publishEvent(topic string, ev interface{}) {
	if s.events == nil {
		return
	}
	s.events.Publish(&events.Envelope{
		Topic: topic,
		Event: ev,
	})
}

// getContainerRuntime 获取容器配置的 runtime 类型，默认 runc
func (s *Service) getContainerRuntime(containerID string) (runtime.RuntimeType, error) {
	info, err := s.containers.Get(containerID)
	if err != nil {
		return "", fmt.Errorf("获取容器元数据失败: %w", err)
	}
	return runtime.RuntimeType(info.Runtime), nil
}

// Create 创建容器任务：委托给 Runtime Service
// 对齐 Docker: dockerd → containerd.CreateTask → Runtime Service → shim
func (s *Service) Create(info *containerstore.ContainerInfo, cgroupName string) (shimPID int, err error) {
	shimPID, err = s.runtime.CreateTask(context.Background(), info, cgroupName)
	if err != nil {
		return 0, err
	}

	s.publishEvent("/tasks/create", events.TaskCreate{
		ContainerID: info.ID,
		Image:       info.Image,
		PID:         shimPID,
	})

	return shimPID, nil
}

// Start 启动容器任务
func (s *Service) Start(containerID string) error {
	rtType, err := s.getContainerRuntime(containerID)
	if err != nil {
		return err
	}
	if err := s.runtime.Start(context.Background(), containerID, rtType); err != nil {
		return fmt.Errorf("启动任务失败: %w", err)
	}
	s.publishEvent("/tasks/start", events.TaskStart{
		ContainerID: containerID,
	})
	return nil
}

// Kill 向容器发送信号
func (s *Service) Kill(containerID string, signal int) error {
	rtType, err := s.getContainerRuntime(containerID)
	if err != nil {
		return err
	}
	if err := s.runtime.Kill(context.Background(), containerID, signal, rtType); err != nil {
		return fmt.Errorf("发送信号失败: %w", err)
	}
	return nil
}

// Delete 删除容器任务
func (s *Service) Delete(containerID string) error {
	rtType, err := s.getContainerRuntime(containerID)
	if err != nil {
		return err
	}
	if err := s.runtime.Delete(context.Background(), containerID, rtType); err != nil {
		return fmt.Errorf("删除任务失败: %w", err)
	}
	s.publishEvent("/tasks/delete", events.TaskDelete{
		ContainerID: containerID,
	})
	return nil
}

// ShutdownShim 仅关闭 shim（比 Delete 更轻量，用于自然退出的容器）
func (s *Service) ShutdownShim(containerID string) {
	rtType, err := s.getContainerRuntime(containerID)
	if err != nil {
		return
	}
	s.runtime.ShutdownShim(context.Background(), containerID, rtType)
}

// RestartShim 重启 shim 以接管已有容器
func (s *Service) RestartShim(containerID string, containerPID int) (int, error) {
	rtType, err := s.getContainerRuntime(containerID)
	if err != nil {
		return 0, err
	}
	return s.runtime.RestartShim(context.Background(), containerID, containerPID, rtType)
}

// GetState 获取容器任务状态
// 对齐 containerd: 始终从 runtime 获取实时状态，不使用缓存
func (s *Service) GetState(containerID string) (*metadata.TaskState, error) {
	rtType, err := s.getContainerRuntime(containerID)
	if err != nil {
		return nil, err
	}
	return s.runtime.GetState(context.Background(), containerID, rtType)
}

// List 列出所有容器任务
// 对齐 containerd: 通过 Runtime Service 聚合所有 runtime 的任务
func (s *Service) List() ([]*metadata.TaskState, error) {
	return s.runtime.ListAll(context.Background())
}

// GetExitInfo 获取容器退出信息
func (s *Service) GetExitInfo(containerID string) (*shim.ExitInfo, error) {
	rtType, err := s.getContainerRuntime(containerID)
	if err != nil {
		return nil, err
	}
	return s.runtime.GetExitInfo(context.Background(), containerID, rtType)
}

// WaitForCreate 等待容器创建完成
func (s *Service) WaitForCreate(containerID string, timeout time.Duration) (int, error) {
	rtType, err := s.getContainerRuntime(containerID)
	if err != nil {
		return 0, err
	}
	return s.runtime.WaitForCreate(context.Background(), containerID, timeout, rtType)
}

// Pause 暂停容器任务
func (s *Service) Pause(containerID string) error {
	rtType, err := s.getContainerRuntime(containerID)
	if err != nil {
		return err
	}
	if err := s.runtime.Pause(context.Background(), containerID, rtType); err != nil {
		return fmt.Errorf("暂停容器失败: %w", err)
	}
	s.publishEvent("/tasks/paused", events.TaskPause{
		ContainerID: containerID,
	})
	return nil
}

// Resume 恢复容器任务
func (s *Service) Resume(containerID string) error {
	rtType, err := s.getContainerRuntime(containerID)
	if err != nil {
		return err
	}
	if err := s.runtime.Resume(context.Background(), containerID, rtType); err != nil {
		return fmt.Errorf("恢复容器失败: %w", err)
	}
	s.publishEvent("/tasks/resumed", events.TaskResume{
		ContainerID: containerID,
	})
	return nil
}

// Attach 连接到容器的 TTY，返回可用于双向 I/O 转发的连接
func (s *Service) Attach(containerID string) (net.Conn, error) {
	rtType, err := s.getContainerRuntime(containerID)
	if err != nil {
		return nil, err
	}
	return s.runtime.Attach(context.Background(), containerID, rtType)
}

// ExecStream 在容器内执行命令，返回与 shim 通信的连接
func (s *Service) ExecStream(containerID string, args []string, tty bool) (net.Conn, error) {
	rtType, err := s.getContainerRuntime(containerID)
	if err != nil {
		return nil, err
	}
	return s.runtime.ExecStream(context.Background(), containerID, args, tty, rtType)
}

// Resize 调整容器终端大小
func (s *Service) Resize(containerID string, rows, cols uint16) error {
	rtType, err := s.getContainerRuntime(containerID)
	if err != nil {
		return err
	}
	if err := s.runtime.Resize(context.Background(), containerID, rows, cols, rtType); err != nil {
		return fmt.Errorf("调整窗口大小失败: %w", err)
	}
	return nil
}

// IsShimAlive 检查 shim 进程是否存活
func (s *Service) IsShimAlive(containerID string) bool {
	rtType, err := s.getContainerRuntime(containerID)
	if err != nil {
		return false
	}
	return s.runtime.IsShimAlive(context.Background(), containerID, rtType)
}

// ReadShimPID 读取 shim PID
func (s *Service) ReadShimPID(containerID string) int {
	rtType, err := s.getContainerRuntime(containerID)
	if err != nil {
		return 0
	}
	return s.runtime.ReadShimPID(context.Background(), containerID, rtType)
}

// ReadExitInfo 读取退出信息（从磁盘文件）
func (s *Service) ReadExitInfo(containerID string) (*shim.ExitInfo, error) {
	rtType, err := s.getContainerRuntime(containerID)
	if err != nil {
		return nil, err
	}
	return s.runtime.ReadExitInfo(context.Background(), containerID, rtType)
}

// IOCopy 双向 I/O 转发辅助函数
func IOCopy(dst net.Conn, src net.Conn) (int64, error) {
	buf := make([]byte, 32768)
	var total int64
	for {
		n, err := src.Read(buf)
		if n > 0 {
			_, werr := dst.Write(buf[:n])
			if werr != nil {
				return total, werr
			}
			total += int64(n)
		}
		if err != nil {
			return total, err
		}
	}
}

// RelayStream 双向转发字节流，用于 attach/exec 场景
func RelayStream(daemonConn, shimConn net.Conn) {
	var once sync.Once
	done := make(chan struct{})

	go func() {
		defer once.Do(func() { close(done) })
		_, _ = IOCopy(shimConn, daemonConn)
	}()
	go func() {
		defer once.Do(func() { close(done) })
		_, _ = IOCopy(daemonConn, shimConn)
	}()
	<-done
	daemonConn.Close()
	shimConn.Close()
}

// ParseSignal 解析信号字符串为整数
func ParseSignal(signalStr string) (int, error) {
	sig, err := strconv.Atoi(signalStr)
	if err != nil {
		return 0, fmt.Errorf("无效信号: %s", signalStr)
	}
	return sig, nil
}
