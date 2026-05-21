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
	"mini-docker/container"
	"mini-docker/spec"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"mini-docker/containerd"
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
	service    *containerd.Service
}

// ContainerState 运行中容器的运行时状态（仅 Daemon 持有）
type ContainerState struct {
	ID        string
	Pid       int
	ShimPID   int
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
		service:    containerd.NewService(),
	}
}

// 启动 Daemon，监听 Unix Socket
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

	// 监听 Unix Socket,此处会创建SocketPath文件
	l, err := net.Listen("unix", SocketPath)
	if err != nil {
		return fmt.Errorf("监听 Unix Socket 失败: %w", err)
	}
	d.listener = l

	// 设置 Socket 文件权限（允许所有用户访问）
	os.Chmod(SocketPath, 0666)

	log.Printf("Daemon 启动成功，监听 %s (PID: %d)\n", SocketPath, os.Getpid())

	// 恢复已有容器的管理（Daemon 重启后）
	d.restoreContainers()

	// 启动事件总线
	go d.eventBus.Run()

	// 信号处理
	sigCh := make(chan os.Signal, 1)
	//注册需要监听的信号
	//告诉 Go 运行时（Runtime）：当这个进程收到以下两种信号时，不要直接杀死进程，而是把信号“转发”到 sigCh 通道里：
	//syscall.SIGINT：中断信号。通常是用户在终端按下 Ctrl + C 时触发。
	//syscall.SIGTERM：终止信号。通常是系统关机、使用 kill 命令（默认不带 -9），或 systemd 停止服务时发送的优雅退出请求。
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigCh // 阻塞等待，直到收到信号
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
		case <-d.shutdown: //检查通道是否关闭,如果通道调用了close(d.shutdown) （也就是在deamon进程关闭的时候Stop() 函数执行了），读取关闭的通道绝不阻塞，会立刻返回一个该类型的“零值”（这里是 struct{}{}）
			return
		default:
		}

		conn, err := d.listener.Accept() //阻塞等待客户端连接
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
	// mini-docker run -d --name web -m 128m -c 512 -p 8080:80 -v /data:/app busybox sleep 3600
	case "run": // 创建并运行容器（-d 后台模式走 daemon，前台模式直接调用 container 包）
		return d.handleRun(req)
	// mini-docker stop <containerID>
	case "stop": // 停止运行中的容器（SIGTERM → 等待 → SIGKILL）
		return d.handleStop(req)
	// mini-docker start <containerID>
	case "start": // 启动已停止的容器（重新创建 shim + runtime）
		return d.handleStart(req)
	// mini-docker pause <containerID>
	case "pause": // 暂停容器（通过 cgroup freezer 冻结进程）
		return d.handlePause(req)
	// mini-docker unpause <containerID>
	case "unpause": // 恢复已暂停的容器
		return d.handleUnpause(req)
	// mini-docker rm <containerID>
	case "rm": // 删除已停止的容器（清理 shim、runtime、overlay 等资源）
		return d.handleRm(req)
	// mini-docker ps
	case "ps": // 列出所有容器（从持久化的 ContainerInfo JSON 读取）
		return d.handlePs(req)
	// mini-docker exec <containerID> ls -la
	case "exec": // 在运行中的容器内执行命令（通过 shim + nsenter 进入容器 namespace）
		return d.handleExec(req)
	// mini-docker logs <containerID>
	case "logs": // 查看容器日志（读取 json-log 格式日志文件）
		return d.handleLogs(req)
	// mini-docker events
	case "events": // 实时监听容器事件流（阻塞式推送 start/stop/exit 等事件）
		return d.handleEvents(req)
	// mini-docker images
	case "images": // 列出本地镜像（从镜像数据库读取）
		return d.handleImages(req)
	// mini-docker pull busybox
	case "pull": // 拉取镜像（从 Registry 下载或本地创建 rootfs）
		return d.handlePull(req)
	// mini-docker rmi busybox
	case "rmi": // 删除本地镜像（清理 rootfs 和镜像数据库记录）
		return d.handleRmi(req)
	// mini-docker network create --name mynet --driver bridge --subnet 172.20.0.0/16
	case "network_create": // 创建 bridge 网络（创建 Linux bridge + 初始化子网 IPAM）
		return d.handleNetworkCreate(req)
	// mini-docker network list
	case "network_list": // 列出所有已创建的网络
		return d.handleNetworkList(req)
	// mini-docker network delete <networkName>
	case "network_delete": // 删除网络（断开所有容器 + 销毁 bridge + 清理 iptables）
		return d.handleNetworkDelete(req)
	// daemon 内部使用，客户端通过 Client.Ping() 调用
	case "ping": // 心跳检测，客户端用来确认 daemon 是否存活
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
func (d *Daemon) WatchContainer(containerID string) {
	state, ok := d.GetContainerState(containerID)
	if !ok {
		return
	}

	exitInfo, err := d.service.GetExitInfo(containerID)
	if err != nil {
		log.Printf("获取容器 %s 退出信息失败: %v\n", containerID[:12], err)
		return
	}

	exitCode := 0
	if exitInfo != nil {
		exitCode = exitInfo.ExitCode
	}

	d.eventBus.Publish(Event{
		Type:      "container_exit",
		Container: containerID,
		ExitCode:  exitCode,
		Time:      time.Now(),
	})

	if state.RestartPolicy != "no" {
		if state.RestartPolicy == "always" ||
			(state.RestartPolicy == "on-failure" && exitCode != 0) {
			log.Printf("容器 %s 退出 (exit=%d)，根据重启策略 %s 准备重启\n",
				containerID[:12], exitCode, state.RestartPolicy)
			d.handleRestart(containerID, exitCode)
			return
		}
	}

	d.cleanupExitedContainer(containerID)
}

// handleRestart 处理容器重启
func (d *Daemon) handleRestart(containerID string, exitCode int) {
	d.mu.Lock()
	delete(d.containers, containerID)
	d.mu.Unlock()

	imageName := containerd.GetContainerImage(containerID)
	cmd := containerd.GetContainerCmd(containerID)
	restartPolicy := containerd.GetContainerRestartPolicy(containerID)

	d.service.DeleteTask(containerID)

	req := Request{
		Type: "run",
		Args: map[string]string{
			"image":          imageName,
			"cmd":            strings.Join(cmd, " "),
			"restart_policy": restartPolicy,
			"detach":         "true",
		},
	}
	resp := d.handleRun(req)
	if resp.Success {
		log.Printf("容器 %s 重启成功\n", containerID[:12])
	} else {
		log.Printf("容器 %s 重启失败: %s\n", containerID[:12], resp.Message)
	}
}

// cleanupExitedContainer 清理已退出的容器
func (d *Daemon) cleanupExitedContainer(containerID string) {
	d.mu.Lock()
	delete(d.containers, containerID)
	d.mu.Unlock()
}

// restoreContainers 的核心职责是在 Daemon 重启后，重新接管和恢复对已有容器的管理。
// 在容器化架构（如 Docker / containerd）中，Daemon 进程（dockerd）的退出、崩溃或重启，不应该导致正在运行的后台容器（-d）一同挂掉（这一特性在 Docker 中被称为 Live Restore）。
// 当 Daemon 重新启动时，它需要知道当前宿主机上还有哪些容器正在运行，并将它们重新纳入自己的监控体系中。
//
// 恢复流程（对齐 Docker 的 dockerd restore 逻辑）：
//  1. 从 runtime state 文件扫描所有已知任务
//  2. 验证 shim 进程是否存活（通过 socket 连通性检测）
//  3. 验证容器进程是否存活（通过 signal 0 探测）
//  4. 从持久化的 ContainerInfo JSON 重建内存状态
//  5. 重新注册到 Daemon 的 containers map
//  6. 重新启动 WatchContainer goroutine 监控容器退出
//  7. 对于已死亡的容器，根据重启策略决定是否重启
func (d *Daemon) restoreContainers() {
	log.Println("恢复已有容器管理...")

	tasks, err := d.service.ListTasks()
	if err != nil {
		log.Printf("列出已有任务失败: %v\n", err)
		return
	}

	if len(tasks) == 0 {
		log.Println("没有需要恢复的容器")
		return
	}

	restored := 0
	failed := 0
	for _, task := range tasks {
		shortID := task.ID
		if len(shortID) > 12 {
			shortID = shortID[:12]
		}

		if task.Status == "stopped" {
			d.handleStoppedTask(task.ID, shortID)
			continue
		}

		shimAlive := containerd.IsShimAlive(task.ID)
		if !shimAlive {
			log.Printf("容器 %s 的 shim 进程已离线，尝试降级恢复...\n", shortID)
			if d.handleDeadShim(task.ID, shortID) {
				restored++
			} else {
				failed++
			}
			continue
		}

		shimPID := containerd.ReadShimPID(task.ID)

		info, err := d.loadContainerInfoForRestore(task.ID)
		if err != nil {
			log.Printf("容器 %s 的元数据加载失败: %v，跳过恢复\n", shortID, err)
			failed++
			continue
		}

		createdAt := time.Now()
		if info.CreatedAt != "" {
			if t, err := time.Parse("2006-01-02 15:04:05", info.CreatedAt); err == nil {
				createdAt = t
			}
		}

		state := &ContainerState{
			ID:            task.ID,
			Pid:           task.Pid,
			ShimPID:       shimPID,
			CreatedAt:     createdAt,
			RestartPolicy: info.RestartPolicy,
		}

		d.RegisterContainer(state)
		go d.WatchContainer(task.ID)

		info.Status = "running"
		info.Pid = task.Pid
		container.SaveContainerInfo(info)

		restored++
		log.Printf("恢复容器 %s (PID=%d, shim=%d, 策略=%s)\n",
			shortID, task.Pid, shimPID, info.RestartPolicy)
	}

	d.cleanupOrphanedInfos(tasks)

	log.Printf("容器恢复完成: 共 %d 个任务, 成功恢复 %d 个, 失败 %d 个\n",
		len(tasks), restored, failed)
}

// loadContainerInfoForRestore 从持久化存储加载容器元数据
// 容器元数据存储在 /var/run/mini-docker/<shortID>.json
func (d *Daemon) loadContainerInfoForRestore(containerID string) (*container.ContainerInfo, error) {
	containers, err := container.ListContainers()
	if err != nil {
		return nil, fmt.Errorf("列出容器信息失败: %w", err)
	}

	for _, c := range containers {
		if c.ID == containerID {
			return c, nil
		}
	}

	return nil, fmt.Errorf("容器 %s 的元数据不存在", containerID)
}

// handleStoppedTask 处理已停止的任务
// 从 runtime state 中发现状态为 stopped 的任务，清理残留的 runtime 资源
func (d *Daemon) handleStoppedTask(taskID string, shortID string) {
	info, err := d.loadContainerInfoForRestore(taskID)
	if err != nil {
		d.service.DeleteTask(taskID)
		return
	}

	if info.Status == "running" {
		info.Status = "exited"
		info.FinishedAt = time.Now().Format("2006-01-02 15:04:05")
		container.SaveContainerInfo(info)
		log.Printf("容器 %s 进程已退出，状态已更新为 exited\n", shortID)
	}

	if info.RestartPolicy == "always" {
		log.Printf("容器 %s 重启策略为 always，准备重启...\n", shortID)
		d.triggerRestart(taskID, info)
	}
}

// handleDeadShim 处理 shim 进程已死亡的情况
// shim 死亡意味着无法再与容器 runtime 通信，需要判断容器进程是否仍在运行
// 返回值: true 表示成功恢复（进程仍存活并已注册），false 表示恢复失败
func (d *Daemon) handleDeadShim(taskID string, shortID string) bool {
	info, err := d.loadContainerInfoForRestore(taskID)
	if err != nil {
		d.service.DeleteTask(taskID)
		return false
	}

	proc, err := os.FindProcess(info.Pid)
	processAlive := err == nil && proc.Signal(syscall.Signal(0)) == nil

	if processAlive {
		log.Printf("容器 %s 进程 (PID=%d) 仍在运行但 shim 已丢失，降级恢复\n",
			shortID, info.Pid)

		createdAt := time.Now()
		if info.CreatedAt != "" {
			if t, err := time.Parse("2006-01-02 15:04:05", info.CreatedAt); err == nil {
				createdAt = t
			}
		}

		state := &ContainerState{
			ID:            taskID,
			Pid:           info.Pid,
			ShimPID:       0,
			CreatedAt:     createdAt,
			RestartPolicy: info.RestartPolicy,
		}

		d.RegisterContainer(state)
		go d.WatchContainer(taskID)

		info.Status = "running"
		container.SaveContainerInfo(info)
		return true
	}

	log.Printf("容器 %s 进程和 shim 均已死亡，清理资源\n", shortID)
	info.Status = "exited"
	info.ExitCode = -1
	info.FinishedAt = time.Now().Format("2006-01-02 15:04:05")
	container.SaveContainerInfo(info)
	d.service.DeleteTask(taskID)

	if info.RestartPolicy == "always" || (info.RestartPolicy == "on-failure" && info.ExitCode != 0) {
		log.Printf("容器 %s 根据重启策略 %s 准备重启\n", shortID, info.RestartPolicy)
		d.triggerRestart(taskID, info)
	}
	return false
}

// triggerRestart 触发容器重启
func (d *Daemon) triggerRestart(taskID string, info *container.ContainerInfo) {
	d.service.DeleteTask(taskID)

	cmdStr := ""
	if len(info.Cmd) > 0 {
		cmdStr = strings.Join(info.Cmd, " ")
	}

	req := Request{
		Type: "run",
		Args: map[string]string{
			"image":          info.Image,
			"cmd":            cmdStr,
			"restart_policy": info.RestartPolicy,
			"detach":         "true",
			"name":           info.Name,
		},
	}
	if info.NetworkName != "" {
		req.Args["network"] = info.NetworkName
	}
	if info.PortMap != "" {
		req.Args["port_map"] = info.PortMap
	}

	resp := d.handleRun(req)
	if resp.Success {
		log.Printf("容器 %s 重启成功\n", taskID[:minLen(taskID, 12)])
	} else {
		log.Printf("容器 %s 重启失败: %s\n", taskID[:minLen(taskID, 12)], resp.Message)
	}
}

// cleanupOrphanedInfos 清理孤儿 ContainerInfo
// 当 ContainerInfo JSON 文件存在但对应的 runtime state 已不存在时，
// 说明容器的 runtime 资源已被清理但元数据残留，需要同步更新状态
func (d *Daemon) cleanupOrphanedInfos(tasks []spec.State) {
	taskSet := make(map[string]bool)
	for _, t := range tasks {
		taskSet[t.ID] = true
	}

	containers, err := container.ListContainers()
	if err != nil {
		return
	}

	for _, c := range containers {
		if taskSet[c.ID] {
			continue
		}

		if c.Status == "running" {
			c.Status = "exited"
			c.FinishedAt = time.Now().Format("2006-01-02 15:04:05")
			container.SaveContainerInfo(c)
			log.Printf("清理孤儿容器 %s: runtime 已不存在，状态更新为 exited\n",
				c.ID[:minLen(c.ID, 12)])
		}
	}
}

func minLen(s string, n int) int {
	if len(s) < n {
		return len(s)
	}
	return n
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

// ExitInfo 容器退出信息（从 shim 读取）
type ExitInfo struct {
	ExitCode int    `json:"exit_code"`
	ExitedAt string `json:"exited_at"`
}
