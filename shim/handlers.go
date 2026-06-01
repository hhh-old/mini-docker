//go:build linux

package shim

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"os/exec"
	"sync"
	"time"

	"mini-docker/pty"
	"mini-docker/types"

	"golang.org/x/sys/unix"
)

// ---------------------------------------------------------------------------
// 通用响应辅助函数
// ---------------------------------------------------------------------------

func sendSuccess(conn net.Conn, data interface{}) {
	json.NewEncoder(conn).Encode(types.ShimResponse{Success: true, Data: data})
}

func sendSuccessRaw(conn net.Conn) {
	json.NewEncoder(conn).Encode(types.ShimResponse{Success: true})
}

func sendError(conn net.Conn, msg string) {
	json.NewEncoder(conn).Encode(types.ShimResponse{Success: false, Message: msg})
}

func sendErrorf(conn net.Conn, format string, args ...interface{}) {
	sendError(conn, fmt.Sprintf(format, args...))
}

// ---------------------------------------------------------------------------
// 各请求类型的处理器
// ---------------------------------------------------------------------------

// handleState 处理 state 请求
// 对齐 Docker/containerd: shim 通过 runtime state 获取容器状态，而非直接访问 libcontainer
func handleState(conn net.Conn, ctx *shimContext, req *types.ShimRequest) {
	state, err := getContainerStateViaRuntime(ctx.containerID)
	if err != nil {
		sendError(conn, err.Error())
		return
	}
	sendSuccess(conn, state)
}

// handleKill 处理 kill 请求
// 对齐 Docker/containerd: shim 通过调用 runtime kill 发送信号，而非直接调用系统 kill
// 这样信号发送由 runtime 统一管理，确保符合 OCI 规范
func handleKill(conn net.Conn, ctx *shimContext, req *types.ShimRequest) {
	if err := runRuntimeCommandWithSignal("kill", ctx.containerID, req.Signal); err != nil {
		sendErrorf(conn, "发送信号失败: %v", err)
		return
	}
	sendSuccessRaw(conn)
}

// handleExitInfo 处理 exit_info 请求
func handleExitInfo(conn net.Conn, ctx *shimContext, req *types.ShimRequest) {
	select {
	case <-ctx.exitReady: // 容器一旦退出，这里立刻响应，并将退出码返回给 Daemon
		sendSuccess(conn, ctx.exitInfo)
	case <-time.After(5 * time.Second):
		sendError(conn, "容器尚未退出")
	}
}

// handleExec 处理 exec 请求
// docker exec 是 Docker 提供的一个核心命令，用于在已运行的容器内部执行新的进程或命令
// 最典型的场景是：当你用 docker run -d 启动了一个后台容器（比如一个 nginx 或 redis 容器）后，想查看容器内部的文件、安装软件、或者启动一个交互式 Shell 来调试，这时候 docker exec 就能派上用场。
// 与 docker attach 的区别
//
//	docker exec		    				docker attach
//
// 作用		在容器内启动一个新进程				连接到容器当前正在运行的主进程的终端
// 交互性		可以同时运行多个 exec，彼此独立		可以有多个 attach 连接，通常只用一个，否则多人 attach 会互相干扰
// 退出影响	exit 只退出 exec 进程，容器继续运行	attach 退出不直接影响容器（Ctrl+P Q 分离），但 Ctrl+C 会发信号给主进程，可能导致容器退出
//
// 适用场景	调试、执行临时命令、开启额外 Shell	查看主进程的输出、与主进程交互（不推荐）
func handleExec(conn net.Conn, ctx *shimContext, req *types.ShimRequest, containerPID int) {
	if containerPID <= 0 {
		sendError(conn, "容器进程尚未启动")
		return
	}

	// 对齐 Docker: 交互式 exec (-it) 需要创建 PTY
	var execPTY *pty.PTY
	var execCmd *exec.Cmd
	var ptyErr error
	if req.Tty {
		execPTY, ptyErr = pty.Open()
		if ptyErr != nil {
			log.Printf("[shim] 创建 exec PTY 失败: %v\n", ptyErr)
			sendError(conn, "创建 PTY 失败")
			return
		}
		// 将 PTY Slave 路径传递给 runtime，runtime 会打开并绑定到 nsenter
		execArgs := append([]string{"runtime", "exec", ctx.containerID, "--tty", "--console", execPTY.Name}, req.Args...)
		execCmd = exec.Command("/proc/self/exe", execArgs...)
	} else {
		execArgs := append([]string{"runtime", "exec", ctx.containerID}, req.Args...)
		execCmd = exec.Command("/proc/self/exe", execArgs...)
		execCmd.Stdin = conn
		execCmd.Stdout = conn
		execCmd.Stderr = conn
	}

	// 对齐 Docker/containerd: shim 通过调用 runtime exec 在容器内执行命令，而非直接使用 nsenter
	// 这样 exec 进程能正确继承容器的 namespace、cgroup、seccomp 等配置
	if err := execCmd.Start(); err != nil {
		log.Printf("[shim] exec runtime 启动失败: %v\n", err)
		if execPTY != nil {
			execPTY.Close()
		}
		sendError(conn, "exec 启动失败")
		return
	}

	// 如果是 TTY 模式，启动 I/O 转发 goroutine
	// 把用户的网络连接（conn）和容器里的伪终端（PTY）连接起来，让用户感觉自己就在容器终端里操作。
	// ┌──────────┐
	// │ 用户终端 │
	// └────┬─────┘
	//      │
	//      ▼
	//    conn
	//      │
	//      ▼
	// ┌──────────┐
	// │ 当前程序 │
	// └────┬─────┘
	//      │
	//      ▼
	// PTY Master
	//      │
	//      ▼
	// PTY Slave
	//      │
	//      ▼
	//   bash/sh
	var ioWg sync.WaitGroup
	var closeOnce sync.Once

	if req.Tty && execPTY != nil {
		execPTY.Slave.Close() // nsenter 会重新打开 slave
		execPTY.Slave = nil   // 标记为已关闭，防止 pty.Close() 重复关闭

		// 转发用户输入 → PTY Master
		ioWg.Add(1)
		go func() {
			defer ioWg.Done()
			// io.Copy(dst, src)内部逻辑类似：
			// for {
			//     n, err := src.Read(buf)
			//
			//     dst.Write(buf[:n])
			//
			//     if err != nil {
			//         break
			//     }
			// }

			_, _ = io.Copy(execPTY.Master, conn) // conn没数据的时候阻塞
			// 任意一侧 I/O 结束，关闭 conn 和 Master 触发另一侧也退出
			closeOnce.Do(func() {
				_ = conn.Close()
				_ = execPTY.Master.Close()
			})
		}()
		// 转发 PTY Master → 用户输出
		ioWg.Add(1)
		go func() {
			defer ioWg.Done()
			_, _ = io.Copy(conn, execPTY.Master)
			closeOnce.Do(func() {
				_ = conn.Close()
				_ = execPTY.Master.Close()
			})
		}()
	}

	// I/O 转发 goroutine 已就绪，现在通知 Daemon
	json.NewEncoder(conn).Encode(types.ShimResponse{Success: true, Stream: req.Tty})

	// 等待 exec 进程完成
	// 对齐 Docker: 交互式 exec (-it) 没有超时限制，非交互式 exec 有超时
	if req.Tty {
		// TTY 模式：等待用户主动退出（如 Ctrl+D 或 exit 命令）
		// 没有超时限制，因为用户可能在执行长时间交互式任务
		_ = execCmd.Wait()
		// exec 进程退出后，关闭 conn 和 Master 触发 I/O goroutine 退出
		closeOnce.Do(func() {
			_ = conn.Close()
			_ = execPTY.Master.Close()
		})
		ioWg.Wait() // 等待 I/O 转发 goroutine 退出
		// I/O goroutine 全部退出后，关闭 Slave（如果还没关闭的话）
		// 注意：此时 Master 已在 closeOnce 中关闭，execPTY.Close() 只会尝试关闭 Slave
		if execPTY.Slave != nil {
			execPTY.Slave.Close()
			execPTY.Slave = nil
		}
	} else {
		// 非 TTY 模式：设置 60 秒超时，防止命令卡死
		done := make(chan error, 1)
		go func() {
			done <- execCmd.Wait()
		}()

		select {
		case <-done:
		case <-time.After(60 * time.Second):
			if execCmd.Process != nil {
				execCmd.Process.Kill()
			}
			<-done
		}
	}
}

// handleAttach 处理 attach 请求
// 连接到容器已有的主进程，共享其 stdin/stdout/stderr。
func handleAttach(conn net.Conn, ctx *shimContext, req *types.ShimRequest) {
	if ctx.containerPTY == nil || ctx.containerPTY.Master == nil {
		sendError(conn, "容器未启用 TTY")
		return
	}

	// 生成唯一的连接 ID，用于管理多个 attach 连接
	// 参考 Docker/containerd-shim: 每个 attach 客户端独立管理，支持动态添加和移除
	connID := fmt.Sprintf("attach-%d-%d", time.Now().UnixNano(), unix.Getpid())

	// 将新连接添加到连接池
	//一个容器可以被多个客户端同时 attach
	//┌─ Client A (docker attach) ──┐
	//│                              │
	//├─ Client B (docker attach) ──┤──→ shim ──→ PTY Master ──→ 容器进程
	//│                              │
	//└─ Client C (docker logs -f) ─┘
	//每个 attach 连接独立管理，一个客户端断开不影响其他客户端。这就是为什么用 map[string]*attachConn 而不是单个 net.Conn
	ctx.attachMu.Lock()
	ctx.attachConns[connID] = &attachConn{
		id:     connID,
		conn:   conn,
		closed: false,
	}
	ctx.attachMu.Unlock()

	// 启动广播 goroutine（只启动一次）
	// 输出（一对多，广播）:
	//   容器 stdout ──→ PTY Slave ──→ PTY Master ──→ Read() ──┬──→ Client A
	//                                                        ├──→ Client B
	//                                                        └──→ Client C
	// 注意：多个 attach 连接共享同一个 PTY Master，不能用多个 goroutine 分别 Read Master，
	// 因为 PTY Master 的 Read 是竞争性的（数据会被随机分配给某个 reader），
	// 必须由一个 goroutine 统一 Read 再广播给所有客户端
	ctx.broadcastOnce.Do(func() {
		go func() {
			buf := make([]byte, 4096)
			for {
				n, err := ctx.containerPTY.Master.Read(buf)
				if err != nil {
					// 退出条件:1.容器进程退出 → Slave 端所有 FD 关闭 → 内核在 Master 端产生 EOF → Master.Read() 返回 EOF
					//           2.shim 关闭 Master（shutdown 时 containerPTY.Close()）
					break
				}
				// 广播到所有 attach 客户端
				ctx.attachMu.RLock()
				for _, ac := range ctx.attachConns {
					if !ac.closed {
						if _, writeErr := ac.conn.Write(buf[:n]); writeErr != nil {
							ac.closed = true
						}
					}
				}
				ctx.attachMu.RUnlock()
			}
			// Master EOF → 容器退出，关闭所有 attach 连接
			// 这也会触发各 handleAttach 中的 io.Copy(Master, conn) 退出，
			// 从而让 done channel 关闭，handleAttach 正常返回，不会 goroutine 泄漏
			ctx.attachMu.Lock()
			for _, ac := range ctx.attachConns {
				if !ac.closed {
					ac.closed = true
					ac.conn.Close()
				}
			}
			ctx.attachConns = make(map[string]*attachConn)
			ctx.attachMu.Unlock()
		}()
	})

	// 输入方向: conn ──io.Copy──→ PTY Master ──(内核PTM)──→ Slave ──→ 容器stdin
	// 输入（多对一，合并）:
	//   Client A ──┐
	//   Client B ──┼──→ io.Copy ──→ PTY Master ──→ PTY Slave ──→ 容器进程 stdin
	//   Client C ──┘     (各自独立 Copy)
	// 每个 attach 连接独立启动输入转发 goroutine，多个客户端的输入合并写入同一个 PTY Master
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = io.Copy(ctx.containerPTY.Master, conn) // 退出条件:daemon 关闭 conn 的写端（用户退出终端 / Ctrl+D），或广播 goroutine 关闭 conn
	}()

	// I/O 转发 goroutine 已就绪，现在通知 Daemon 和 shim 主 goroutine
	// 先告诉 Daemon "attach 成功了，接下来走流式通道"
	// 再触发 attachReady，让 shim 主 goroutine 可以执行 runtime start
	// 顺序很重要：先发响应再触发信号，确保 Daemon 收到响应后 shim 才可能启动容器
	json.NewEncoder(conn).Encode(types.ShimResponse{Success: true, Stream: true})
	ctx.attachOnce.Do(func() { close(ctx.attachReady) })

	// 阻塞住当前 handleShimConn goroutine，等待当前 attach 连接断开
	// 参考 Docker: 每个 attach 连接独立生命周期，断开不影响其他连接
	<-done

	// 清理当前连接
	ctx.attachMu.Lock()
	if ac, ok := ctx.attachConns[connID]; ok {
		ac.closed = true
		ac.conn.Close()
		delete(ctx.attachConns, connID)
	}
	ctx.attachMu.Unlock()
	//场景 									退出链路
	//容器退出 → Master EOF 					广播 goroutine 退出 → 关闭所有 conn → io.Copy 返回 → done 关闭 → handleAttach 返回
	//客户端断开 								conn io.Copy(Master, conn) 读到 EOF → done 关闭 → handleAttach 清理 → 广播跳过
	//shim shutdown → containerPTY.Close()  Master.Read 返回 err → 同容器退出路径
}

// handleResize 处理 resize 请求
func handleResize(conn net.Conn, ctx *shimContext, req *types.ShimRequest) {
	if ctx.containerPTY == nil || ctx.containerPTY.Master == nil {
		sendError(conn, "容器未启用 TTY")
		return
	}
	if err := ctx.containerPTY.SetWinsize(req.Rows, req.Cols); err != nil {
		sendError(conn, err.Error())
		return
	}
	sendSuccessRaw(conn)
}

// handleStart 处理 start 请求
func handleStart(conn net.Conn, ctx *shimContext, req *types.ShimRequest) {
	ctx.startOnce.Do(func() { close(ctx.startReady) })
	sendSuccessRaw(conn)
}

// handlePause 处理 pause 请求
// 对齐 Docker/containerd: shim 通过调用 runtime pause 暂停容器，而非直接访问 libcontainer
func handlePause(conn net.Conn, ctx *shimContext, req *types.ShimRequest) {
	if err := runRuntimeCommand("pause", ctx.containerID); err != nil {
		sendErrorf(conn, "暂停容器失败: %v", err)
		return
	}
	sendSuccessRaw(conn)
}

// handleUnpause 处理 unpause 请求
// 对齐 Docker/containerd: shim 通过调用 runtime resume 恢复容器，而非直接访问 libcontainer
func handleUnpause(conn net.Conn, ctx *shimContext, req *types.ShimRequest) {
	if err := runRuntimeCommand("resume", ctx.containerID); err != nil {
		sendErrorf(conn, "恢复容器失败: %v", err)
		return
	}
	sendSuccessRaw(conn)
}

// handleShutdown 处理 shutdown 请求
func handleShutdown(conn net.Conn, ctx *shimContext, req *types.ShimRequest) {
	sendSuccessRaw(conn)
	ctx.shutdownOnce.Do(func() { close(ctx.shutdownDone) })
}
