//go:build linux

package containerd

/*
=======================================================================
  Client —— Daemon 通过此客户端与 containerd 独立进程通信

  对齐 Docker 的 C/S 架构：
  ┌──────────┐    Unix Socket     ┌──────────────┐
  │ Daemon   │ ───────────────→  │  containerd  │
  │ (client) │  RPC 调用          │  (server)    │
  └──────────┘                    └──────────────┘

  Client 提供与原 Service 相同的方法签名，但底层通过 Unix Socket
  调用 containerd 独立进程的 API，而非本地函数调用。

  对齐 Docker: dockerd 不直接操作 shim/runc，而是通过 containerd 的
  gRPC API 间接管理容器生命周期。

=======================================================================
*/

import (
	"encoding/json"
	"fmt"
	"net"
	"strconv"
	"syscall"
	"time"

	"mini-docker/constants"
	"mini-docker/containerstore"
	"mini-docker/libcontainer"
)

// Client containerd 远程客户端（Daemon 侧使用）
// 对齐 Docker: dockerd 通过 containerd 的 gRPC 客户端与 containerd 通信
type Client struct {
	socketPath string
	timeout    time.Duration
}

// NewClient 创建 containerd 客户端
func NewClient() *Client {
	return &Client{
		socketPath: ContainerdSocketPath,
		timeout:    constants.DefaultConnectTimeout,
	}
}

// CreateTask 创建容器任务（对齐原 Service.CreateTask）
func (c *Client) CreateTask(info *containerstore.ContainerInfo) (shimPID int, err error) {
	resp, err := SendRequest(Request{
		Type: ReqCreateTask,
		Args: map[string]string{"container_id": info.ID},
	})
	if err != nil {
		return 0, err
	}
	if !resp.Success {
		return 0, fmt.Errorf("%s", resp.Message)
	}

	data, _ := json.Marshal(resp.Data)
	var result struct {
		ShimPID int `json:"shim_pid"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return 0, fmt.Errorf("解析 shim PID 失败: %w", err)
	}
	return result.ShimPID, nil
}

// KillTask 向容器发送信号（对齐原 Service.KillTask）
func (c *Client) KillTask(containerID string, signal syscall.Signal) error {
	resp, err := SendRequest(Request{
		Type: ReqKillTask,
		Args: map[string]string{
			"container_id": containerID,
			"signal":       strconv.Itoa(int(signal)),
		},
	})
	if err != nil {
		return err
	}
	if !resp.Success {
		return fmt.Errorf("%s", resp.Message)
	}
	return nil
}

// GetTaskState 获取容器任务状态（对齐原 Service.GetTaskState）
func (c *Client) GetTaskState(containerID string) (*libcontainer.ContainerState, error) {
	resp, err := SendRequest(Request{
		Type: ReqGetTaskState,
		Args: map[string]string{"container_id": containerID},
	})
	if err != nil {
		return nil, err
	}
	if !resp.Success {
		return nil, fmt.Errorf("%s", resp.Message)
	}

	data, _ := json.Marshal(resp.Data)
	var state libcontainer.ContainerState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("解析任务状态失败: %w", err)
	}
	return &state, nil
}

// GetExitInfo 获取容器退出信息（对齐原 Service.GetExitInfo）
func (c *Client) GetExitInfo(containerID string) (*ExitInfo, error) {
	resp, err := SendRequest(Request{
		Type: ReqGetExitInfo,
		Args: map[string]string{"container_id": containerID},
	})
	if err != nil {
		return nil, err
	}
	if !resp.Success {
		return nil, fmt.Errorf("%s", resp.Message)
	}

	data, _ := json.Marshal(resp.Data)
	var info ExitInfo
	if err := json.Unmarshal(data, &info); err != nil {
		return nil, fmt.Errorf("解析退出信息失败: %w", err)
	}
	return &info, nil
}

// DeleteTask 删除容器任务（对齐原 Service.DeleteTask）
func (c *Client) DeleteTask(containerID string) error {
	resp, err := SendRequest(Request{
		Type: ReqDeleteTask,
		Args: map[string]string{"container_id": containerID},
	})
	if err != nil {
		return err
	}
	if !resp.Success {
		return fmt.Errorf("%s", resp.Message)
	}
	return nil
}

// ShutdownShim 关闭 shim（对齐原 Service.ShutdownShim）
func (c *Client) ShutdownShim(containerID string) {
	SendRequest(Request{
		Type: ReqShutdownShim,
		Args: map[string]string{"container_id": containerID},
	})
}

// RestartShim 重启 shim 以接管容器（对齐原 Service.RestartShim）
func (c *Client) RestartShim(containerID string, containerPID int) (int, error) {
	resp, err := SendRequest(Request{
		Type: ReqRestartShim,
		Args: map[string]string{
			"container_id":  containerID,
			"container_pid": strconv.Itoa(containerPID),
		},
	})
	if err != nil {
		return 0, err
	}
	if !resp.Success {
		return 0, fmt.Errorf("%s", resp.Message)
	}

	data, _ := json.Marshal(resp.Data)
	var result struct {
		ShimPID int `json:"shim_pid"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return 0, fmt.Errorf("解析 shim PID 失败: %w", err)
	}
	return result.ShimPID, nil
}

// WaitForCreate 等待容器创建完成（对齐原 Service.WaitForCreate）
func (c *Client) WaitForCreate(containerID string, timeout time.Duration) (int, error) {
	resp, err := SendRequest(Request{
		Type: ReqWaitForCreate,
		Args: map[string]string{
			"container_id": containerID,
			"timeout":      strconv.Itoa(int(timeout.Milliseconds())),
		},
	})
	if err != nil {
		return 0, err
	}
	if !resp.Success {
		return 0, fmt.Errorf("%s", resp.Message)
	}

	data, _ := json.Marshal(resp.Data)
	var result struct {
		Pid int `json:"pid"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return 0, fmt.Errorf("解析 PID 失败: %w", err)
	}
	return result.Pid, nil
}

// StartTask 启动容器任务（对齐原 Service.StartTask）
func (c *Client) StartTask(containerID string) error {
	resp, err := SendRequest(Request{
		Type: ReqStartTask,
		Args: map[string]string{"container_id": containerID},
	})
	if err != nil {
		return err
	}
	if !resp.Success {
		return fmt.Errorf("%s", resp.Message)
	}
	return nil
}

// PauseTask 暂停容器任务（对齐原 Service.PauseTask）
func (c *Client) PauseTask(containerID string) error {
	resp, err := SendRequest(Request{
		Type: ReqPauseTask,
		Args: map[string]string{"container_id": containerID},
	})
	if err != nil {
		return err
	}
	if !resp.Success {
		return fmt.Errorf("%s", resp.Message)
	}
	return nil
}

// ResumeTask 恢复容器任务（对齐原 Service.ResumeTask）
func (c *Client) ResumeTask(containerID string) error {
	resp, err := SendRequest(Request{
		Type: ReqResumeTask,
		Args: map[string]string{"container_id": containerID},
	})
	if err != nil {
		return err
	}
	if !resp.Success {
		return fmt.Errorf("%s", resp.Message)
	}
	return nil
}

// AttachTask 连接到容器 TTY（对齐原 Service.AttachTask）
// 返回一个可用于双向 I/O 转发的连接
func (c *Client) AttachTask(containerID string) (net.Conn, error) {
	conn, resp, err := SendStreamRequest(Request{
		Type: ReqAttachTask,
		Args: map[string]string{"container_id": containerID},
	})
	if err != nil {
		return nil, err
	}
	if resp != nil && !resp.Success {
		conn.Close()
		return nil, fmt.Errorf("attach 失败: %s", resp.Message)
	}
	return conn, nil
}

// ExecTaskStream 在容器内执行命令（对齐原 Service.ExecTaskStream）
// 返回一个可用于 I/O 转发的连接
func (c *Client) ExecTaskStream(containerID string, args []string, tty bool) (net.Conn, error) {
	argsJSON, _ := json.Marshal(args)
	conn, resp, err := SendStreamRequest(Request{
		Type: ReqExecTaskStream,
		Args: map[string]string{
			"container_id": containerID,
			"tty":          strconv.FormatBool(tty),
			"args_json":    string(argsJSON),
		},
	})
	if err != nil {
		return nil, err
	}
	if resp != nil && !resp.Success {
		conn.Close()
		return nil, fmt.Errorf("%s", resp.Message)
	}
	return conn, nil
}

// ResizeTask 调整容器终端大小（对齐原 Service.ResizeTask）
func (c *Client) ResizeTask(containerID string, rows, cols uint16) error {
	resp, err := SendRequest(Request{
		Type: ReqResizeTask,
		Args: map[string]string{
			"container_id": containerID,
			"rows":         strconv.Itoa(int(rows)),
			"cols":         strconv.Itoa(int(cols)),
		},
	})
	if err != nil {
		return err
	}
	if !resp.Success {
		return fmt.Errorf("%s", resp.Message)
	}
	return nil
}

// ListTasks 列出所有容器任务（对齐原 Service.ListTasks）
func (c *Client) ListTasks() ([]*libcontainer.ContainerState, error) {
	resp, err := SendRequest(Request{
		Type: ReqListTasks,
		Args: map[string]string{},
	})
	if err != nil {
		return nil, err
	}
	if !resp.Success {
		return nil, fmt.Errorf("%s", resp.Message)
	}

	data, _ := json.Marshal(resp.Data)
	var states []*libcontainer.ContainerState
	if err := json.Unmarshal(data, &states); err != nil {
		return nil, fmt.Errorf("解析任务列表失败: %w", err)
	}
	return states, nil
}

// IsShimAlive 通过 containerd 检查 shim 是否存活
func (c *Client) IsShimAlive(containerID string) bool {
	resp, err := SendRequest(Request{
		Type: ReqIsShimAlive,
		Args: map[string]string{"container_id": containerID},
	})
	if err != nil {
		return false
	}
	if !resp.Success {
		return false
	}
	data, _ := json.Marshal(resp.Data)
	var result struct {
		Alive bool `json:"alive"`
	}
	if json.Unmarshal(data, &result) != nil {
		return false
	}
	return result.Alive
}

// ReadShimPID 通过 containerd 读取 shim PID
func (c *Client) ReadShimPID(containerID string) int {
	resp, err := SendRequest(Request{
		Type: ReqReadShimPID,
		Args: map[string]string{"container_id": containerID},
	})
	if err != nil {
		return 0
	}
	if !resp.Success {
		return 0
	}
	data, _ := json.Marshal(resp.Data)
	var result struct {
		Pid int `json:"pid"`
	}
	if json.Unmarshal(data, &result) != nil {
		return 0
	}
	return result.Pid
}

// ReadExitInfo 通过 containerd 读取退出信息
func (c *Client) ReadExitInfo(containerID string) (*ExitInfo, error) {
	resp, err := SendRequest(Request{
		Type: ReqReadExitInfo,
		Args: map[string]string{"container_id": containerID},
	})
	if err != nil {
		return nil, err
	}
	if !resp.Success {
		return nil, fmt.Errorf("%s", resp.Message)
	}

	data, _ := json.Marshal(resp.Data)
	var info ExitInfo
	if err := json.Unmarshal(data, &info); err != nil {
		return nil, fmt.Errorf("解析退出信息失败: %w", err)
	}
	return &info, nil
}
