//go:build linux

package containerd

/*
=======================================================================
  Task 生命周期处理器（对齐 Docker: containerd Task Service）

  处理 Daemon 发来的 Task 相关请求，包括：
  - 创建/启动/删除容器任务
  - 信号发送（kill）
  - 暂停/恢复
  - 状态查询
  - 流式连接（attach/exec）
  - 终端大小调整
  - Shim 进程管理（存活检查、PID 读取）

  重构为薄层：handler 只做参数解析和调用 Task Service，
  业务逻辑全部委托给 task.Service 插件。

=======================================================================
*/

import (
	"encoding/json"
	"fmt"
	"net"
	"strconv"
	"time"

	"mini-docker/containerd/plugin"
	"mini-docker/containerd/task"
)

// getTaskService 从插件管理器获取 Task Service
func (c *Containerd) getTaskService() *task.Service {
	inst, _ := c.plugins.Get(plugin.TypeService, "task")
	if inst == nil {
		return nil
	}
	return inst.(*task.Service)
}

// handleCreateTask 处理创建容器任务请求
// 对齐 Docker: dockerd → containerd.CreateTask → 启动 shim + runtime create
func (c *Containerd) handleCreateTask(req Request) Response {
	containerID := req.Args["container_id"]
	if containerID == "" {
		return Response{Success: false, Message: "容器 ID 不能为空"}
	}

	svc := c.getTaskService()
	if svc == nil {
		return Response{Success: false, Message: "Task 服务未初始化"}
	}

	// 通过 Container Service 获取容器元数据
	info, err := c.getContainersService().Get(containerID)
	if err != nil {
		return Response{Success: false, Message: fmt.Sprintf("加载容器信息失败: %v", err)}
	}

	// CgroupName 从请求参数获取（由 Daemon 生成并传入）
	cgroupName := req.Args["cgroup_name"]

	shimPID, err := svc.Create(info, cgroupName)
	if err != nil {
		return Response{Success: false, Message: fmt.Sprintf("创建任务失败: %v", err)}
	}

	return Response{Success: true, Data: map[string]interface{}{"shim_pid": shimPID}}
}

// handleStartTask 处理启动容器任务请求
func (c *Containerd) handleStartTask(req Request) Response {
	containerID := req.Args["container_id"]
	if containerID == "" {
		return Response{Success: false, Message: "容器 ID 不能为空"}
	}
	svc := c.getTaskService()
	if svc == nil {
		return Response{Success: false, Message: "Task 服务未初始化"}
	}
	if err := svc.Start(containerID); err != nil {
		return Response{Success: false, Message: err.Error()}
	}
	return Response{Success: true}
}

// handleKillTask 处理发送信号请求
func (c *Containerd) handleKillTask(req Request) Response {
	containerID := req.Args["container_id"]
	signalStr := req.Args["signal"]
	if containerID == "" || signalStr == "" {
		return Response{Success: false, Message: "需要指定容器 ID 和信号"}
	}
	sig, err := task.ParseSignal(signalStr)
	if err != nil {
		return Response{Success: false, Message: err.Error()}
	}
	svc := c.getTaskService()
	if svc == nil {
		return Response{Success: false, Message: "Task 服务未初始化"}
	}
	if err := svc.Kill(containerID, sig); err != nil {
		return Response{Success: false, Message: err.Error()}
	}
	return Response{Success: true}
}

// handleDeleteTask 处理删除容器任务请求
func (c *Containerd) handleDeleteTask(req Request) Response {
	containerID := req.Args["container_id"]
	if containerID == "" {
		return Response{Success: false, Message: "容器 ID 不能为空"}
	}
	svc := c.getTaskService()
	if svc == nil {
		return Response{Success: false, Message: "Task 服务未初始化"}
	}
	if err := svc.Delete(containerID); err != nil {
		return Response{Success: false, Message: err.Error()}
	}
	return Response{Success: true}
}

// handleShutdownShim 处理关闭 shim 请求
func (c *Containerd) handleShutdownShim(req Request) Response {
	containerID := req.Args["container_id"]
	if containerID == "" {
		return Response{Success: false, Message: "容器 ID 不能为空"}
	}
	svc := c.getTaskService()
	if svc == nil {
		return Response{Success: false, Message: "Task 服务未初始化"}
	}
	svc.ShutdownShim(containerID)
	return Response{Success: true}
}

// handleRestartShim 处理重启 shim 请求
func (c *Containerd) handleRestartShim(req Request) Response {
	containerID := req.Args["container_id"]
	pidStr := req.Args["container_pid"]
	if containerID == "" || pidStr == "" {
		return Response{Success: false, Message: "需要指定容器 ID 和容器 PID"}
	}
	containerPID, err := strconv.Atoi(pidStr)
	if err != nil {
		return Response{Success: false, Message: fmt.Sprintf("无效 PID: %s", pidStr)}
	}
	svc := c.getTaskService()
	if svc == nil {
		return Response{Success: false, Message: "Task 服务未初始化"}
	}
	shimPID, err := svc.RestartShim(containerID, containerPID)
	if err != nil {
		return Response{Success: false, Message: err.Error()}
	}
	return Response{Success: true, Data: map[string]interface{}{"shim_pid": shimPID}}
}

// handleGetTaskState 处理获取任务状态请求
func (c *Containerd) handleGetTaskState(req Request) Response {
	containerID := req.Args["container_id"]
	if containerID == "" {
		return Response{Success: false, Message: "容器 ID 不能为空"}
	}
	svc := c.getTaskService()
	if svc == nil {
		return Response{Success: false, Message: "Task 服务未初始化"}
	}
	state, err := svc.GetState(containerID)
	if err != nil {
		return Response{Success: false, Message: err.Error()}
	}
	return Response{Success: true, Data: state}
}

// handleGetExitInfo 处理获取退出信息请求
func (c *Containerd) handleGetExitInfo(req Request) Response {
	containerID := req.Args["container_id"]
	if containerID == "" {
		return Response{Success: false, Message: "容器 ID 不能为空"}
	}
	svc := c.getTaskService()
	if svc == nil {
		return Response{Success: false, Message: "Task 服务未初始化"}
	}
	info, err := svc.GetExitInfo(containerID)
	if err != nil {
		return Response{Success: false, Message: err.Error()}
	}
	return Response{Success: true, Data: info}
}

// handleWaitForCreate 处理等待容器创建完成请求
func (c *Containerd) handleWaitForCreate(req Request) Response {
	containerID := req.Args["container_id"]
	timeoutStr := req.Args["timeout"]
	if containerID == "" || timeoutStr == "" {
		return Response{Success: false, Message: "需要指定容器 ID 和超时时间"}
	}
	timeoutMs, _ := strconv.Atoi(timeoutStr)
	timeout := time.Duration(timeoutMs) * time.Millisecond

	svc := c.getTaskService()
	if svc == nil {
		return Response{Success: false, Message: "Task 服务未初始化"}
	}
	pid, err := svc.WaitForCreate(containerID, timeout)
	if err != nil {
		return Response{Success: false, Message: err.Error()}
	}
	return Response{Success: true, Data: map[string]interface{}{"pid": pid}}
}

// handleListTasks 处理列出所有任务请求
func (c *Containerd) handleListTasks(req Request) Response {
	svc := c.getTaskService()
	if svc == nil {
		return Response{Success: false, Message: "Task 服务未初始化"}
	}
	states, err := svc.List()
	if err != nil {
		return Response{Success: false, Message: err.Error()}
	}
	return Response{Success: true, Data: states}
}

// handlePauseTask 处理暂停任务请求
func (c *Containerd) handlePauseTask(req Request) Response {
	containerID := req.Args["container_id"]
	if containerID == "" {
		return Response{Success: false, Message: "容器 ID 不能为空"}
	}
	svc := c.getTaskService()
	if svc == nil {
		return Response{Success: false, Message: "Task 服务未初始化"}
	}
	if err := svc.Pause(containerID); err != nil {
		return Response{Success: false, Message: err.Error()}
	}
	return Response{Success: true}
}

// handleResumeTask 处理恢复任务请求
func (c *Containerd) handleResumeTask(req Request) Response {
	containerID := req.Args["container_id"]
	if containerID == "" {
		return Response{Success: false, Message: "容器 ID 不能为空"}
	}
	svc := c.getTaskService()
	if svc == nil {
		return Response{Success: false, Message: "Task 服务未初始化"}
	}
	if err := svc.Resume(containerID); err != nil {
		return Response{Success: false, Message: err.Error()}
	}
	return Response{Success: true}
}

// handleAttachTask 处理 attach 请求（流式连接）
// 对齐 Docker: dockerd → containerd → shim 的流式 I/O 通道
func (c *Containerd) handleAttachTask(req Request, conn net.Conn) Response {
	containerID := req.Args["container_id"]
	if containerID == "" {
		return Response{Success: false, Message: "容器 ID 不能为空"}
	}
	svc := c.getTaskService()
	if svc == nil {
		return Response{Success: false, Message: "Task 服务未初始化"}
	}

	shimConn, err := svc.Attach(containerID)
	if err != nil {
		return Response{Success: false, Message: fmt.Sprintf("attach 到容器失败: %v", err)}
	}

	// 先发送响应，然后进入流式转发
	WriteResponse(conn, Response{Success: true, Stream: true})

	// 双向转发: daemon conn ←→ shim conn
	task.RelayStream(conn, shimConn)

	return Response{}
}

// handleExecTaskStream 处理 exec 流式请求
func (c *Containerd) handleExecTaskStream(req Request, conn net.Conn) Response {
	containerID := req.Args["container_id"]
	if containerID == "" {
		return Response{Success: false, Message: "容器 ID 不能为空"}
	}

	tty := req.Args["tty"] == "true"
	var args []string
	if argsJSON := req.Args["args_json"]; argsJSON != "" {
		json.Unmarshal([]byte(argsJSON), &args)
	}

	svc := c.getTaskService()
	if svc == nil {
		return Response{Success: false, Message: "Task 服务未初始化"}
	}

	shimConn, err := svc.ExecStream(containerID, args, tty)
	if err != nil {
		return Response{Success: false, Message: fmt.Sprintf("执行命令失败: %v", err)}
	}

	// 先发送响应，然后进入流式转发
	WriteResponse(conn, Response{Success: true, Stream: true})

	// 双向转发: daemon conn ←→ shim conn
	task.RelayStream(conn, shimConn)

	return Response{}
}

// handleResizeTask 处理调整终端大小请求
func (c *Containerd) handleResizeTask(req Request) Response {
	containerID := req.Args["container_id"]
	if containerID == "" {
		return Response{Success: false, Message: "容器 ID 不能为空"}
	}
	var rows, cols uint16
	fmt.Sscanf(req.Args["rows"], "%d", &rows)
	fmt.Sscanf(req.Args["cols"], "%d", &cols)
	svc := c.getTaskService()
	if svc == nil {
		return Response{Success: false, Message: "Task 服务未初始化"}
	}
	if err := svc.Resize(containerID, rows, cols); err != nil {
		return Response{Success: false, Message: err.Error()}
	}
	return Response{Success: true}
}

// handleIsShimAlive 处理检查 shim 是否存活请求
func (c *Containerd) handleIsShimAlive(req Request) Response {
	containerID := req.Args["container_id"]
	if containerID == "" {
		return Response{Success: false, Message: "容器 ID 不能为空"}
	}
	svc := c.getTaskService()
	if svc == nil {
		return Response{Success: false, Message: "Task 服务未初始化"}
	}
	alive := svc.IsShimAlive(containerID)
	return Response{Success: true, Data: map[string]interface{}{"alive": alive}}
}

// handleReadShimPID 处理读取 shim PID 请求
func (c *Containerd) handleReadShimPID(req Request) Response {
	containerID := req.Args["container_id"]
	if containerID == "" {
		return Response{Success: false, Message: "容器 ID 不能为空"}
	}
	svc := c.getTaskService()
	if svc == nil {
		return Response{Success: false, Message: "Task 服务未初始化"}
	}
	pid := svc.ReadShimPID(containerID)
	return Response{Success: true, Data: map[string]interface{}{"pid": pid}}
}

// handleReadExitInfo 处理读取退出信息请求
func (c *Containerd) handleReadExitInfo(req Request) Response {
	containerID := req.Args["container_id"]
	if containerID == "" {
		return Response{Success: false, Message: "容器 ID 不能为空"}
	}
	svc := c.getTaskService()
	if svc == nil {
		return Response{Success: false, Message: "Task 服务未初始化"}
	}
	info, err := svc.ReadExitInfo(containerID)
	if err != nil {
		return Response{Success: false, Message: fmt.Sprintf("读取退出信息失败: %v", err)}
	}
	return Response{Success: true, Data: info}
}
