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

  文件拆分：
  - server_linux.go          ← 核心结构体、初始化、启动/停止、连接处理、路由
  - handler_task_linux.go    ← Task 生命周期处理器（薄层，委托 task.Service 插件）
  - handler_image_linux.go   ← 镜像管理处理器
  - handler_snapshot_linux.go← 快照管理处理器（Prepare/Remove/RegisterCommitted/DiffPath）
  - handler_gc_linux.go      ← GC 处理器
  - handler_content_linux.go ← Content Store RPC 处理器
  - handler_container_linux.go← Container 元数据处理器

  插件化服务（通过 Plugin Manager 管理）：
  - containerd/shim/         ← Shim Manager 插件（shim 进程生命周期）
  - containerd/runtime/      ← Runtime Service 插件（OCI Runtime 抽象）
  - containerd/task/         ← Task Service 插件（容器运行时生命周期）

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
	"runtime/debug"
	"sync"
	"syscall"
	"time"

	"mini-docker/constants"
	"mini-docker/containerd/content"
	"mini-docker/containerd/diff"
	"mini-docker/containerd/events"
	"mini-docker/containerd/gc"
	"mini-docker/containerd/images"
	"mini-docker/containerd/metadata"
	"mini-docker/containerd/plugin"
	"mini-docker/containerd/snapshots"
)

// ---------------------------------------------------------------------------
// containerd 进程主体
// ---------------------------------------------------------------------------

// Containerd containerd 独立进程主体
// 对齐 Docker: containerd 作为一个独立守护进程运行，有自己的事件循环和状态管理
//
// 重构为使用 Plugin Manager：
// 所有核心组件（metadata/content/snapshotter/images/gc）通过插件系统管理，
// Containerd 不再持有各组件的直接引用，而是通过 Plugin Manager 按需获取。
// 这对齐了真实 containerd 的架构：所有组件都是插件，生命周期由 Plugin Manager 统一管理。
type Containerd struct {
	listener net.Listener // Unix Socket 监听器
	shutdown chan struct{}
	stopOnce sync.Once       // 防止 close(shutdown) 被 double-close 导致 panic
	plugins  *plugin.Manager // 插件管理器，统一管理所有组件的生命周期
	config   *plugin.Config  // 插件配置，对齐 containerd: config.toml

	// activeWriters 管理当前活跃的 Content Writer 实例
	// Writer 创建后需要在多次 RPC 调用间保持状态，直到 Commit 或 Close
	activeWriters   map[string]content.Writer // ref → Writer
	activeWritersMu sync.Mutex
}

// NewContainerd 创建 containerd 实例
// 对齐 containerd: 使用 Plugin Manager 管理所有组件的生命周期
// 1. 加载配置文件（对齐 containerd: /etc/containerd/config.toml）
// 2. 创建 Plugin Manager
// 3. 注册所有内置插件
// 4. 按拓扑排序初始化所有插件
func NewContainerd() (*Containerd, error) {
	c := &Containerd{
		shutdown:      make(chan struct{}),
		plugins:       plugin.NewManager(),
		activeWriters: make(map[string]content.Writer),
	}
	log.Printf("containerd 进程启动")

	// 加载配置文件（对齐 containerd: 从 config.toml 加载）
	// 配置文件不存在时自动生成默认配置
	configPath := constants.ContainerdConfigPath
	cfg, err := plugin.LoadConfig(configPath)
	if err != nil {
		log.Printf("警告: 加载配置文件 %s 失败: %v，使用默认配置", configPath, err)
		cfg = plugin.DefaultConfig()
	}
	c.config = cfg
	log.Printf("配置文件加载完成: %s (default_snapshotter=%s)", configPath, cfg.DefaultSnapshotter)

	// 注册所有内置插件
	plugin.RegisterBuiltinPlugins(c.plugins)

	// 按拓扑排序初始化所有插件，传入配置
	if err := c.plugins.Initialize(cfg); err != nil {
		c.plugins.Close()
		return nil, fmt.Errorf("初始化插件失败: %w", err)
	}

	return c, nil
}

// ---------------------------------------------------------------------------
// 插件访问辅助方法
// ---------------------------------------------------------------------------

// getImageService 从插件管理器获取镜像服务
func (c *Containerd) getImageService() *images.Service {
	inst, _ := c.plugins.Get(plugin.TypeService, "images")
	if inst == nil {
		return nil
	}
	return inst.(*images.Service)
}

// getSnapshotterService 从插件管理器获取 Snapshotter Service
func (c *Containerd) getSnapshotterService() *snapshots.Service {
	inst, _ := c.plugins.Get(plugin.TypeService, "snapshotter")
	if inst == nil {
		return nil
	}
	return inst.(*snapshots.Service)
}

// getDiffService 从插件管理器获取 Diff Service
func (c *Containerd) getDiffService() *diff.Service {
	inst, _ := c.plugins.Get(plugin.TypeService, "diff")
	if inst == nil {
		return nil
	}
	return inst.(*diff.Service)
}

// getGcCollector 从插件管理器获取 GC 收集器
func (c *Containerd) getGcCollector() *gc.Collector {
	inst, _ := c.plugins.Get(plugin.TypeService, "gc")
	if inst == nil {
		return nil
	}
	return inst.(*gc.Collector)
}

// getMetaDB 从插件管理器获取元数据数据库
func (c *Containerd) getMetaDB() *metadata.DB {
	inst, _ := c.plugins.Get(plugin.TypeService, "metadata")
	if inst == nil {
		return nil
	}
	return inst.(*metadata.DB)
}

// getEventService 从插件管理器获取事件服务
func (c *Containerd) getEventService() *events.Service {
	inst, _ := c.plugins.Get(plugin.TypeService, "events")
	if inst == nil {
		return nil
	}
	return inst.(*events.Service)
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
		constants.RuntimeDir,
		constants.ShimDir,
	} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return fmt.Errorf("创建目录 %s 失败: %w", dir, err)
		}
	}

	os.Remove(constants.ContainerdSocketPath)

	l, err := net.Listen("unix", constants.ContainerdSocketPath)
	if err != nil {
		return fmt.Errorf("监听 Unix Socket 失败: %w", err)
	}
	c.listener = l
	os.Chmod(constants.ContainerdSocketPath, 0666)

	log.Printf("containerd 启动成功，监听 %s (PID: %d)\n", constants.ContainerdSocketPath, os.Getpid())

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigCh
		log.Printf("收到信号 %v，开始优雅关闭...\n", sig)
		c.Stop()
		os.Exit(0)
	}()

	go c.acceptLoop()

	// GC 已在插件初始化时启动（service.gc 插件的 Init 中调用 collector.Start()）
	log.Println("GC 守护进程已通过插件系统启动 (周期: 5分钟)")

	return nil
}

// Stop 优雅关闭 containerd
// 对齐 containerd: 通过 Plugin Manager 按逆初始化顺序关闭所有插件
func (c *Containerd) Stop() {
	c.stopOnce.Do(func() {
		close(c.shutdown)
		// 插件管理器按逆初始化顺序关闭所有插件
		// 包括: gc.Stop() → snapshotter.Close() → contentStore.Close() → metaDB.Close()
		if c.plugins != nil {
			c.plugins.Close()
		}
		if c.listener != nil {
			c.listener.Close()
		}
		os.Remove(constants.ContainerdSocketPath)
		log.Println("containerd 已停止")
	})
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

// isBidirectionalStream 判断请求是否为双向流式请求
// 双向流式：attach/exec，客户端和服务端在 JSON 握手后切换为原始字节流双向转发
// 转发过程中连接由转发逻辑自行管理（不调用 conn.Close()）
func isBidirectionalStream(reqType string) bool {
	return reqType == ReqAttachTask || reqType == ReqExecTaskStream
}

// isProgressStream 判断请求是否为单向进度推送流式请求
// 单向进度流：pull_image，客户端和服务端在 JSON 帧中持续推送下载进度
// 客户端在 ResultFrame 收到后断开连接，不需要双向转发
func isProgressStream(reqType string) bool {
	return reqType == ReqPullImage || reqType == ReqSubscribeEvents
}

// handleConnection 处理单个 Daemon 连接
// 对齐 Docker: containerd 的每个 gRPC 请求都是独立的
// 流式请求（attach/exec/pull_image）在发送响应后进入转发逻辑，
// 连接生命周期由转发逻辑管理（attach/exec 双向转发，pull_image 单向进度推送）
//
// panic recovery: 单个请求的 panic 不应导致整个 containerd 进程崩溃
// 对齐真实 containerd 的容错设计——服务端必须隔离请求级别的故障
func (c *Containerd) handleConnection(conn net.Conn) {
	// 捕获 panic，避免单个请求的错误导致整个 containerd 进程崩溃
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[panic] 请求处理 panic: %v\n%s", r, debug.Stack())
			// 尝试通知客户端发生了错误
			WriteResponse(conn, Response{Success: false, Message: fmt.Sprintf("containerd 内部错误: %v", r)})
			conn.Close()
		}
	}()

	var req Request
	if err := json.NewDecoder(conn).Decode(&req); err != nil {
		WriteResponse(conn, Response{Success: false, Message: fmt.Sprintf("解析请求失败: %v", err)})
		conn.Close()
		return
	}

	// 双向流（attach/exec）和单向进度流（pull_image）都由 routeRequest 自行管理连接生命周期，所以返回之前没有conn.Close()切断unix socket通信连接
	if isBidirectionalStream(req.Type) || isProgressStream(req.Type) {
		c.routeRequest(req, conn)
		return
	}
	//非流式请求，一次性请求，写回response以后连接就关闭了
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
	case ReqPullImage:
		return c.handlePullImage(req, conn)
	case ReqListImages:
		return c.handleListImages(req)
	case ReqRemoveImage:
		return c.handleRemoveImage(req)
	case ReqInspectImage:
		return c.handleInspectImage(req)
	case ReqResolveImage:
		return c.handleResolveImage(req)
	case ReqRegisterImage:
		return c.handleRegisterImage(req)
	case ReqCommitContainer:
		return c.handleCommitContainer(req)
	case ReqCreateContainer:
		return c.handleCreateContainer(req)
	case ReqGetContainer:
		return c.handleGetContainer(req)
	case ReqListContainers:
		return c.handleListContainers(req)
	case ReqUpdateContainer:
		return c.handleUpdateContainer(req)
	case ReqDeleteContainer:
		return c.handleDeleteContainer(req)
	case ReqGC:
		return c.handleGC(req)
	case ReqPrepareSnapshot:
		return c.handlePrepareSnapshot(req)
	case ReqRemoveSnapshot:
		return c.handleRemoveSnapshot(req)
	case ReqDiffPath:
		return c.handleDiffPath(req)
	case ReqCommitSnapshot:
		return c.handleCommitSnapshot(req)
	case ReqWalkSnapshots:
		return c.handleWalkSnapshots(req)
	case ReqViewSnapshot:
		return c.handleViewSnapshot(req)
	case ReqStatSnapshot:
		return c.handleStatSnapshot(req)
	case ReqCleanupSnapshot:
		return c.handleCleanup(req)
	case ReqMountsSnapshot:
		return c.handleMountsSnapshot(req)
	case ReqUpdateSnapshot:
		return c.handleUpdateSnapshot(req)
	case ReqUsageSnapshot:
		return c.handleUsageSnapshot(req)
	case ReqContentInfo:
		return c.handleContentInfo(req)
	case ReqContentPath:
		return c.handleContentPath(req)
	case ReqContentExists:
		return c.handleContentExists(req)
	case ReqContentDelete:
		return c.handleContentDelete(req)
	case ReqContentWrite:
		return c.handleContentWrite(req)
	case ReqContentCommit:
		return c.handleContentCommit(req)
	case ReqContentWalk:
		return c.handleContentWalk(req)
	case ReqContentUpdate:
		return c.handleContentUpdate(req)
	case ReqDiffApply:
		return c.handleDiffApply(req)
	case ReqDiffDiff:
		return c.handleDiffDiff(req)
	case ReqPublishEvent:
		return c.handlePublishEvent(req)
	case ReqSubscribeEvents:
		return c.handleSubscribeEvents(req, conn)
	case ReqGetEventArchive:
		return c.handleGetEventArchive(req)
	case ReqPing:
		return Response{Success: true, Message: "pong"}
	default:
		return Response{Success: false, Message: fmt.Sprintf("未知请求类型: %s", req.Type)}
	}
}

// ---------------------------------------------------------------------------
// containerd 进程管理（供 Daemon 调用的包级函数）
// ---------------------------------------------------------------------------

// IsContainerdRunning 检查 containerd 是否已在运行
func IsContainerdRunning() bool {
	conn, err := net.DialTimeout("unix", constants.ContainerdSocketPath, time.Second)
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
		conn, err := net.DialTimeout("unix", constants.ContainerdSocketPath, time.Second)
		if err == nil {
			conn.Close()
			return nil
		}
		time.Sleep(constants.PollInterval)
	}
	return fmt.Errorf("等待 containerd 就绪超时")
}
