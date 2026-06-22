//go:build linux

package runtime

import (
	"context"
	"fmt"
	"net"
	"time"

	"mini-docker/containerd/metadata"
	"mini-docker/containerd/shim"
	"mini-docker/containerstore"
	"mini-docker/spec"
)

// Service Runtime Manager
// 对齐 containerd: runtime.v2 作为 Runtime 插件管理器，按 RuntimeType 路由到具体实现。
// 当前默认注册 runc runtime；未来可通过 Register 方法扩展 kata、gvisor 等实现。
type Service struct {
	runtimes map[RuntimeType]Runtime
}

// NewService 创建 Runtime Manager，默认注册 runc runtime
func NewService(shimMgr *shim.ShimManager) *Service {
	s := &Service{
		runtimes: make(map[RuntimeType]Runtime),
	}
	s.Register(NewRuncRuntime(shimMgr))
	return s
}

// Register 注册新的 Runtime 实现
func (s *Service) Register(rt Runtime) {
	s.runtimes[rt.Type()] = rt
}

// getRuntime 根据 RuntimeType 获取实现，空值默认返回 runc
func (s *Service) getRuntime(rtType RuntimeType) (Runtime, error) {
	if rtType == "" {
		rtType = TypeRunc
	}
	rt, ok := s.runtimes[rtType]
	if !ok {
		return nil, fmt.Errorf("不支持的 runtime: %s", rtType)
	}
	return rt, nil
}

// resolveRuntime 根据容器信息选择 Runtime
func (s *Service) resolveRuntime(info *containerstore.ContainerInfo) (Runtime, error) {
	return s.getRuntime(RuntimeType(info.Runtime))
}

// BuildOCISpec 生成 OCI Spec
func (s *Service) BuildOCISpec(info *containerstore.ContainerInfo, cgroupName string) *spec.Spec {
	rt, err := s.resolveRuntime(info)
	if err != nil {
		return nil
	}
	return rt.BuildOCISpec(info, cgroupName)
}

// CreateTask 创建容器任务
func (s *Service) CreateTask(ctx context.Context, info *containerstore.ContainerInfo, cgroupName string) (int, error) {
	rt, err := s.resolveRuntime(info)
	if err != nil {
		return 0, err
	}
	return rt.CreateTask(ctx, info, cgroupName)
}

// Start 启动容器内进程
func (s *Service) Start(ctx context.Context, containerID string, rtType RuntimeType) error {
	rt, err := s.getRuntime(rtType)
	if err != nil {
		return err
	}
	return rt.Start(ctx, containerID)
}

// Kill 发送信号
func (s *Service) Kill(ctx context.Context, containerID string, signal int, rtType RuntimeType) error {
	rt, err := s.getRuntime(rtType)
	if err != nil {
		return err
	}
	return rt.Kill(ctx, containerID, signal)
}

// Delete 删除容器任务
func (s *Service) Delete(ctx context.Context, containerID string, rtType RuntimeType) error {
	rt, err := s.getRuntime(rtType)
	if err != nil {
		return err
	}
	return rt.Delete(ctx, containerID)
}

// Pause 暂停容器
func (s *Service) Pause(ctx context.Context, containerID string, rtType RuntimeType) error {
	rt, err := s.getRuntime(rtType)
	if err != nil {
		return err
	}
	return rt.Pause(ctx, containerID)
}

// Resume 恢复容器
func (s *Service) Resume(ctx context.Context, containerID string, rtType RuntimeType) error {
	rt, err := s.getRuntime(rtType)
	if err != nil {
		return err
	}
	return rt.Resume(ctx, containerID)
}

// GetState 获取容器实时状态
func (s *Service) GetState(ctx context.Context, containerID string, rtType RuntimeType) (*metadata.TaskState, error) {
	rt, err := s.getRuntime(rtType)
	if err != nil {
		return nil, err
	}
	return rt.GetState(ctx, containerID)
}

// List 列出指定 runtime 的所有任务
func (s *Service) List(ctx context.Context, rtType RuntimeType) ([]*metadata.TaskState, error) {
	rt, err := s.getRuntime(rtType)
	if err != nil {
		return nil, err
	}
	return rt.List(ctx)
}

// ListAll 列出所有已注册 runtime 的任务
func (s *Service) ListAll(ctx context.Context) ([]*metadata.TaskState, error) {
	var all []*metadata.TaskState
	for _, rt := range s.runtimes {
		states, err := rt.List(ctx)
		if err != nil {
			return nil, err
		}
		all = append(all, states...)
	}
	return all, nil
}

// GetExitInfo 获取容器退出信息
func (s *Service) GetExitInfo(ctx context.Context, containerID string, rtType RuntimeType) (*shim.ExitInfo, error) {
	rt, err := s.getRuntime(rtType)
	if err != nil {
		return nil, err
	}
	return rt.GetExitInfo(ctx, containerID)
}

// ShutdownShim 仅关闭 shim
func (s *Service) ShutdownShim(ctx context.Context, containerID string, rtType RuntimeType) {
	rt, err := s.getRuntime(rtType)
	if err != nil {
		return
	}
	rt.ShutdownShim(ctx, containerID)
}

// RestartShim 重启 shim
func (s *Service) RestartShim(ctx context.Context, containerID string, containerPID int, rtType RuntimeType) (int, error) {
	rt, err := s.getRuntime(rtType)
	if err != nil {
		return 0, err
	}
	return rt.RestartShim(ctx, containerID, containerPID)
}

// WaitForCreate 等待容器创建完成
func (s *Service) WaitForCreate(ctx context.Context, containerID string, timeout time.Duration, rtType RuntimeType) (int, error) {
	rt, err := s.getRuntime(rtType)
	if err != nil {
		return 0, err
	}
	return rt.WaitForCreate(ctx, containerID, timeout)
}

// Attach 连接容器 TTY
func (s *Service) Attach(ctx context.Context, containerID string, rtType RuntimeType) (net.Conn, error) {
	rt, err := s.getRuntime(rtType)
	if err != nil {
		return nil, err
	}
	return rt.Attach(ctx, containerID)
}

// ExecStream 在容器内执行命令
func (s *Service) ExecStream(ctx context.Context, containerID string, args []string, tty bool, rtType RuntimeType) (net.Conn, error) {
	rt, err := s.getRuntime(rtType)
	if err != nil {
		return nil, err
	}
	return rt.ExecStream(ctx, containerID, args, tty)
}

// Resize 调整终端大小
func (s *Service) Resize(ctx context.Context, containerID string, rows, cols uint16, rtType RuntimeType) error {
	rt, err := s.getRuntime(rtType)
	if err != nil {
		return err
	}
	return rt.Resize(ctx, containerID, rows, cols)
}

// IsShimAlive 检查 shim 是否存活
func (s *Service) IsShimAlive(ctx context.Context, containerID string, rtType RuntimeType) bool {
	rt, err := s.getRuntime(rtType)
	if err != nil {
		return false
	}
	return rt.IsShimAlive(ctx, containerID)
}

// ReadShimPID 读取 shim PID
func (s *Service) ReadShimPID(ctx context.Context, containerID string, rtType RuntimeType) int {
	rt, err := s.getRuntime(rtType)
	if err != nil {
		return 0
	}
	return rt.ReadShimPID(ctx, containerID)
}

// ReadExitInfo 从磁盘读取退出信息
func (s *Service) ReadExitInfo(ctx context.Context, containerID string, rtType RuntimeType) (*shim.ExitInfo, error) {
	rt, err := s.getRuntime(rtType)
	if err != nil {
		return nil, err
	}
	return rt.ReadExitInfo(ctx, containerID)
}
