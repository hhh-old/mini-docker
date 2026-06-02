//go:build linux

package containerd

/*
=======================================================================
  containerd 独立进程的服务端（对齐真实 Docker 的 containerd 架构）

  真实 Docker 的架构：
  ┌──────────┐    gRPC/Socket    ┌──────────────┐    exec    ┌──────────────┐
  │ dockerd  │ ──────────────→  │  containerd  │ ────────→ │ containerd-  │
  │ (Daemon) │  /run/           │  (独立进程)    │           │ shim         │
  └──────────┘  containerd.sock └──────────────┘           └──────────────┘
                                          │
                                          │ exec
                                          ▼
                                   ┌──────────────┐
                                   │    runc      │
                                   └──────────────┘

  mini-docker 的对齐架构：
  ┌──────────┐    JSON/Socket    ┌──────────────┐    exec    ┌──────────────┐
  │ Daemon   │ ──────────────→  │  containerd  │ ────────→ │    shim      │
  │ (client) │  /var/run/       │  (独立进程)    │           │              │
  └──────────┘  containerd.sock └──────────────┘           └──────────────┘
                                          │                        │
                                          │ /proc/self/exe         │ /proc/self/exe
                                          ▼                        ▼
                                   ┌──────────────┐        ┌──────────────┐
                                   │   runtime    │        │  容器 init   │
                                   └──────────────┘        └──────────────┘

  containerd 进程的核心职责（对齐真实 Docker）：
  1. 管理 shim 进程的创建、通信和销毁
  2. 管理 OCI Spec 的生成和存储
  3. 提供 Task 生命周期 API（create/start/kill/delete/pause/resume）
  4. 代理 Daemon 对 shim 的所有操作
  5. 维护 Task 列表和状态

=======================================================================
*/

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"mini-docker/constants"
	"mini-docker/containerstore"
	"mini-docker/libcontainer"
	"mini-docker/spec"
	"mini-docker/types"
	"mini-docker/utils"
)

// ---------------------------------------------------------------------------
// containerd 进程主体
// ---------------------------------------------------------------------------

const (
	runtimeDir = constants.RuntimeDir
	shimDir    = constants.ShimDir
)

// Containerd containerd 独立进程主体
// 对齐 Docker: containerd 作为一个独立守护进程运行，有自己的事件循环和状态管理
type Containerd struct {
	mu       sync.RWMutex
	listener net.Listener
	shutdown chan struct{}
}

// NewContainerd 创建 containerd 实例
func NewContainerd() *Containerd {
	return &Containerd{
		shutdown: make(chan struct{}),
	}
}

// Start 启动 containerd 独立进程
// 对齐 Docker: containerd 通过 systemd 或手动启动，监听自己的 Unix Socket
func (c *Containerd) Start() error {
	if IsContainerdRunning() {
		return fmt.Errorf("containerd 已在运行，请先停止")
	}

	for _, dir := range []string{
		constants.MiniDockerRunRoot,
		filepath.Dir(constants.ContainerdLogPath),
		runtimeDir,
		shimDir,
	} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return fmt.Errorf("创建目录 %s 失败: %w", dir, err)
		}
	}

	os.Remove(ContainerdSocketPath)

	l, err := net.Listen("unix", ContainerdSocketPath)
	if err != nil {
		return fmt.Errorf("监听 Unix Socket 失败: %w", err)
	}
	c.listener = l
	os.Chmod(ContainerdSocketPath, 0666)

	log.Printf("containerd 启动成功，监听 %s (PID: %d)\n", ContainerdSocketPath, os.Getpid())

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigCh
		log.Printf("收到信号 %v，开始优雅关闭...\n", sig)
		c.Stop()
		os.Exit(0)
	}()

	go c.acceptLoop()

	return nil
}

// Stop 优雅关闭 containerd
func (c *Containerd) Stop() {
	close(c.shutdown)
	if c.listener != nil {
		c.listener.Close()
	}
	os.Remove(ContainerdSocketPath)
	log.Println("containerd 已停止")
}

// acceptLoop 接受 Daemon 的连接
func (c *Containerd) acceptLoop() {
	for {
		select {
		case <-c.shutdown:
			return
		default:
		}

		conn, err := c.listener.Accept()
		if err != nil {
			select {
			case <-c.shutdown:
				return
			default:
				log.Printf("接受连接失败: %v\n", err)
				time.Sleep(100 * time.Millisecond)
				continue
			}
		}

		go c.handleConnection(conn)
	}
}

// isStreamRequest 判断请求是否为流式请求
// 流式请求在 routeRequest 内部自行管理连接生命周期，不需要外部关闭
func isStreamRequest(reqType string) bool {
	return reqType == ReqAttachTask || reqType == ReqExecTaskStream
}

// handleConnection 处理单个 Daemon 连接
// 对齐 Docker: containerd 的每个 gRPC 请求都是独立的
// 流式请求（attach/exec）在发送响应后进入双向转发，连接由转发逻辑管理
func (c *Containerd) handleConnection(conn net.Conn) {
	var req Request
	if err := json.NewDecoder(conn).Decode(&req); err != nil {
		WriteResponse(conn, Response{Success: false, Message: fmt.Sprintf("解析请求失败: %v", err)})
		conn.Close()
		return
	}

	// 流式请求在 routeRequest 内部完成响应发送和双向转发
	if isStreamRequest(req.Type) {
		c.routeRequest(req, conn)
		return
	}

	resp := c.routeRequest(req, conn)
	WriteResponse(conn, resp)
	conn.Close()
}

// routeRequest 路由请求到对应处理器
func (c *Containerd) routeRequest(req Request, conn net.Conn) Response {
	switch req.Type {
	case ReqCreateTask:
		return c.handleCreateTask(req)
	case ReqStartTask:
		return c.handleStartTask(req)
	case ReqKillTask:
		return c.handleKillTask(req)
	case ReqDeleteTask:
		return c.handleDeleteTask(req)
	case ReqShutdownShim:
		return c.handleShutdownShim(req)
	case ReqRestartShim:
		return c.handleRestartShim(req)
	case ReqGetTaskState:
		return c.handleGetTaskState(req)
	case ReqGetExitInfo:
		return c.handleGetExitInfo(req)
	case ReqWaitForCreate:
		return c.handleWaitForCreate(req)
	case ReqListTasks:
		return c.handleListTasks(req)
	case ReqPauseTask:
		return c.handlePauseTask(req)
	case ReqResumeTask:
		return c.handleResumeTask(req)
	case ReqAttachTask:
		return c.handleAttachTask(req, conn)
	case ReqExecTaskStream:
		return c.handleExecTaskStream(req, conn)
	case ReqResizeTask:
		return c.handleResizeTask(req)
	case ReqIsShimAlive:
		return c.handleIsShimAlive(req)
	case ReqReadShimPID:
		return c.handleReadShimPID(req)
	case ReqReadExitInfo:
		return c.handleReadExitInfo(req)
	case ReqPing:
		return Response{Success: true, Message: "pong"}
	default:
		return Response{Success: false, Message: fmt.Sprintf("未知请求类型: %s", req.Type)}
	}
}

// ---------------------------------------------------------------------------
// Task 生命周期处理器
// ---------------------------------------------------------------------------

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

	bundlePath := filepath.Join(runtimeDir, info.ID, "bundle")
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

	socketPath := filepath.Join(shimDir, info.ID, "shim.sock")
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

// ---------------------------------------------------------------------------
// 内部实现函数（从原 service.go 迁移）
// ---------------------------------------------------------------------------

// ExitInfo 退出信息类型别名
type ExitInfo = types.ExitInfo

func resolveShimDir(containerID string) string {
	return filepath.Join(shimDir, containerID)
}

func connectShim(containerID string) (net.Conn, error) {
	shimContainerDir := resolveShimDir(containerID)
	socketPath := filepath.Join(shimContainerDir, "shim.sock")
	conn, err := net.DialTimeout("unix", socketPath, 5*time.Second)
	if err != nil {
		return nil, fmt.Errorf("连接 shim 失败: %w", err)
	}
	return conn, nil
}

func shimCall(containerID string, req types.ShimRequest) error {
	conn, err := connectShim(containerID)
	if err != nil {
		return err
	}
	defer conn.Close()

	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return fmt.Errorf("发送%s请求失败: %w", req.Type, err)
	}

	var resp types.ShimResponse
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		return fmt.Errorf("读取%s响应失败: %w", req.Type, err)
	}
	if !resp.Success {
		return fmt.Errorf("%s失败: %s", req.Type, resp.Message)
	}
	return nil
}

func shimCallWithData(containerID string, req types.ShimRequest, result any) error {
	conn, err := connectShim(containerID)
	if err != nil {
		return err
	}
	defer conn.Close()

	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return fmt.Errorf("发送%s请求失败: %w", req.Type, err)
	}

	var resp types.ShimResponse
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		return fmt.Errorf("读取%s响应失败: %w", req.Type, err)
	}
	if !resp.Success {
		return fmt.Errorf("%s失败: %s", req.Type, resp.Message)
	}

	data, _ := json.Marshal(resp.Data)
	if err := json.Unmarshal(data, result); err != nil {
		return fmt.Errorf("解析%s数据失败: %w", req.Type, err)
	}
	return nil
}

func readExitInfoFromFile(containerID string) (*ExitInfo, error) {
	shimContainerDir := resolveShimDir(containerID)
	exitPath := filepath.Join(shimContainerDir, "exit.json")
	data, err := os.ReadFile(exitPath)
	if err != nil {
		return nil, fmt.Errorf("读取退出信息失败: %w", err)
	}
	var info ExitInfo
	if err := json.Unmarshal(data, &info); err != nil {
		return nil, fmt.Errorf("解析退出信息失败: %w", err)
	}
	return &info, nil
}

func waitForSocket(path string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("unix", path, constants.ShimConnectTimeout)
		if err == nil {
			conn.Close()
			return nil
		}
		time.Sleep(constants.PollInterval)
	}
	return fmt.Errorf("等待 socket %s 超时", path)
}

func buildOCISpec(info *containerstore.ContainerInfo) *spec.Spec {
	return spec.DefaultSpec(&spec.SpecConfig{
		Tty:           info.Tty,
		Memory:        info.Memory,
		CPUShares:     info.CPUShares,
		Image:         info.Image,
		RootFS:        info.RootFS,
		Cmd:           info.Cmd,
		Volumes:       info.Volumes,
		Hostname:      info.Name,
		Network:       info.Network,
		RestartPolicy: info.RestartPolicy,
		OverlayMerged: info.OverlayMerged,
		OverlayUpper:  info.OverlayUpper,
		OverlayWork:   info.OverlayWork,
		PortMap:       info.PortMap,
		CgroupName:    info.CgroupName,
	})
}

func isShimAlive(containerID string) bool {
	shimContainerDir := resolveShimDir(containerID)
	socketPath := filepath.Join(shimContainerDir, "shim.sock")
	conn, err := net.DialTimeout("unix", socketPath, constants.ShimConnectTimeout)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

func readShimPID(containerID string) int {
	pidPath := filepath.Join(resolveShimDir(containerID), "shim.pid")
	data, err := os.ReadFile(pidPath)
	if err != nil {
		return 0
	}
	var pid int
	fmt.Sscanf(strings.TrimSpace(string(data)), "%d", &pid)
	return pid
}

func deleteTask(containerID string) error {
	conn, err := connectShim(containerID)
	if err == nil {
		json.NewEncoder(conn).Encode(types.ShimRequest{Type: "shutdown"})
		conn.Close()
		shimPID := readShimPID(containerID)
		if shimPID > 0 {
			exited := false
			for i := 0; i < 30; i++ {
				if proc, e := os.FindProcess(shimPID); e == nil {
					if proc.Signal(syscall.Signal(0)) != nil {
						exited = true
						break
					}
				} else {
					exited = true
					break
				}
				time.Sleep(100 * time.Millisecond)
			}
			if !exited {
				if proc, e := os.FindProcess(shimPID); e == nil {
					proc.Signal(syscall.SIGKILL)
					for i := 0; i < 50; i++ {
						if proc.Signal(syscall.Signal(0)) != nil {
							break
						}
						time.Sleep(100 * time.Millisecond)
					}
				}
			}
		} else {
			time.Sleep(2 * time.Second)
		}
	} else {
		shimPID := readShimPID(containerID)
		if shimPID > 0 {
			if proc, e := os.FindProcess(shimPID); e == nil {
				proc.Signal(syscall.SIGKILL)
				for i := 0; i < 50; i++ {
					if proc.Signal(syscall.Signal(0)) != nil {
						break
					}
					time.Sleep(100 * time.Millisecond)
				}
			}
		}
	}

	containerPID := 0
	if info, err := readExitInfoFromFile(containerID); err != nil || info == nil {
		createdPath := filepath.Join(resolveShimDir(containerID), "created")
		if data, err := os.ReadFile(createdPath); err == nil {
			fmt.Sscanf(strings.TrimSpace(string(data)), "%d", &containerPID)
		}
	}

	if containerPID > 0 && utils.CheckProcessAlive(containerPID) {
		if proc, err := os.FindProcess(containerPID); err == nil {
			proc.Signal(syscall.SIGKILL)
			for i := 0; i < 50; i++ {
				if !utils.CheckProcessAlive(containerPID) {
					break
				}
				time.Sleep(100 * time.Millisecond)
			}
		}
	}

	stateDir := filepath.Join(runtimeDir, containerID)
	os.RemoveAll(stateDir)

	shimContainerDir := resolveShimDir(containerID)
	os.RemoveAll(shimContainerDir)

	return nil
}

func shutdownShim(containerID string) {
	conn, err := connectShim(containerID)
	if err != nil {
		return
	}
	json.NewEncoder(conn).Encode(types.ShimRequest{Type: "shutdown"})
	conn.Close()
	time.Sleep(constants.ShutdownWaitTime)
}

func restartShim(containerID string, containerPID int) (int, error) {
	bundlePath := filepath.Join(runtimeDir, containerID, "bundle")
	if _, err := os.Stat(bundlePath); err != nil {
		return 0, fmt.Errorf("bundle 目录不存在: %w", err)
	}

	shimContainerDir := resolveShimDir(containerID)
	os.Remove(filepath.Join(shimContainerDir, "shim.sock"))
	os.Remove(filepath.Join(shimContainerDir, "shim.pid"))
	os.Remove(filepath.Join(shimContainerDir, "created"))
	os.Remove(filepath.Join(shimContainerDir, "exit.json"))

	cmd := newShimCommand([]string{"shim", containerID, bundlePath, "--takeover", fmt.Sprintf("%d", containerPID)})

	logDir := filepath.Join(filepath.Dir(constants.DaemonLogPath), "shim")
	logPath := filepath.Join(logDir, containerID+".log")
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

	socketPath := filepath.Join(shimContainerDir, "shim.sock")
	if err := waitForSocket(socketPath, constants.SocketWaitTimeout); err != nil {
		cmd.Process.Kill()
		cmd.Wait()
		return 0, fmt.Errorf("shim socket 未就绪: %w", err)
	}

	return shimPID, nil
}

func waitForCreate(containerID string, timeout time.Duration) (int, error) {
	shimContainerDir := resolveShimDir(containerID)
	createdPath := filepath.Join(shimContainerDir, "created")

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(createdPath)
		if err == nil {
			pid, _ := strconv.Atoi(strings.TrimSpace(string(data)))
			if pid > 0 {
				return pid, nil
			}
		}
		time.Sleep(constants.PollInterval)
	}
	return 0, fmt.Errorf("等待容器 %s 创建超时", containerID)
}

func attachToShim(containerID string) (net.Conn, error) {
	conn, err := connectShim(containerID)
	if err != nil {
		return nil, err
	}

	req := types.ShimRequest{Type: "attach"}
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		conn.Close()
		return nil, fmt.Errorf("发送 attach 请求失败: %w", err)
	}

	var resp types.ShimResponse
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		conn.Close()
		return nil, fmt.Errorf("读取 attach 响应失败: %w", err)
	}
	if !resp.Success {
		conn.Close()
		return nil, fmt.Errorf("attach 失败: %s", resp.Message)
	}
	return conn, nil
}

func execTaskStream(containerID string, args []string, tty bool) (net.Conn, error) {
	conn, err := connectShim(containerID)
	if err != nil {
		return nil, err
	}

	req := types.ShimRequest{Type: "exec", Args: args, Tty: tty}
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		conn.Close()
		return nil, fmt.Errorf("发送请求失败: %w", err)
	}

	var resp types.ShimResponse
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		conn.Close()
		return nil, fmt.Errorf("读取响应失败: %w", err)
	}
	if !resp.Success {
		conn.Close()
		return nil, fmt.Errorf("%s", resp.Message)
	}

	return conn, nil
}

// newShimCommand 创建 shim 进程命令
// containerd 独立进程后，使用 /proc/self/exe 仍然可行（同一个二进制）
func newShimCommand(args []string) *exec.Cmd {
	cmd := exec.Command("/proc/self/exe", args...)
	cmd.SysProcAttr = newShimSysProcAttr()
	return cmd
}

// ---------------------------------------------------------------------------
// containerd 进程管理（供 Daemon 调用的包级函数）
// ---------------------------------------------------------------------------

// IsContainerdRunning 检查 containerd 是否已在运行
func IsContainerdRunning() bool {
	conn, err := net.DialTimeout("unix", ContainerdSocketPath, time.Second)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// WaitForContainerd 等待 containerd 进程就绪（Daemon 启动时调用）
func WaitForContainerd(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("unix", ContainerdSocketPath, time.Second)
		if err == nil {
			conn.Close()
			return nil
		}
		time.Sleep(constants.PollInterval)
	}
	return fmt.Errorf("等待 containerd 就绪超时")
}
