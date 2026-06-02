package containerd

/*
=======================================================================
  API 协议 —— containerd 独立进程与 Daemon 之间的通信协议

  对齐 Docker 的 containerd 通信架构：
  ┌──────────┐    Unix Socket    ┌──────────────┐
  │ dockerd  │ ──────────────→  │  containerd  │
  │ (client) │  gRPC-like API   │  (server)    │
  └──────────┘                   └──────────────┘

  mini-docker 的对齐架构：
  ┌──────────┐    Unix Socket    ┌──────────────┐
  │ Daemon   │ ──────────────→  │  containerd  │
  │ (client) │  JSON + 原始流    │  (server)    │
  └──────────┘                   └──────────────┘

  通信模式：
  1. 普通请求/响应：JSON 编码的 Request → Response
  2. 流式连接：先 JSON 握手，再切换为原始字节流（用于 Attach/Exec）

=======================================================================
*/

import (
	"mini-docker/constants"
)

// ---------------------------------------------------------------------------
// 请求类型常量
// ---------------------------------------------------------------------------

const (
	// ReqCreateTask 创建容器任务（启动 shim + runtime create）
	ReqCreateTask = "create_task"
	// ReqStartTask 启动容器任务（通知 shim 执行 runtime start）
	ReqStartTask = "start_task"
	// ReqKillTask 向容器发送信号
	ReqKillTask = "kill_task"
	// ReqDeleteTask 删除容器任务（停止 shim + 清理 runtime 目录）
	ReqDeleteTask = "delete_task"
	// ReqShutdownShim 仅关闭 shim（比 DeleteTask 更轻量，用于自然退出的容器）
	ReqShutdownShim = "shutdown_shim"
	// ReqRestartShim 重启 shim 以接管已有容器
	ReqRestartShim = "restart_shim"
	// ReqGetTaskState 获取容器任务状态
	ReqGetTaskState = "get_task_state"
	// ReqGetExitInfo 获取容器退出信息
	ReqGetExitInfo = "get_exit_info"
	// ReqWaitForCreate 等待容器创建完成
	ReqWaitForCreate = "wait_for_create"
	// ReqListTasks 列出所有容器任务
	ReqListTasks = "list_tasks"
	// ReqPauseTask 暂停容器任务
	ReqPauseTask = "pause_task"
	// ReqResumeTask 恢复容器任务
	ReqResumeTask = "resume_task"
	// ReqAttachTask 连接到容器的 TTY（流式连接）
	ReqAttachTask = "attach_task"
	// ReqExecTaskStream 在容器内执行命令（流式连接）
	ReqExecTaskStream = "exec_task_stream"
	// ReqResizeTask 调整容器终端大小
	ReqResizeTask = "resize_task"
	// ReqIsShimAlive 检查 shim 进程是否存活
	ReqIsShimAlive = "is_shim_alive"
	// ReqReadShimPID 读取 shim PID
	ReqReadShimPID = "read_shim_pid"
	// ReqReadExitInfo 读取退出信息（从磁盘文件）
	ReqReadExitInfo = "read_exit_info"
	// ReqPing 健康检查
	ReqPing = "ping"
)

// ---------------------------------------------------------------------------
// 请求/响应结构体
// ---------------------------------------------------------------------------

// Request containerd API 请求（Daemon → containerd）
type Request struct {
	Type string            `json:"type"`
	Args map[string]string `json:"args,omitempty"`
}

// Response containerd API 响应（containerd → Daemon）
type Response struct {
	Success bool        `json:"success"`
	Message string      `json:"message,omitempty"`
	Data    interface{} `json:"data,omitempty"`
	Stream  bool        `json:"stream,omitempty"`
}

// ---------------------------------------------------------------------------
// 路径常量
// ---------------------------------------------------------------------------

// ContainerdSocketPath 是 containerd 进程的 Unix Socket 路径
const ContainerdSocketPath = constants.ContainerdSocketPath
