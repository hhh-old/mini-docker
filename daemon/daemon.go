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
	"mini-docker/containerstore"
	"mini-docker/libcontainer"
	"mini-docker/network"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"mini-docker/constants"
	"mini-docker/containerd"
	"mini-docker/utils"
)

const (
	SocketPath    = constants.SocketPath
	DaemonPidFile = constants.DaemonPidFile
	DaemonLogPath = constants.DaemonLogPath
)

// Daemon 守护进程主体
type Daemon struct {
	mu         sync.RWMutex
	once       sync.Once
	containers map[string]*ContainerLive // 运行中的容器状态
	listener   net.Listener
	eventBus   *EventBus
	shutdown   chan struct{}
	service    *containerd.Service
}

// ContainerLive 运行中容器的运行时状态（仅 Daemon 持有）
// 嵌入 *ContainerInfo 消除重复字段，业务元数据通过 Info 直接访问，
// 纯内存状态（NetMgr、CgroupMgr 等接口实例）保留在此结构体中，因为它们无法序列化
type ContainerLive struct {
	mu           sync.Mutex
	Info         *containerstore.ContainerInfo
	ShimPID      int
	Cmd          *os.Process
	NetMgr       network.Manager
	RestartCount int
	UserStopped  bool
}

// NewDaemon 创建 Daemon 实例
func NewDaemon() *Daemon {
	return &Daemon{
		containers: make(map[string]*ContainerLive),
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
		constants.MiniDockerRunRoot,
		filepath.Dir(DaemonLogPath),
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

	// 确保默认网络存在（对齐 Docker: dockerd 启动时自动创建 bridge 网络）
	if err := network.EnsureDefaultNetwork(); err != nil {
		log.Printf("警告: 创建默认网络失败: %v\n", err)
	}

	// 恢复已有容器的管理（Daemon 重启后）
	d.restoreContainers()

	// 启动事件总线
	d.eventBus.Run()

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
	d.once.Do(func() {
		close(d.shutdown)
	})
	if d.listener != nil {
		d.listener.Close()
	}
	// 对齐 Docker: Daemon 停止时清理 iptables 规则
	// Docker 在 dockerd 停止时清理 DOCKER 链，mini-docker 清理 MINI-DOCKER 链
	network.CleanupAllIptables()
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
				time.Sleep(100 * time.Millisecond)
				continue
			}
		}

		go d.handleConnection(conn)
	}
}

// handleConnection 处理单个客户端连接
// 对齐 Docker 的 C/S 架构：支持普通请求/响应和流式 I/O 转发两种模式
func (d *Daemon) handleConnection(conn net.Conn) {
	var req Request
	if err := json.NewDecoder(conn).Decode(&req); err != nil {
		d.sendResponse(conn, Response{Success: false, Message: fmt.Sprintf("解析请求失败: %v", err)})
		conn.Close()
		return
	}

	resp := d.routeRequest(req, conn)

	if resp.Stream {
		d.sendResponse(conn, resp)
		if resp.StreamReady != nil {
			<-resp.StreamReady
		}
		return
	}

	d.sendResponse(conn, resp)
	conn.Close()
}

// routeRequest 路由请求到对应处理器
func (d *Daemon) routeRequest(req Request, conn net.Conn) Response {
	switch req.Type {
	case "run":
		return d.handleRun(req, conn)
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
		return d.handleExec(req, conn)
	case "logs":
		return d.handleLogs(req)
	case "resize":
		return d.handleResize(req)
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
	case "volume_create":
		return d.handleVolumeCreate(req)
	case "volume_list":
		return d.handleVolumeList(req)
	case "volume_rm":
		return d.handleVolumeRm(req)
	case "volume_inspect":
		return d.handleVolumeInspect(req)
	case "build":
		return d.handleBuild(req)
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
	for len(data) > 0 {
		n, err := conn.Write(data)
		if err != nil {
			log.Printf("写入响应失败: %v\n", err)
			return
		}
		data = data[n:]
	}
}

// RegisterContainer 注册运行中的容器到 Daemon
func (d *Daemon) RegisterContainer(state *ContainerLive) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.containers[state.Info.ID] = state
	d.eventBus.Publish(Event{
		Type:      "container_create",
		Container: state.Info.ID,
		Time:      time.Now(),
	})
}

// UnregisterContainer 从 Daemon 注销容器
func (d *Daemon) UnregisterContainer(containerID string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.containers, containerID)
}

// GetContainerLive 获取容器运行时状态
func (d *Daemon) GetContainerLive(containerID string) (*ContainerLive, bool) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	s, ok := d.containers[containerID]
	return s, ok
}

// WatchContainer 是 Daemon 的 容器生命周期监控器 ，它在独立的 goroutine 中运行，负责：
// |  1. 监听容器退出
// │  2. 收集退出信息
// │  3. 发布退出事件
// │  4. 更新容器状态
// │  5. 根据重启策略决定是否重启
// 轮询 shim 直到容器退出，然后处理重启策略或清理资源
func (d *Daemon) WatchContainer(containerID string) {
	state, ok := d.GetContainerLive(containerID)
	if !ok {
		return
	}

	// 轮询等待容器退出,获取容器状态码
	var exitCode int
	for {
		select {
		case <-d.shutdown:
			return
		default:
		}
		// 尝试从 shim 获取退出信息
		exitInfo, err := d.service.GetExitInfo(containerID)
		if err == nil {
			if exitInfo != nil {
				exitCode = exitInfo.ExitCode //获得退出码
			}
			break
		}
		// 如果 shim 不在线，等待容器进程退出
		if !containerd.IsShimAlive(containerID) {
			if state.Info.Pid > 0 && utils.CheckProcessAlive(state.Info.Pid) {
				log.Printf("容器 %s 的 shim 已离线，直接等待进程退出\n", containerID)
				for utils.CheckProcessAlive(state.Info.Pid) {
					select {
					case <-d.shutdown:
						return
					case <-time.After(2 * time.Second):
					}
				}
			}
			//此时容器进程已经退出了
			if info, err := containerd.ReadExitInfo(containerID); err == nil && info != nil {
				exitCode = info.ExitCode
			} else {
				exitCode = -1
			}
			break
		}

		select {
		case <-d.shutdown:
			return
		case <-time.After(6 * time.Second):
		}
	}

	d.eventBus.Publish(Event{
		Type:      "container_exit",
		Container: containerID,
		ExitCode:  exitCode,
		Time:      time.Now(),
	})
	//更新容器状态（通过嵌入的 Info 直接修改，避免从磁盘重新加载）
	state.Info.Status = libcontainer.StatusStopped
	state.Info.ExitCode = exitCode
	state.Info.Pid = 0
	state.Info.FinishedAt = utils.NowFormatted()
	containerstore.SaveContainerInfo(state.Info)
	//根据重启策略决定是否重启
	if state.Info.RestartPolicy != "no" {
		state.mu.Lock()
		userStopped := state.UserStopped
		state.mu.Unlock()
		if !userStopped {
			if state.Info.RestartPolicy == "always" ||
				(state.Info.RestartPolicy == "on-failure" && exitCode != 0) {
				log.Printf("容器 %s 退出 (exit=%d)，根据重启策略 %s 准备重启\n",
					containerID, exitCode, state.Info.RestartPolicy)
				d.handleRestart(containerID, exitCode)
				return
			}
		}
	}

	if _, ok := d.GetContainerLive(containerID); !ok {
		return
	}
	//清理退出的容器
	d.cleanupExitedContainer(containerID)
}

func buildContainerLive(info *containerstore.ContainerInfo, shimPID int) *ContainerLive {
	state := &ContainerLive{
		Info:    info,
		ShimPID: shimPID,
	}

	if info.Network != "" {
		state.NetMgr = network.NewManagerFromInfo(info.Network, info.PortMap, info.ContainerIP, info.VethHost)
	}

	return state
}

// handleRestart 处理容器重启（对齐 Docker: 保留容器ID + 指数退避 + 次数限制）
func (d *Daemon) handleRestart(containerID string, exitCode int) {
	info, err := containerstore.LoadContainerInfoByID(containerID)
	if err != nil {
		log.Printf("容器 %s 重启失败: 无法加载容器信息\n", containerID)
		d.cleanupExitedContainer(containerID)
		return
	}

	state, ok := d.GetContainerLive(containerID)
	restartCount := 0
	if ok {
		state.mu.Lock()
		restartCount = state.RestartCount + 1
		state.RestartCount = restartCount
		state.mu.Unlock()
	}

	if info.RestartPolicy == "on-failure" {
		maxRetries := info.MaxRestartRetries
		if maxRetries <= 0 {
			maxRetries = constants.DefaultMaxRetries
		}
		if restartCount > maxRetries {
			log.Printf("容器 %s 已达到 on-failure 最大重启次数 (%d)，停止重启\n",
				containerID, maxRetries)
			d.cleanupExitedContainer(containerID)
			return
		}
	}

	backoff := time.Duration(restartCount) * constants.RestartBackoffBase
	if backoff > constants.RestartBackoffMax {
		backoff = constants.RestartBackoffMax
	}
	log.Printf("容器 %s 将在 %v 后重启 (第 %d 次)\n",
		containerID, backoff, restartCount)

	select {
	case <-time.After(backoff):
	case <-d.shutdown:
		return
	}

	d.mu.Lock()
	delete(d.containers, containerID)
	d.mu.Unlock()

	d.cleanupContainerResources(containerID, nil, CleanupOptions{DeleteTask: true, CleanupOverlay: true, RemoveInfo: true})

	backupInfo := *info

	req := buildRunRequest(info)

	resp := d.runWithID(req, nil, containerID)
	if resp.Success {
		if newState, ok := d.GetContainerLive(containerID); ok {
			newState.mu.Lock()
			newState.RestartCount = restartCount
			newState.mu.Unlock()
		}
		log.Printf("容器 %s 重启成功 (第 %d 次)\n", containerID, restartCount)
	} else {
		log.Printf("容器 %s 重启失败: %s\n", containerID, resp.Message)
		backupInfo.Status = libcontainer.StatusStopped
		containerstore.SaveContainerInfo(&backupInfo)
	}
}

// cleanupExitedContainer 清理已退出的容器
// 仅清理运行时资源（网络、cgroup、shim），保留 OverlayFS 和 ContainerInfo 以支持 docker start
func (d *Daemon) cleanupExitedContainer(containerID string) {
	d.mu.Lock()
	state, _ := d.containers[containerID]
	delete(d.containers, containerID)
	d.mu.Unlock()

	d.cleanupContainerResources(containerID, state, CleanupOptions{ShutdownShim: true})
}

// runHealthCheckLoop 周期执行健康检查（对齐 Docker: Daemon 周期执行 HEALTHCHECK）
func (d *Daemon) runHealthCheckLoop(containerID string) {
	info, err := containerstore.LoadContainerInfoByID(containerID)
	if err != nil || info.HealthCmd == "" {
		return
	}

	config := containerstore.ParseHealthConfig(info)

	initialDelay := 5 * time.Second
	select {
	case <-time.After(initialDelay):
	case <-d.shutdown:
		return
	}

	ticker := time.NewTicker(config.Interval)
	defer ticker.Stop()

	for {
		select {
		case <-d.shutdown:
			return
		case <-ticker.C:
		}

		currentInfo, err := containerstore.LoadContainerInfoByID(containerID)
		if err != nil || (currentInfo.Status != libcontainer.StatusRunning && currentInfo.Status != libcontainer.StatusPaused) {
			return
		}

		result := containerstore.RunHealthCheck(currentInfo, config)
		containerstore.SaveHealthResult(containerID, result)

		if result.Status == containerstore.HealthUnhealthy {
			d.eventBus.Publish(Event{
				Type:      "container_unhealthy",
				Container: containerID,
				Time:      time.Now(),
				Message:   fmt.Sprintf("健康检查失败: %s", result.Output),
			})
		}
	}
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
		if task.Status == libcontainer.StatusStopped {
			d.handleStoppedTask(task.ID)
			continue
		}

		shimAlive := containerd.IsShimAlive(task.ID)
		if !shimAlive {
			log.Printf("容器 %s 的 shim 进程已离线，尝试降级恢复...\n", task.ID)
			if d.handleDeadShim(task.ID) {
				restored++
			} else {
				failed++
			}
			continue
		}

		shimPID := containerd.ReadShimPID(task.ID)

		info, err := containerstore.LoadContainerInfoByID(task.ID)
		if err != nil {
			log.Printf("容器 %s 的元数据加载失败: %v，跳过恢复\n", task.ID, err)
			failed++
			continue
		}

		state := buildContainerLive(info, shimPID)

		d.RegisterContainer(state)
		go d.WatchContainer(task.ID)

		info.Status = libcontainer.StatusRunning
		info.Pid = task.Pid
		containerstore.SaveContainerInfo(info)

		restored++
		log.Printf("恢复容器 %s (PID=%d, shim=%d, 策略=%s)\n",
			task.ID, task.Pid, shimPID, info.RestartPolicy)
	}

	d.cleanupOrphanedInfos(tasks)

	log.Printf("容器恢复完成: 共 %d 个任务, 成功恢复 %d 个, 失败 %d 个\n",
		len(tasks), restored, failed)
}

// handleStoppedTask 处理已停止的任务
// 从 runtime state 中发现状态为 stopped 的任务，清理残留的 runtime 资源
func (d *Daemon) handleStoppedTask(taskID string) {
	info, err := containerstore.LoadContainerInfoByID(taskID)
	if err != nil {
		d.service.DeleteTask(taskID)
		return
	}

	if info.Status == libcontainer.StatusRunning {
		exitCode := info.ExitCode
		if exitInfo, err := containerd.ReadExitInfo(taskID); err == nil && exitInfo != nil {
			exitCode = exitInfo.ExitCode
		}
		info.Status = libcontainer.StatusStopped
		info.ExitCode = exitCode
		info.FinishedAt = time.Now().Format(constants.TimeFormat)
		containerstore.SaveContainerInfo(info)
		log.Printf("容器 %s 进程已退出，状态已更新为 stopped\n", taskID)
	}

	if info.RestartPolicy == "always" ||
		(info.RestartPolicy == "on-failure" && info.ExitCode != 0) {
		log.Printf("容器 %s 重启策略为 %s，准备重启...\n", taskID, info.RestartPolicy)
		d.triggerRestart(taskID, info)
	}
}

// handleDeadShim 处理 shim 进程已死亡的情况
// shim 死亡意味着无法再与容器 runtime 通信，需要判断容器进程是否仍在运行
// 返回值: true 表示成功恢复（进程仍存活并已注册），false 表示恢复失败
func (d *Daemon) handleDeadShim(taskID string) bool {
	info, err := containerstore.LoadContainerInfoByID(taskID)
	if err != nil {
		d.service.DeleteTask(taskID)
		return false
	}

	proc, err := os.FindProcess(info.Pid)
	processAlive := err == nil && proc.Signal(syscall.Signal(0)) == nil

	if processAlive {
		if !info.Tty {
			// 非 TTY 容器：shim 崩溃后容器仍可存活（日志文件 fd 不受 shim 影响）
			// 重启 shim 以 takeover 模式接管容器，恢复 Wait4 监控和控制 socket 服务
			log.Printf("容器 %s 进程 (PID=%d) 仍在运行，非 TTY 模式，重启 shim 接管\n",
				taskID, info.Pid)
			shimPID, restartErr := d.service.RestartShim(taskID, info.Pid)
			if restartErr != nil {
				log.Printf("容器 %s 重启 shim 失败: %v，降级恢复\n", taskID, restartErr)
				// 降级恢复：无 shim 监控，仅注册容器状态
				state := buildContainerLive(info, 0)
				d.RegisterContainer(state)
				go d.WatchContainer(taskID)
				info.Status = libcontainer.StatusRunning
				containerstore.SaveContainerInfo(info)
				return true
			}
			state := buildContainerLive(info, shimPID)
			d.RegisterContainer(state)
			go d.WatchContainer(taskID)
			info.ShimPID = shimPID
			info.Status = libcontainer.StatusRunning
			containerstore.SaveContainerInfo(info)
			return true
		}
		// TTY 容器：shim 崩溃后 PTY Master 已关闭，容器可能即将收到 SIGHUP 退出
		// 降级恢复，等待容器自然退出后由 WatchContainer 捕获
		log.Printf("容器 %s 进程 (PID=%d) 仍在运行但 shim 已丢失，TTY 模式降级恢复\n",
			taskID, info.Pid)

		state := buildContainerLive(info, 0)

		d.RegisterContainer(state)
		go d.WatchContainer(taskID)

		info.Status = libcontainer.StatusRunning
		containerstore.SaveContainerInfo(info)
		return true
	}

	log.Printf("容器 %s 进程和 shim 均已死亡，清理资源\n", taskID)
	info.Status = libcontainer.StatusStopped
	info.ExitCode = -1
	info.FinishedAt = time.Now().Format(constants.TimeFormat)
	containerstore.SaveContainerInfo(info)
	d.service.DeleteTask(taskID)

	if info.RestartPolicy == "always" {
		log.Printf("容器 %s 根据重启策略 %s 准备重启\n", taskID, info.RestartPolicy)
		d.triggerRestart(taskID, info)
	}
	return false
}

// triggerRestart 触发容器重启（Daemon 恢复场景，保留容器ID）
func (d *Daemon) triggerRestart(taskID string, info *containerstore.ContainerInfo) {
	d.cleanupContainerResources(taskID, nil, CleanupOptions{DeleteTask: true, CleanupOverlay: true, RemoveInfo: true})

	req := buildRunRequest(info)

	resp := d.runWithID(req, nil, taskID)
	if resp.Success {
		log.Printf("容器 %s 重启成功\n", taskID)
	} else {
		log.Printf("容器 %s 重启失败: %s\n", taskID, resp.Message)
	}
}

// cleanupOrphanedInfos 清理孤儿 ContainerInfo
// 当 ContainerInfo JSON 文件存在但对应的 runtime state 已不存在时，
// 说明容器的 runtime 资源已被清理但元数据残留，需要同步更新状态
func (d *Daemon) cleanupOrphanedInfos(tasks []*libcontainer.ContainerState) {
	taskSet := make(map[string]bool)
	for _, t := range tasks {
		taskSet[t.ID] = true
	}

	containers, err := containerstore.ListContainers()
	if err != nil {
		return
	}

	for _, c := range containers {
		if taskSet[c.ID] {
			continue
		}

		if c.Status == libcontainer.StatusRunning {
			c.Status = libcontainer.StatusStopped
			c.FinishedAt = time.Now().Format(constants.TimeFormat)
			containerstore.SaveContainerInfo(c)
			log.Printf("清理孤儿容器 %s: runtime 已不存在，状态更新为 stopped\n",
				c.ID)
		}
	}
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
	// 发送 signal 0 检查进程是否存在，这里利用信号 0 无副作用的探测，判断之前记录的 PID 对应的守护进程是否还在运行。
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

// CleanupOptions 控制容器清理的范围（对齐 Docker: 不同操作需要不同粒度的清理）
//
// 清理顺序（对齐 Docker dockerd 的清理流程）:
//  1. DeleteTask / ShutdownShim — 停止 shim 和 runtime
//  2. 网络清理 — 删除 veth、释放 IP、清理 iptables 规则
//  3. Cgroup 清理 — 删除 cgroup 目录
//  4. Overlay 清理 — 卸载并删除 OverlayFS 目录
//  5. 删除 ContainerInfo JSON — 移除持久化元数据
type CleanupOptions struct {
	DeleteTask     bool // 停止 shim 进程并删除 runtime/shim 目录
	ShutdownShim   bool // 仅关闭 shim（比 DeleteTask 更轻量，用于自然退出的容器）
	CleanupOverlay bool // 清理 OverlayFS（卸载 + 删除目录）
	RemoveInfo     bool // 删除 ContainerInfo JSON 文件
}

// cleanupContainerResources 统一的容器资源清理方法
// 嵌入 *ContainerInfo 后，state.Info 直接包含所有元数据，
// 不再需要"先试运行时状态，不行再试 ContainerInfo"的双路径降级逻辑
func (d *Daemon) cleanupContainerResources(containerID string, state *ContainerLive, opts CleanupOptions) {
	// 获取 Info 引用：state 非空时用 Info，否则从磁盘加载
	var info *containerstore.ContainerInfo
	if state != nil && state.Info != nil {
		info = state.Info
	} else {
		info, _ = containerstore.LoadContainerInfoByID(containerID)
	}

	if opts.DeleteTask {
		d.service.DeleteTask(containerID)
	} else if opts.ShutdownShim {
		d.service.ShutdownShim(containerID)
	}

	// 网络清理：优先使用运行时接口（NetMgr 是活的接口实例），否则从 Info 重建
	if state != nil && state.NetMgr != nil {
		_ = state.NetMgr.Disconnect()
	} else if info != nil {
		containerstore.CleanupContainerNetwork(info)
	}

	// Cgroup 清理：从 Info 获取 cgroupName
	if info != nil && info.CgroupName != "" {
		containerstore.CleanupCgroup(info.CgroupName)
	}

	if opts.CleanupOverlay && info != nil {
		containerstore.CleanupOverlay(info)
	}

	if opts.RemoveInfo {
		containerstore.RemoveContainerInfo(containerID)
	}
}
