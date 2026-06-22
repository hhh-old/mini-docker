//go:build linux

package runtime

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"

	"mini-docker/constants"
	"mini-docker/containerd/metadata"
	"mini-docker/containerd/shim"
	"mini-docker/containerstore"
	"mini-docker/spec"
	"mini-docker/types"
)

// RuncRuntime 基于 runc/libcontainer 的 Runtime 实现。
// 内部通过 ShimManager 管理 shim 进程，是真实 containerd 中 runtime.v2/runc 的简化对应。
type RuncRuntime struct {
	shimMgr *shim.ShimManager
}

// NewRuncRuntime 创建 RuncRuntime 实例
func NewRuncRuntime(shimMgr *shim.ShimManager) *RuncRuntime {
	return &RuncRuntime{shimMgr: shimMgr}
}

// Type 返回 runc runtime 类型
func (r *RuncRuntime) Type() RuntimeType {
	return TypeRunc
}

// BuildOCISpec 根据容器信息生成 OCI Runtime Spec
func (r *RuncRuntime) BuildOCISpec(info *containerstore.ContainerInfo, cgroupName string) *spec.Spec {
	return r.shimMgr.BuildOCISpec(info, cgroupName)
}

// CreateTask 创建并启动 shim，返回 shim PID
func (r *RuncRuntime) CreateTask(ctx context.Context, info *containerstore.ContainerInfo, cgroupName string) (int, error) {
	bundlePath := filepath.Join(constants.RuntimeDir, info.ID, "bundle")
	ociSpec := r.BuildOCISpec(info, cgroupName)
	if err := spec.SaveSpec(ociSpec, bundlePath); err != nil {
		return 0, fmt.Errorf("保存 config.json 失败: %w", err)
	}

	shimArgs := []string{"shim", info.ID, bundlePath}
	if info.Tty {
		shimArgs = append(shimArgs, "--tty")
	}
	cmd := r.shimMgr.NewCommand(shimArgs)

	logDir := filepath.Join(filepath.Dir(constants.DaemonLogPath), "shim")
	if err := os.MkdirAll(logDir, 0755); err != nil {
		return 0, fmt.Errorf("创建 shim 日志目录失败: %w", err)
	}
	logPath := filepath.Join(logDir, info.ID+".log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err == nil {
		cmd.Stdout = logFile
		cmd.Stderr = logFile
	}

	if err := cmd.Start(); err != nil {
		if logFile != nil {
			logFile.Close()
		}
		return 0, fmt.Errorf("启动 shim 失败: %w", err)
	}
	if logFile != nil {
		logFile.Close()
	}

	shimPID := cmd.Process.Pid
	socketPath := filepath.Join(constants.ShimDir, info.ID, "shim.sock")
	if err := r.shimMgr.WaitForSocket(socketPath, constants.SocketWaitTimeout); err != nil {
		cmd.Process.Kill()
		cmd.Wait()
		return 0, fmt.Errorf("shim socket 未就绪: %w", err)
	}
	return shimPID, nil
}

// Start 启动容器内进程
func (r *RuncRuntime) Start(ctx context.Context, containerID string) error {
	return r.shimMgr.Call(containerID, types.ShimRequest{Type: "start"})
}

// Kill 向容器发送信号
func (r *RuncRuntime) Kill(ctx context.Context, containerID string, signal int) error {
	return r.shimMgr.Call(containerID, types.ShimRequest{Type: "kill", Signal: signal})
}

// Delete 删除容器任务
func (r *RuncRuntime) Delete(ctx context.Context, containerID string) error {
	return r.shimMgr.Delete(containerID)
}

// Pause 暂停容器
func (r *RuncRuntime) Pause(ctx context.Context, containerID string) error {
	return r.shimMgr.Call(containerID, types.ShimRequest{Type: "pause"})
}

// Resume 恢复容器
func (r *RuncRuntime) Resume(ctx context.Context, containerID string) error {
	return r.shimMgr.Call(containerID, types.ShimRequest{Type: "unpause"})
}

// GetState 获取容器实时状态
func (r *RuncRuntime) GetState(ctx context.Context, containerID string) (*metadata.TaskState, error) {
	return r.shimMgr.GetState(containerID)
}

// List 列出所有容器任务状态
func (r *RuncRuntime) List(ctx context.Context) ([]*metadata.TaskState, error) {
	return r.shimMgr.ListStates()
}

// GetExitInfo 获取容器退出信息
func (r *RuncRuntime) GetExitInfo(ctx context.Context, containerID string) (*shim.ExitInfo, error) {
	conn, err := r.shimMgr.Connect(containerID)
	if err != nil {
		return r.shimMgr.ReadExitInfoFromFile(containerID)
	}
	conn.Close()

	var info shim.ExitInfo
	if err := r.shimMgr.CallWithData(containerID, types.ShimRequest{Type: "exit_info"}, &info); err != nil {
		if fileInfo, readErr := r.shimMgr.ReadExitInfoFromFile(containerID); readErr == nil && fileInfo != nil {
			return fileInfo, nil
		}
		return nil, fmt.Errorf("获取退出信息失败: %w", err)
	}
	return &info, nil
}

// ShutdownShim 仅关闭 shim
func (r *RuncRuntime) ShutdownShim(ctx context.Context, containerID string) {
	r.shimMgr.Shutdown(containerID)
}

// RestartShim 重启 shim 以接管已有容器
func (r *RuncRuntime) RestartShim(ctx context.Context, containerID string, containerPID int) (int, error) {
	return r.shimMgr.Restart(containerID, containerPID)
}

// WaitForCreate 等待容器创建完成
func (r *RuncRuntime) WaitForCreate(ctx context.Context, containerID string, timeout time.Duration) (int, error) {
	return r.shimMgr.WaitForCreate(containerID, timeout)
}

// Attach 连接到容器 TTY
func (r *RuncRuntime) Attach(ctx context.Context, containerID string) (net.Conn, error) {
	return r.shimMgr.Attach(containerID)
}

// ExecStream 在容器内执行命令
func (r *RuncRuntime) ExecStream(ctx context.Context, containerID string, args []string, tty bool) (net.Conn, error) {
	return r.shimMgr.ExecStream(containerID, args, tty)
}

// Resize 调整容器终端大小
func (r *RuncRuntime) Resize(ctx context.Context, containerID string, rows, cols uint16) error {
	return r.shimMgr.Call(containerID, types.ShimRequest{Type: "resize", Rows: rows, Cols: cols})
}

// IsShimAlive 检查 shim 是否存活
func (r *RuncRuntime) IsShimAlive(ctx context.Context, containerID string) bool {
	return r.shimMgr.IsAlive(containerID)
}

// ReadShimPID 读取 shim PID
func (r *RuncRuntime) ReadShimPID(ctx context.Context, containerID string) int {
	return r.shimMgr.ReadPID(containerID)
}

// ReadExitInfo 从磁盘读取退出信息
func (r *RuncRuntime) ReadExitInfo(ctx context.Context, containerID string) (*shim.ExitInfo, error) {
	return r.shimMgr.ReadExitInfoFromFile(containerID)
}
