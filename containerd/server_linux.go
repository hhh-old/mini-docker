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
  - handler_task_linux.go    ← Task 生命周期处理器
  - handler_image_linux.go   ← 镜像管理处理器
  - handler_snapshot_linux.go← 快照管理处理器（Prepare/Remove/RegisterCommitted/DiffPath）
  - handler_gc_linux.go      ← GC 处理器
  - shim_manager_linux.go    ← Shim 进程管理函数
  - containerd/gc/adapter.go ← GC 适配器类型（ContentStoreAdapter/SnapshotterAdapter）

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
	"syscall"
	"time"

	"mini-docker/constants"
	"mini-docker/containerd/content"
	"mini-docker/containerd/gc"
	"mini-docker/containerd/images"
	"mini-docker/containerd/metadata"
	"mini-docker/containerd/snapshots"
	"mini-docker/containerd/snapshots/overlay"
)

// ---------------------------------------------------------------------------
// containerd 进程主体
// ---------------------------------------------------------------------------

// Containerd containerd 独立进程主体
// 对齐 Docker: containerd 作为一个独立守护进程运行，有自己的事件循环和状态管理
//
// 字段类型说明：
//   - snapshotter  使用接口 snapshots.Snapshotter（可插拔：overlay/btrfs/native）
//   - contentStore 使用接口 content.Store（同上：file/blob/s3 等）
//   - metaDB/imageService/gcCollector  使用具体类型（当前项目唯一实现，未来如需可插拔再升级为接口）
//
// 混用接口/具体类型是有意为之：依赖反转原则（"对扩展开放"）仅在确实存在多种实现的边界处
// 才有价值。snapshotter 和 contentStore 是真实 containerd 中已存在多实现的标准扩展点，
// 而其他组件（boltdb 元数据库、image 服务、gc 收集器）目前只有单一实现。
type Containerd struct {
	listener     net.Listener
	shutdown     chan struct{}
	metaDB       *metadata.DB
	contentStore content.Store
	snapshotter  snapshots.Snapshotter // 对齐 containerd: 依赖接口而非具体类型，支持可插拔 Snapshotter
	imageService *images.Service
	gcCollector  *gc.Collector
}

// NewContainerd 创建 containerd 实例
// 关键组件（metadata.DB）初始化失败时返回 error，避免返回不可用的实例
func NewContainerd() (*Containerd, error) {
	c := &Containerd{
		shutdown: make(chan struct{}),
	}
	log.Printf("containerd 进程启动")

	// 初始化 metadata.DB（关键组件，失败则直接返回错误）
	metaDB, err := metadata.Open(constants.MiniDockerRoot + "/metadata.db")
	if err != nil {
		return nil, fmt.Errorf("初始化 metadata.DB 失败: %w", err)
	}
	c.metaDB = metaDB

	// 初始化 Content Store（关键组件，镜像拉取/删除依赖此存储）
	contentRoot := constants.ContentStoreDir
	contentStore, err := content.NewFilesystemStore(contentRoot, metaDB)
	if err != nil {
		c.metaDB.Close()
		return nil, fmt.Errorf("初始化 Content Store 失败: %w", err)
	}
	c.contentStore = contentStore

	// 初始化 Snapshotter（关键组件，镜像层解压和容器运行时依赖此存储）
	snapRoot := constants.SnapshotterDir
	snap, err := overlay.NewSnapshotter(snapRoot, metaDB)
	if err != nil {
		c.metaDB.Close()
		return nil, fmt.Errorf("初始化 Snapshotter 失败: %w", err)
	}
	c.snapshotter = snap

	// 初始化 Image Service
	leaseMgr := gc.NewLeaseManager(metaDB)
	c.imageService = images.NewService(metaDB, contentStore, snap, leaseMgr)

	// 初始化 GC (5分钟周期)
	// 适配器（ContentDeleter/SnapshotDeleter）已迁移到 containerd/gc/adapter.go，
	// 这里直接调用包导出的构造函数，避免在 containerd 顶层包内自定义适配器类型
	gcCollector := gc.NewCollector(metaDB,
		gc.NewContentStoreAdapter(contentStore),
		gc.NewSnapshotterAdapter(snap),
		5*time.Minute)
	c.gcCollector = gcCollector

	return c, nil
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

	if c.gcCollector != nil {
		c.gcCollector.Start()
		log.Println("GC 守护进程已启动 (周期: 5分钟)")
	}

	return nil
}

// Stop 优雅关闭 containerd
func (c *Containerd) Stop() {
	close(c.shutdown)
	if c.gcCollector != nil {
		c.gcCollector.Stop()
	}
	if c.metaDB != nil {
		c.metaDB.Close()
	}
	if c.snapshotter != nil {
		c.snapshotter.Close()
	}
	if c.listener != nil {
		c.listener.Close()
	}
	os.Remove(constants.ContainerdSocketPath)
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

// isBidirectionalStream 判断请求是否为双向流式请求
// 双向流式：attach/exec，客户端和服务端在 JSON 握手后切换为原始字节流双向转发
// 转发过程中连接由转发逻辑自行管理（不调用 conn.Close()）
func isBidirectionalStream(reqType string) bool {
	return reqType == ReqAttachTask || reqType == ReqExecTaskStream
}

// isProgressStream 判断请求是否为单向进度推送流式请求
// 单向进度流：pull_image，服务端在 JSON 帧中持续推送下载进度
// 客户端在 ResultFrame 收到后断开连接，不需要双向转发
func isProgressStream(reqType string) bool {
	return reqType == ReqPullImage
}

// handleConnection 处理单个 Daemon 连接
// 对齐 Docker: containerd 的每个 gRPC 请求都是独立的
// 流式请求（attach/exec/pull_image）在发送响应后进入转发逻辑，
// 连接生命周期由转发逻辑管理（attach/exec 双向转发，pull_image 单向进度推送）
func (c *Containerd) handleConnection(conn net.Conn) {
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
	case ReqGC:
		return c.handleGC(req)
	case ReqPrepareSnapshot:
		return c.handlePrepareSnapshot(req)
	case ReqRemoveSnapshot:
		return c.handleRemoveSnapshot(req)
	case ReqRegisterCommitted:
		return c.handleRegisterCommitted(req)
	case ReqDiffPath:
		return c.handleDiffPath(req)
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
