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

=======================================================================
*/

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"

	"mini-docker/constants"
	"mini-docker/containerstore"
	"mini-docker/libcontainer"
	"mini-docker/spec"
	"mini-docker/types"
)

// handleCreateTask 处理创建容器任务请求
// 对齐 Docker: dockerd → containerd.CreateTask → 启动 shim + runtime create
func (c *Containerd) handleCreateTask(req Request) Response {
	containerID := req.Args["container_id"]
	if containerID == "" {
		return Response{Success: false, Message: "容器 ID 不能为空"}
	}

	info, err := containerstore.LoadContainerInfoByID(containerID)
	if err != nil {
		return Response{Success: false, Message: fmt.Sprintf("加载容器信息失败: %v", err)}
	}

	bundlePath := filepath.Join(constants.RuntimeDir, info.ID, "bundle")
	ociSpec := buildOCISpec(info)
	if err := spec.SaveSpec(ociSpec, bundlePath); err != nil {
		return Response{Success: false, Message: fmt.Sprintf("保存 config.json 失败: %v", err)}
	}

	shimArgs := []string{"shim", info.ID, bundlePath}
	if info.Tty {
		shimArgs = append(shimArgs, "--tty")
	}
	cmd := newShimCommand(shimArgs)

	logDir := filepath.Join(filepath.Dir(constants.DaemonLogPath), "shim")
	if err := os.MkdirAll(logDir, 0755); err != nil {
		return Response{Success: false, Message: fmt.Sprintf("创建 shim 日志目录失败: %v", err)}
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
		return Response{Success: false, Message: fmt.Sprintf("启动 shim 失败: %v", err)}
	}

	if logFile != nil {
		logFile.Close()
	}

	shimPID := cmd.Process.Pid

	socketPath := filepath.Join(constants.ShimDir, info.ID, "shim.sock")
	if err := waitForSocket(socketPath, constants.SocketWaitTimeout); err != nil {
		cmd.Process.Kill()
		cmd.Wait()
		return Response{Success: false, Message: fmt.Sprintf("shim socket 未就绪: %v", err)}
	}

	return Response{Success: true, Data: map[string]interface{}{"shim_pid": shimPID}}
}

// handleStartTask 处理启动容器任务请求
func (c *Containerd) handleStartTask(req Request) Response {
	containerID := req.Args["container_id"]
	if containerID == "" {
		return Response{Success: false, Message: "容器 ID 不能为空"}
	}
	if err := shimCall(containerID, types.ShimRequest{Type: "start"}); err != nil {
		return Response{Success: false, Message: fmt.Sprintf("启动任务失败: %v", err)}
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
	sig, err := strconv.Atoi(signalStr)
	if err != nil {
		return Response{Success: false, Message: fmt.Sprintf("无效信号: %s", signalStr)}
	}
	if err := shimCall(containerID, types.ShimRequest{Type: "kill", Signal: sig}); err != nil {
		return Response{Success: false, Message: fmt.Sprintf("发送信号失败: %v", err)}
	}
	return Response{Success: true}
}

// handleDeleteTask 处理删除容器任务请求
func (c *Containerd) handleDeleteTask(req Request) Response {
	containerID := req.Args["container_id"]
	if containerID == "" {
		return Response{Success: false, Message: "容器 ID 不能为空"}
	}
	if err := deleteTask(containerID); err != nil {
		return Response{Success: false, Message: fmt.Sprintf("删除任务失败: %v", err)}
	}
	return Response{Success: true}
}

// handleShutdownShim 处理关闭 shim 请求
func (c *Containerd) handleShutdownShim(req Request) Response {
	containerID := req.Args["container_id"]
	if containerID == "" {
		return Response{Success: false, Message: "容器 ID 不能为空"}
	}
	shutdownShim(containerID)
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
	shimPID, err := restartShim(containerID, containerPID)
	if err != nil {
		return Response{Success: false, Message: fmt.Sprintf("重启 shim 失败: %v", err)}
	}
	return Response{Success: true, Data: map[string]interface{}{"shim_pid": shimPID}}
}

// handleGetTaskState 处理获取任务状态请求
func (c *Containerd) handleGetTaskState(req Request) Response {
	containerID := req.Args["container_id"]
	if containerID == "" {
		return Response{Success: false, Message: "容器 ID 不能为空"}
	}

	conn, err := connectShim(containerID)
	if err != nil {
		state, loadErr := libcontainer.LoadContainerState(containerID)
		if loadErr != nil {
			return Response{Success: false, Message: fmt.Sprintf("获取任务状态失败: %v", loadErr)}
		}
		return Response{Success: true, Data: state}
	}
	conn.Close()

	var state libcontainer.ContainerState
	if err := shimCallWithData(containerID, types.ShimRequest{Type: "state"}, &state); err != nil {
		return Response{Success: false, Message: fmt.Sprintf("获取任务状态失败: %v", err)}
	}
	return Response{Success: true, Data: state}
}

// handleGetExitInfo 处理获取退出信息请求
func (c *Containerd) handleGetExitInfo(req Request) Response {
	containerID := req.Args["container_id"]
	if containerID == "" {
		return Response{Success: false, Message: "容器 ID 不能为空"}
	}

	conn, err := connectShim(containerID)
	if err != nil {
		info, readErr := readExitInfoFromFile(containerID)
		if readErr != nil {
			return Response{Success: false, Message: fmt.Sprintf("获取退出信息失败: %v", readErr)}
		}
		return Response{Success: true, Data: info}
	}
	conn.Close()

	var info ExitInfo
	if err := shimCallWithData(containerID, types.ShimRequest{Type: "exit_info"}, &info); err != nil {
		return Response{Success: false, Message: fmt.Sprintf("获取退出信息失败: %v", err)}
	}
	return Response{Success: true, Data: &info}
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

	pid, err := waitForCreate(containerID, timeout)
	if err != nil {
		return Response{Success: false, Message: fmt.Sprintf("等待容器创建失败: %v", err)}
	}
	return Response{Success: true, Data: map[string]interface{}{"pid": pid}}
}

// handleListTasks 处理列出所有任务请求
func (c *Containerd) handleListTasks(req Request) Response {
	states, err := libcontainer.ListContainerStates()
	if err != nil {
		return Response{Success: false, Message: fmt.Sprintf("列出任务失败: %v", err)}
	}

	for _, state := range states {
		if state.Status == libcontainer.StatusRunning || state.Status == libcontainer.StatusCreated {
			proc, err := os.FindProcess(state.Pid)
			if err != nil || proc.Signal(syscall.Signal(0)) != nil {
				state.Status = libcontainer.StatusStopped
			}
		}
	}
	return Response{Success: true, Data: states}
}

// handlePauseTask 处理暂停任务请求
func (c *Containerd) handlePauseTask(req Request) Response {
	containerID := req.Args["container_id"]
	if containerID == "" {
		return Response{Success: false, Message: "容器 ID 不能为空"}
	}
	if err := shimCall(containerID, types.ShimRequest{Type: "pause"}); err != nil {
		return Response{Success: false, Message: fmt.Sprintf("暂停容器失败: %v", err)}
	}
	return Response{Success: true}
}

// handleResumeTask 处理恢复任务请求
func (c *Containerd) handleResumeTask(req Request) Response {
	containerID := req.Args["container_id"]
	if containerID == "" {
		return Response{Success: false, Message: "容器 ID 不能为空"}
	}
	if err := shimCall(containerID, types.ShimRequest{Type: "unpause"}); err != nil {
		return Response{Success: false, Message: fmt.Sprintf("恢复容器失败: %v", err)}
	}
	return Response{Success: true}
}

// handleAttachTask 处理 attach 请求（流式连接）
// 对齐 Docker: dockerd → containerd → shim 的流式 I/O 通道
// 流式请求处理流程：
//  1. containerd 连接到 shim 的 attach 接口，获取 shimConn
//  2. containerd 向 daemon 发送 JSON 响应（stream=true）
//  3. containerd 在 conn 和 shimConn 之间双向转发字节流
//  4. 任意一侧断开，转发结束
func (c *Containerd) handleAttachTask(req Request, conn net.Conn) Response {
	containerID := req.Args["container_id"]
	if containerID == "" {
		return Response{Success: false, Message: "容器 ID 不能为空"}
	}

	shimConn, err := attachToShim(containerID)
	if err != nil {
		return Response{Success: false, Message: fmt.Sprintf("attach 到容器失败: %v", err)}
	}

	// 先发送响应，然后进入流式转发
	WriteResponse(conn, Response{Success: true, Stream: true})

	// 双向转发: daemon conn ←→ shim conn
	var once sync.Once
	done := make(chan struct{})
	go func() {
		defer once.Do(func() { close(done) })
		_, _ = ioCopy(shimConn, conn)
	}()
	go func() {
		defer once.Do(func() { close(done) })
		_, _ = ioCopy(conn, shimConn)
	}()
	<-done
	conn.Close()
	shimConn.Close()

	// 返回空响应表示已处理完毕（流式请求不走常规返回路径）
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

	shimConn, err := execTaskStream(containerID, args, tty)
	if err != nil {
		return Response{Success: false, Message: fmt.Sprintf("执行命令失败: %v", err)}
	}

	// 先发送响应，然后进入流式转发
	WriteResponse(conn, Response{Success: true, Stream: true})

	// 双向转发: daemon conn ←→ shim conn
	var once sync.Once
	done := make(chan struct{})
	go func() {
		defer once.Do(func() { close(done) })
		_, _ = ioCopy(shimConn, conn)
	}()
	go func() {
		defer once.Do(func() { close(done) })
		_, _ = ioCopy(conn, shimConn)
	}()
	<-done
	conn.Close()
	shimConn.Close()

	return Response{}
}

// ioCopy 是 io.Copy 的别名，避免导入 io 包与本地变量冲突
func ioCopy(dst net.Conn, src net.Conn) (int64, error) {
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

// handleResizeTask 处理调整终端大小请求
func (c *Containerd) handleResizeTask(req Request) Response {
	containerID := req.Args["container_id"]
	if containerID == "" {
		return Response{Success: false, Message: "容器 ID 不能为空"}
	}
	var rows, cols uint16
	fmt.Sscanf(req.Args["rows"], "%d", &rows)
	fmt.Sscanf(req.Args["cols"], "%d", &cols)
	if err := shimCall(containerID, types.ShimRequest{Type: "resize", Rows: rows, Cols: cols}); err != nil {
		return Response{Success: false, Message: fmt.Sprintf("调整窗口大小失败: %v", err)}
	}
	return Response{Success: true}
}

// handleIsShimAlive 处理检查 shim 是否存活请求
func (c *Containerd) handleIsShimAlive(req Request) Response {
	containerID := req.Args["container_id"]
	if containerID == "" {
		return Response{Success: false, Message: "容器 ID 不能为空"}
	}
	alive := isShimAlive(containerID)
	return Response{Success: true, Data: map[string]interface{}{"alive": alive}}
}

// handleReadShimPID 处理读取 shim PID 请求
func (c *Containerd) handleReadShimPID(req Request) Response {
	containerID := req.Args["container_id"]
	if containerID == "" {
		return Response{Success: false, Message: "容器 ID 不能为空"}
	}
	pid := readShimPID(containerID)
	return Response{Success: true, Data: map[string]interface{}{"pid": pid}}
}

// handleReadExitInfo 处理读取退出信息请求
func (c *Containerd) handleReadExitInfo(req Request) Response {
	containerID := req.Args["container_id"]
	if containerID == "" {
		return Response{Success: false, Message: "容器 ID 不能为空"}
	}
	info, err := readExitInfoFromFile(containerID)
	if err != nil {
		return Response{Success: false, Message: fmt.Sprintf("读取退出信息失败: %v", err)}
	}
	return Response{Success: true, Data: info}
}
