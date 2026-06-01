//go:build linux

package shim

import (
	"net"
	"os"
	"sync"

	"mini-docker/pty"
	"mini-docker/types"
)

// attachConn 表示一个 attach 连接及其相关的 I/O 管道
// 参考 Docker/containerd: 每个 attach 客户端独立管理，支持动态添加和移除
type attachConn struct {
	id     string   // 唯一标识符
	conn   net.Conn // 客户端连接
	closed bool     // 是否已关闭
}

// shimContext 保存 shim 进程运行时的所有状态
// 参考 Docker/containerd-shim: shim 作为容器进程的"养父"，持久化托管 I/O 流和终端
type shimContext struct {
	containerID  string
	containerPID int
	pidMu        sync.Mutex
	exitReady    <-chan struct{}
	exitInfo     *types.ExitInfo
	shutdownDone chan struct{}
	shutdownOnce sync.Once
	containerPTY *pty.PTY
	attachReady  chan struct{}
	startReady   chan struct{}
	startOnce    sync.Once
	attachOnce   sync.Once
	logFile      *os.File
	// 新增：支持多次 attach 的多路复用管理
	// 参考 Docker/containerd-shim: 使用连接池管理多个 attach 客户端，实现多客户端同时连接
	attachConns   map[string]*attachConn // attach 连接池，key 为连接 ID
	attachMu      sync.RWMutex           // 保护 attachConns 的并发访问
	broadcastOnce sync.Once              // 确保广播 goroutine 只启动一次
}
