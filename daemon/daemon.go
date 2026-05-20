package daemon

/*
=======================================================================
  Daemon 进程模型 —— 对齐 Docker 的 dockerd 架构
=======================================================================

  真实 Docker 的架构：
  ┌──────────┐    Unix Socket    ┌──────────┐    API    ┌──────────────┐
  │ docker   │ ──────────────→  │ dockerd  │ ──────→  │ containerd   │
  │ CLI      │  /var/run/       │ Daemon   │          │ → shim → runc│
  └──────────┘  docker.sock     └──────────┘          └──────────────┘

  mini-docker 的对齐架构：
  ┌──────────┐    Unix Socket    ┌──────────────┐
  │ mini-    │ ──────────────→  │ mini-docker   │
  │ docker   │  /var/run/       │ daemon        │
  │ CLI      │  mini-docker.sock│ (管理所有容器) │
  └──────────┘                  └──────────────┘

  核心改进：
  1. CLI 与容器管理解耦 —— CLI 可退出，Daemon 持续管理
  2. 后台容器 (-d) 可靠运行 —— Daemon 的 event loop 感知退出
  3. 支持重启策略 —— Daemon 负责重启崩溃的容器
  4. 统一事件源 —— 所有容器事件由 Daemon 收集和分发

=======================================================================
*/

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

const (
	SocketPath    = "/var/run/mini-docker/mini-docker.sock"
	DaemonPidFile = "/var/run/mini-docker/daemon.pid"
	DaemonLogPath = "/var/log/mini-docker/daemon.log"
)

// Daemon 守护进程主体
type Daemon struct {
	mu         sync.RWMutex
	containers map[string]*ContainerState // 运行中的容器状态
	listener   net.Listener
	eventBus   *EventBus
	shutdown   chan struct{}
}

// ContainerState 运行中容器的运行时状态（仅 Daemon 持有）
type ContainerState struct {
	ID        string
	Pid       int
	Cmd       *os.Process
	CgroupMgr interface {
		Apply(pid int) error
		Destroy() error
		Freeze() error
		Unfreeze() error
	}
	NetMgr interface {
		Connect(pid int) error
		Disconnect() error
		GetVethHost() string
		GetContainerIP() string
	}
	CreatedAt     time.Time
	RestartPolicy string // "no", "always", "on-failure"
	RestartCount  int
}

// NewDaemon 创建 Daemon 实例
func NewDaemon() *Daemon {
	return &Daemon{
		containers: make(map[string]*ContainerState),
		eventBus:   NewEventBus(),
		shutdown:   make(chan struct{}),
	}
}

// Start 启动 Daemon，监听 Unix Socket
func (d *Daemon) Start() error {
	// 检查是否已有 Daemon 运行
	if IsRunning() {
		return fmt.Errorf("Daemon 已在运行 (PID: %s)，请先停止", readPidFile())
	}

	// 创建必要目录
	for _, dir := range []string{
		filepath.Dir(SocketPath),
		filepath.Dir(DaemonLogPath),
		"/var/run/mini-docker",
		"/var/log/mini-docker",
	} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return fmt.Errorf("创建目录 %s 失败: %w", dir, err)
		}
	}

	// 清理旧 Socket 文件
	os.Remove(SocketPath)

	// 写入 PID 文件
	if err := os.WriteFile(DaemonPidFile, []byte(fmt.Sprintf("%d", os.Getpid())), 0644); err != nil {
		return fmt.Errorf("写入 PID 文件失败: %w", err)
	}

	// 监听 Unix Socket
	l, err := net.Listen("unix", SocketPath)
	if err != nil {
		return fmt.Errorf("监听 Unix Socket 失败: %w", err)
	}
	d.listener = l

	// 设置 Socket 文件权限（允许同组用户访问）
	os.Chmod(SocketPath, 0660)

	log.Printf("Daemon 启动成功，监听 %s (PID: %d)\n", SocketPath, os.Getpid())

	// 恢复已有容器的管理（Daemon 重启后）
	d.restoreContainers()

	// 启动事件总线
	go d.eventBus.Run()

	// 信号处理
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigCh
		log.Printf("收到信号 %v，开始优雅关闭...\n", sig)
		d.Stop()
		os.Exit(0)
	}()

	// 主循环：接受连接
	go d.acceptLoop()

	return nil
}

// Stop 优雅关闭 Daemon
func (d *Daemon) Stop() {
	close(d.shutdown)
	if d.listener != nil {
		d.listener.Close()
	}
	os.Remove(SocketPath)
	os.Remove(DaemonPidFile)
	log.Println("Daemon 已停止")
}

// acceptLoop 接受客户端连接
func (d *Daemon) acceptLoop() {
	for {
		select {
		case <-d.shutdown:
			return
		default:
		}

		conn, err := d.listener.Accept()
		if err != nil {
			select {
			case <-d.shutdown:
				return
			default:
				log.Printf("接受连接失败: %v\n", err)
				continue
			}
		}

		go d.handleConnection(conn)
	}
}

// handleConnection 处理单个客户端连接
func (d *Daemon) handleConnection(conn net.Conn) {
	defer conn.Close()

	// 读取请求
	buf := make([]byte, 65536) // 64KB 缓冲区
	n, err := conn.Read(buf)
	if err != nil {
		log.Printf("读取请求失败: %v\n", err)
		return
	}

	var req Request
	if err := json.Unmarshal(buf[:n], &req); err != nil {
		d.sendError(conn, fmt.Sprintf("解析请求失败: %v", err))
		return
	}

	// 路由到对应处理器
	resp := d.routeRequest(req)

	// 发送响应
	d.sendResponse(conn, resp)
}

// routeRequest 路由请求到对应处理器
func (d *Daemon) routeRequest(req Request) Response {
	switch req.Type {
	case "run":
		return d.handleRun(req)
	case "stop":
		return d.handleStop(req)
	case "start":
		return d.handleStart(req)
	case "pause":
		return d.handlePause(req)
	case "unpause":
		return d.handleUnpause(req)
	case "rm":
		return d.handleRm(req)
	case "ps":
		return d.handlePs(req)
	case "exec":
		return d.handleExec(req)
	case "logs":
		return d.handleLogs(req)
	case "events":
		return d.handleEvents(req)
	case "images":
		return d.handleImages(req)
	case "pull":
		return d.handlePull(req)
	case "rmi":
		return d.handleRmi(req)
	case "network_create":
		return d.handleNetworkCreate(req)
	case "network_list":
		return d.handleNetworkList(req)
	case "network_delete":
		return d.handleNetworkDelete(req)
	case "ping":
		return Response{Success: true, Message: "pong"}
	default:
		return Response{Success: false, Message: fmt.Sprintf("未知请求类型: %s", req.Type)}
	}
}

// sendResponse 发送响应
func (d *Daemon) sendResponse(conn net.Conn, resp Response) {
	data, err := json.Marshal(resp)
	if err != nil {
		log.Printf("序列化响应失败: %v\n", err)
		return
	}
	conn.Write(data)
}

// sendError 发送错误响应
func (d *Daemon) sendError(conn net.Conn, msg string) {
	d.sendResponse(conn, Response{Success: false, Message: msg})
}

// RegisterContainer 注册运行中的容器到 Daemon
func (d *Daemon) RegisterContainer(state *ContainerState) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.containers[state.ID] = state
	d.eventBus.Publish(Event{
		Type:      "container_start",
		Container: state.ID,
		Time:      time.Now(),
	})
}

// UnregisterContainer 从 Daemon 注销容器
func (d *Daemon) UnregisterContainer(containerID string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.containers, containerID)
}

// GetContainerState 获取容器运行时状态
func (d *Daemon) GetContainerState(containerID string) (*ContainerState, bool) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	s, ok := d.containers[containerID]
	return s, ok
}

// WatchContainer 监控容器进程退出（在 goroutine 中调用）
func (d *Daemon) WatchContainer(state *ContainerState) {
	// 等待容器进程退出
	_, err := state.Cmd.Wait()

	exitCode := 0
	if err != nil {
		exitCode = 1 // 非正常退出
	}

	d.eventBus.Publish(Event{
		Type:      "container_exit",
		Container: state.ID,
		ExitCode:  exitCode,
		Time:      time.Now(),
	})

	// 处理重启策略
	if state.RestartPolicy != "no" {
		if state.RestartPolicy == "always" ||
			(state.RestartPolicy == "on-failure" && exitCode != 0) {
			log.Printf("容器 %s 退出 (exit=%d)，根据重启策略 %s 准备重启\n",
				state.ID[:12], exitCode, state.RestartPolicy)
			d.handleRestart(state, exitCode)
			return
		}
	}

	// 清理容器
	d.cleanupExitedContainer(state.ID)
}

// handleRestart 处理容器重启
func (d *Daemon) handleRestart(state *ContainerState, exitCode int) {
	d.mu.Lock()
	delete(d.containers, state.ID)
	d.mu.Unlock()

	// 更新容器状态为 restarting
	updateContainerStatus(state.ID, "restarting")

	// 重新创建容器（使用相同配置）
	req := Request{
		Type: "run",
		Args: map[string]string{
			"image":          getContainerImage(state.ID),
			"restart_policy": state.RestartPolicy,
		},
	}
	resp := d.handleRun(req)
	if resp.Success {
		log.Printf("容器 %s 重启成功\n", state.ID[:12])
	} else {
		log.Printf("容器 %s 重启失败: %s\n", state.ID[:12], resp.Message)
	}
}

// cleanupExitedContainer 清理已退出的容器
func (d *Daemon) cleanupExitedContainer(containerID string) {
	d.mu.Lock()
	delete(d.containers, containerID)
	d.mu.Unlock()

	// 更新容器状态
	updateContainerStatus(containerID, "exited")
}

// restoreContainers Daemon 重启后恢复容器管理
func (d *Daemon) restoreContainers() {
	// 从 /var/run/mini-docker/ 读取已有容器信息
	// 检查进程是否仍在运行，恢复监控
	log.Println("恢复已有容器管理...")
}

// IsRunning 检查 Daemon 是否已在运行
func IsRunning() bool {
	pidStr := readPidFile()
	if pidStr == "" {
		return false
	}

	// 检查进程是否存在
	var pid int
	fmt.Sscanf(pidStr, "%d", &pid)
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	// 发送 signal 0 检查进程是否存在
	if err := proc.Signal(syscall.Signal(0)); err != nil {
		// 进程不存在，清理 PID 文件
		os.Remove(DaemonPidFile)
		return false
	}

	// 检查 Socket 是否可达
	conn, err := net.DialTimeout("unix", SocketPath, time.Second)
	if err != nil {
		// 进程存在但 Socket 不可达（可能是旧进程），清理
		os.Remove(DaemonPidFile)
		return false
	}
	conn.Close()
	return true
}

// readPidFile 读取 PID 文件
func readPidFile() string {
	data, err := os.ReadFile(DaemonPidFile)
	if err != nil {
		return ""
	}
	return string(data)
}

// updateContainerStatus 更新容器元数据中的状态（辅助函数）
func updateContainerStatus(containerID string, status string) {
	// 加载容器信息，更新状态，保存
	// 实际实现在 container 包中完成
	_ = containerID
	_ = status
}

// getContainerImage 获取容器的镜像名（辅助函数）
func getContainerImage(containerID string) string {
	_ = containerID
	return ""
}
