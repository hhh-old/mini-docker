//go:build linux

package shim

import (
	"encoding/json"
	"fmt"
	"log"
	"mini-docker/utils"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"

	"mini-docker/constants"
	"mini-docker/pty"
	"mini-docker/types"

	"golang.org/x/sys/unix"
)

const (
	shimDir = constants.ShimDir
)

// ---------------------------------------------------------------------------
// Shim 进程主入口
// ---------------------------------------------------------------------------

// 关于shim进程：

//┌─────────────┐     exec.Command     ┌─────────────┐
//│   Daemon    │─────────────────────→│    shim     │
//│   进程      │    Setsid: true      │    进程      │
//└─────────────┘                      └─────────────┘
//                                            │
//                    ┌───────────────────────┼───────────────────────┐
//                    │                       │                       │
//                    ▼                       ▼                       ▼
//              /dev/null          shim/<id>.log            shim/<id>.log
//              (Stdin)            (Stdout)                 (Stderr)

// 流 			指向 										说明
// Stdin 		/dev/null 									shim 是后台进程，不需要输入
// Stdout 		<daemon_log_dir>/shim/<containerID>.log 	日志文件
// Stderr 		同上（同一个 logFile） 						日志文件

// Run shim 进程主入口（对标 containerd-shim）
// 参数: containerID, bundlePath [--tty] [--takeover <pid>]
// 流程:
//  1. 设置 PR_SET_CHILD_SUBREAPER（收养孤儿进程）
//  2. 创建控制 socket
//  3. 调用 runtime create（exec 子进程，等待退出）
//  4. 调用 runtime start（exec 子进程，等待退出）
//  5. 等待容器进程退出（回收僵尸）
//  6. 保存退出信息
//  7. 保持运行，提供控制 socket 服务
func Run(args []string) {
	if len(args) < 2 {
		fmt.Fprintf(os.Stderr, "用法: mini-docker shim <containerID> <bundlePath> [--tty] [--takeover <pid>]\n")
		os.Exit(1)
	}

	containerID := args[0]
	bundlePath := args[1]

	// 解析可选参数
	// 对齐 Docker/containerd: TTY 信息由上层（Daemon/containerd）通过参数传递给 shim
	// 避免 shim 直接解析 OCI Spec，保持分层架构的清晰性
	isTTY := false
	takeoverPID := 0
	for i := 2; i < len(args); i++ {
		if args[i] == "--tty" {
			isTTY = true
		} else if args[i] == "--takeover" && i+1 < len(args) {
			takeoverPID, _ = strconv.Atoi(args[i+1])
			i++
		}
	}

	if takeoverPID > 0 {
		if err := runTakeover(containerID, bundlePath, takeoverPID); err != nil {
			fmt.Fprintf(os.Stderr, "shim takeover 错误: %v\n", err)
			os.Exit(1)
		}
	} else {
		if err := run(containerID, bundlePath, isTTY); err != nil {
			fmt.Fprintf(os.Stderr, "shim 错误: %v\n", err)
			os.Exit(1)
		}
	}
}

// ---------------------------------------------------------------------------
// 核心运行逻辑
// ---------------------------------------------------------------------------

// shim 进程与容器进程是"寄生"关系：它直接管理该容器的生命周期，持久化托管其 I/O 流和终端，并在容器进程退出时收集其状态码。
// 要把Bundle传递给容器运行时，容器运行时根据容器文件系统包（Container Filesystem Bundle）来启动容器
func run(containerID, bundlePath string, isTTY bool) (retErr error) {
	// panic 恢复：防止 shim 因意外 panic 而崩溃导致容器失控
	// 对齐 Docker: containerd-shim 也有类似的 panic recovery 机制
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[shim] 容器 %s: 发生 panic: %v\n", containerID, r)
			retErr = fmt.Errorf("shim panic: %v", r)
		}
	}()

	// 1. 设置子进程收割者（Subreaper 机制）
	if err := setupSubreaper(); err != nil {
		return err
	}

	// 2. 创建 shim 目录和控制 socket
	shimContainerDir := filepath.Join(shimDir, containerID)
	listener, err := createControlSocket(shimContainerDir, containerID)
	if err != nil {
		return err
	}
	defer listener.Close()

	// 3. 写 shim PID 文件
	writeShimPID(shimContainerDir)

	// 4. 初始化 PTY 和日志文件
	containerPTY, logFile, err := initPTYAndLog(shimContainerDir, containerID, isTTY)
	if err != nil {
		return err
	}

	// 5. 创建 shimContext
	ctx := createShimContext(containerID, containerPTY, logFile)
	go serveControlSocket(listener, ctx)

	// 6. 调用 runtime create
	if err := runtimeCreate(containerID, bundlePath, containerPTY, logFile, isTTY); err != nil {
		cleanupPTYAndLog(containerPTY, logFile)
		return fmt.Errorf("runtime create 失败: %w", err)
	}

	// 7. 通过 runtime state 获取容器 PID
	containerPID, err := getContainerPIDFromRuntime(containerID)
	if err != nil {
		cleanupPTYAndLog(containerPTY, logFile)
		return fmt.Errorf("通过 runtime 获取容器 PID 失败: %w", err)
	}
	//向ctx中写入containerPID，要加锁，因为handleShimConn goroutine（多个）中在containerPID := ctx.containerPID读取，两者是并发的
	ctx.pidMu.Lock()
	ctx.containerPID = containerPID
	ctx.pidMu.Unlock()
	log.Printf("[shim] 容器 %s: PID=%d, 状态=created\n", containerID, containerPID)

	// 写入 created 文件，通知 Daemon 容器已创建（PID 可用）
	createdPath := filepath.Join(shimContainerDir, "created")
	if err := os.WriteFile(createdPath, []byte(fmt.Sprintf("%d", containerPID)), 0644); err != nil {
		log.Printf("写入文件 %s 失败: %v\n", createdPath, err)
	}

	// 8. 等待 Daemon 发送 start 信号（对齐 Docker: create 与 start 分离）
	// Docker 流程: runc create → 设置网络/cgroup → runc start
	// mini-docker: runtime create → Daemon 设置网络 → shim 收到 start → runtime start
	log.Printf("[shim] 容器 %s: 等待 start 信号...\n", containerID)
	select {
	case <-ctx.startReady:
		log.Printf("[shim] 容器 %s: 收到 start 信号\n", containerID)
	case <-ctx.shutdownDone:
		log.Printf("[shim] 容器 %s: 收到 shutdown 信号，退出\n", containerID)
		return nil
	case <-time.After(5 * time.Minute):
		log.Printf("[shim] 容器 %s: 等待 start 信号超时\n", containerID)
		return nil
	}

	// 9. TTY 模式下等待 attach 连接
	if isTTY {
		//完整调用链(-it 模式):
		//用户执行: mini-docker run -it ubuntu /bin/sh
		//                    │
		//                    ▼
		//         ┌─ CLI (runInteractive) ─────────────────────────────────────┐
		//         │  通过 Unix Socket 发送 {Type: "run", stream: "true"}      │
		//         └────────────────────────────────────────────────────────────┘
		//                    │
		//                    ▼
		//         ┌─ Daemon (handleRun) ──────────────────────────────────────┐
		//         │  1. 启动 shim 进程                                        │
		//         │  2. 轮询等待 shim 写 created 文件                          │
		//         │  3. 设置网络                                               │
		//         │  4. ❶ d.service.AttachTask(containerID)   ← 先 attach!    │
		//         │     └─ 连接 shim socket，发送 {Type: "attach"}            │
		//         │  5. ❷ d.service.StartTask(containerID)    ← 再 start!     │
		//         │     └─ 连接 shim socket，发送 {Type: "start"}             │
		//         └───────────────────────────────────────────────────────────┘
		//                    │                        │
		//                    │ attach 请求             │ start 请求
		//                    ▼                        ▼
		//         ┌─ Shim (serveControlSocket) ───────────────────────────────┐
		//         │                                                           │
		//         │  收到 "attach" 请求:                                       │
		//         │    handleAttach() → close(ctx.attachReady)  ❶ 发出信号    │
		//         │                                                           │
		//         │  主 goroutine 被 <-ctx.attachReady 解除阻塞               │
		//         │    ↓                                                      │
		//         │  执行 runtime start（容器进程开始运行）                     │
		//         │                                                           │
		//         │  收到 "start" 请求:                                       │
		//         │    handleStart() → close(ctx.startReady)                  │
		//         └───────────────────────────────────────────────────────────┘
		log.Printf("[shim] 容器 %s: 等待 attach 连接...\n", containerID)
		select {
		case <-ctx.attachReady:
			log.Printf("[shim] 容器 %s: attach 已连接, 启动容器\n", containerID)
		case <-time.After(30 * time.Second):
			log.Printf("[shim] 容器 %s: attach 超时, 直接启动容器\n", containerID)
		}
	}

	// 10. 调用 runtime start
	log.Printf("[shim] 容器 %s: 调用 runtime start\n", containerID)
	startCmd := exec.Command("/proc/self/exe", "runtime", "start", containerID)
	// 将新启动的 runtime start 子进程的 Stdout（标准输出）和 Stderr（标准错误），绑定到当前 shim 进程的 Stdout 和 Stderr 上
	// 通过这行重定向，runtime create 在创建容器时输出的所有日志、甚至报错崩溃信息（比如 Namespace 创建失败、pivot_root 失败等），都会被自动、原封不动地写入到 shim 进程的输入输出中。
	startCmd.Stdout = os.Stdout
	startCmd.Stderr = os.Stderr
	if err := startCmd.Run(); err != nil {
		if containerPTY != nil {
			containerPTY.Close()
		}
		return fmt.Errorf("runtime start 失败: %w", err)
	}

	log.Printf("[shim] 容器 %s: 用户进程已启动 (PID=%d)\n", containerID, containerPID)

	// 11. 异步等待容器进程退出
	go waitContainerExit(containerPID, ctx.exitInfo, shimContainerDir, containerID, ctx.exitReady, &ctx.exitOnce)

	// 12. 阻塞等待 shutdown 信号
	<-ctx.shutdownDone       // 1. 阻塞等待关闭信号
	if containerPTY != nil { // 2. 收到信号，关闭伪终端主设备
		containerPTY.Close()
	}
	if logFile != nil {
		logFile.Close() // 3. 关闭日志文件
	}
	return nil // 4. 函数返回，Shim 进程退出
}

// ---------------------------------------------------------------------------
// 辅助函数
// ---------------------------------------------------------------------------

// setupSubreaper 设置 PR_SET_CHILD_SUBREAPER，收养孤儿进程
// 在 Linux 中，当父进程退出后，子进程会变成"孤儿进程"，默认会被系统 PID 1（systemd 等）收养。
// 当 mini-docker runtime 命令执行完 create 和 start 退出后，容器进程就成了孤儿。为了让 shim 能够监控并回收容器，代码第一步就执行了：
//
//	unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0)
//
// 原理：这会告诉 Linux 内核，将此 shim 标记为 Subreaper（子孙进程收割者）。之后，所有由 runtime 创建、因 runtime 退出而成为孤儿的容器进程，都会被重定向 reparent（收养）到这个 shim 下。
// 这样，shim 就可以合法地对容器 PID 调用 wait 相关的系统调用，防止其变成"僵尸进程"，并精准捕捉其退出状态。shim进程不收养孤儿进程则shim无法wait容器进程。
func setupSubreaper() error {
	if err := unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0); err != nil {
		return fmt.Errorf("设置 PR_SET_CHILD_SUBREAPER 失败: %w", err)
	}
	return nil
}

// createControlSocket 创建 shim 目录和控制 socket
func createControlSocket(shimContainerDir, containerID string) (net.Listener, error) {
	if err := os.MkdirAll(shimContainerDir, 0755); err != nil {
		return nil, fmt.Errorf("创建 shim 目录失败: %w", err)
	}

	socketPath := filepath.Join(shimContainerDir, "shim.sock")
	os.Remove(socketPath)
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		return nil, fmt.Errorf("创建控制 socket 失败: %w", err)
	}
	return listener, nil
}

// writeShimPID 写入 shim PID 文件
func writeShimPID(shimContainerDir string) {
	pidPath := filepath.Join(shimContainerDir, "shim.pid")
	if err := os.WriteFile(pidPath, []byte(fmt.Sprintf("%d", os.Getpid())), 0644); err != nil {
		log.Printf("写入文件 %s 失败: %v\n", pidPath, err)
	}
}

// initPTYAndLog 初始化 PTY 和日志文件
// isTTY 由上层（Daemon/containerd）通过命令行参数传递
// 对齐 Docker/containerd: shim 不直接解析 OCI Spec，保持分层架构的清晰性
// 伪终端（PTY）与日志的持续托管
// 如果用户启动了交互式容器（-it），容器需要绑定一个终端。
// shim 通过 pty.Open() 打开一对伪终端：Master（主设备，留在 shim 侧）和 Slave（从设备，传递给容器作为输入输出）。
// 即便最外层的客户端连接（如用户拔掉网线、CLI 退出），由于 shim 依然拿着 Master 端，容器的终端就不会由于收到 SIGHUP 信号而崩溃。
func initPTYAndLog(shimContainerDir, containerID string, isTTY bool) (*pty.PTY, *os.File, error) {
	if isTTY {
		containerPTY, err := pty.Open()
		if err != nil {
			return nil, nil, fmt.Errorf("创建 pty 失败: %w", err)
		}
		log.Printf("[shim] 容器 %s: TTY 模式, pty slave=%s\n", containerID, containerPTY.Name)
		return containerPTY, nil, nil
	}

	// 非 TTY 模式：创建日志文件
	logPath := filepath.Join(shimContainerDir, "container.log")
	// 日志轮转：如果日志文件超过最大大小，先截断（防止日志无限增长）
	if info, err := os.Stat(logPath); err == nil && info.Size() > constants.MaxContainerLogSize {
		os.Truncate(logPath, 0)
	}
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return nil, nil, fmt.Errorf("创建日志文件失败: %w", err)
	}

	return nil, logFile, nil
}

// cleanupPTYAndLog 清理 PTY 和日志文件
func cleanupPTYAndLog(containerPTY *pty.PTY, logFile *os.File) {
	if containerPTY != nil {
		containerPTY.Close()
	}
	if logFile != nil {
		logFile.Close()
	}
}

// createShimContext 创建 shimContext
func createShimContext(containerID string, containerPTY *pty.PTY, logFile *os.File) *shimContext {
	exitInfo := &types.ExitInfo{}
	exitReady := make(chan struct{})    // 容器进程退出信号
	shutdownDone := make(chan struct{}) // Shim 进程退出与销毁信号,谁来触发:由外部（Daemon 客户端）通过控制套接字发送 "shutdown" 请求来触发
	attachReady := make(chan struct{})  //Daemon 告诉 shim "用户终端已连接，可以安全启动容器了"
	//- shim 创建容器后（ runtime create ）， 阻塞等待startReady这个信号
	//- Daemon 设置好网络、cgroup 等资源后，发送 "start" 请求给 shim
	//- shim 收到后关闭 startReady channel，继续执行 runtime start
	startReady := make(chan struct{})
	return &shimContext{
		containerID:  containerID,
		exitReady:    exitReady,
		exitInfo:     exitInfo,
		shutdownDone: shutdownDone,
		containerPTY: containerPTY,
		attachReady:  attachReady,
		startReady:   startReady,
		logFile:      logFile,
		attachConns:  make(map[string]*attachConn),
	}
}

// runtimeCreate 调用 runtime create 创建容器
func runtimeCreate(containerID, bundlePath string, containerPTY *pty.PTY, logFile *os.File, isTTY bool) error {
	// shim 构建参数的逻辑:
	// 基础参数（始终存在）：
	//  "runtime", "create", <containerID>, "--bundle", <bundlePath>
	//
	// TTY 模式追加：
	//  "--console", <ptyName>
	//
	// 非 TTY 模式追加：
	//  "--stdout-fd", "3", "--stderr-fd", "3"
	//
	// ### 实际调用示例
	// TTY 模式 ( -it )：
	// /proc/self/exe runtime create abc123 --bundle /var/lib/mini-docker/runtime/abc123/bundle --console /dev/pts/0
	//
	// 非 TTY 模式 ( -d )：
	// /proc/self/exe runtime create abc123 --bundle /var/lib/mini-docker/runtime/abc123/bundle --stdout-fd 3 --stderr-fd 3

	log.Printf("[shim] 容器 %s: 调用 runtime create\n", containerID)
	createArgs := []string{"runtime", "create", containerID, "--bundle", bundlePath}

	if isTTY {
		// TTY 模式：传递 PTY 设备路径
		createArgs = append(createArgs, "--console", containerPTY.Name)
		// 在 Linux 的伪终端（PTY）管理和容器运行时设计中，这里关闭 Slave（从设备）句柄，主要原因：
		// 确保 EOF（进程退出信号）的正确传递（最核心的原因）
		// 这是 Linux 伪终端机制的一条硬性规则：
		// 规则：只有当所有引用了 PTY Slave（从设备）端的文件描述符（FD）都被关闭后，内核才会在 PTY Master（主设备）端产生一个 EOF（在 Go 中读取会表现为 io.EOF 或特定的 EIO 错误）。
		// 后果：如果 Shim 进程不关闭自己手中的 containerPTY.Slave 句柄，那么即便容器内部的 Shell 进程（如 /bin/sh）已经退出、且容器内所有对 Slave 的引用都已释放，由于 Shim 进程还拿着这个 Slave 句柄，内核就不会在 Master 端触发 EOF。
		// 影响：这会导致 io.Copy(conn, containerPTY.Master) 拷贝协程永远阻塞在等待读取上，客户端也就无法感知到容器已经退出，最终导致控制台挂起、连接泄露和资源无法回收。
		// 因此，Shim 必须在完成"桥梁"搭建后，第一时间斩断自己与 Slave 的直接文件描述符引用。
		containerPTY.Slave.Close() // Runtime 会自己重新打开 Slave 设备
		containerPTY.Slave = nil
	} else {
		// 非 TTY 模式：直接传递日志文件 fd 给容器进程
		// 容器进程的 stdout 和 stderr 直接写入日志文件，无需管道中转
		// 优势：shim 崩溃时容器不会收到 SIGPIPE 信号（因为没有管道读端被关闭），非 TTY 容器可存活
		// 对齐 Docker：Docker 的 containerd-shim 也使用日志文件直接写入方式
		//
		// 容器内进程 ──(输出)──> stdout (FD 1) ──> 日志文件 (FD 3)
		// 容器内进程 ──(输出)──> stderr (FD 2) ──> 日志文件 (FD 3)
		// （stdout 和 stderr 都指向同一个日志文件 fd，以 O_APPEND 模式写入保证原子性）
		// 注意：FD 3 是通过 cmd.ExtraFiles 传入的，ExtraFiles 中的文件在子进程中从 FD 3 开始编号
		createArgs = append(createArgs,
			"--stdout-fd", fmt.Sprintf("%d", 3),
			"--stderr-fd", fmt.Sprintf("%d", 3))
	}

	createCmd := exec.Command("/proc/self/exe", createArgs...)
	createCmd.Stdout = os.Stdout
	createCmd.Stderr = os.Stderr
	if !isTTY && logFile != nil {
		// 通过 ExtraFiles 将日志文件作为 FD 3 传给 runtime create 子进程
		// runtime 子进程通过 --stdout-fd 3 --stderr-fd 3 参数知道 FD 3 是日志文件
		// 从而将容器的 stdout/stderr 重定向到日志文件
		createCmd.ExtraFiles = []*os.File{logFile}
	}

	return createCmd.Run()
}

// waitContainerExit 等待容器进程退出并保存退出信息
func waitContainerExit(containerPID int, exitInfo *types.ExitInfo, shimContainerDir, containerID string, exitReady chan struct{}, exitOnce *sync.Once) {
	exitCode := waitForContainerExit(containerPID) // 1. 阻塞等待系统调用 Wait4 返回,等待容器进程结束
	exitInfo.ExitCode = exitCode
	exitInfo.ExitedAt = time.Now().Format(time.RFC3339)

	exitData, _ := json.Marshal(exitInfo)
	exitPath := filepath.Join(shimContainerDir, "exit.json")
	if err := os.WriteFile(exitPath, exitData, 0644); err != nil {
		log.Printf("写入文件 %s 失败: %v\n", exitPath, err)
	}

	log.Printf("[shim] 容器 %s: 已退出 (exit_code=%d)\n", containerID, exitCode)

	// 通知 handleExitInfo 容器已退出，Daemon 可立即获取退出信息
	exitOnce.Do(func() { close(exitReady) })
}

// ---------------------------------------------------------------------------
// 网络和控制
// ---------------------------------------------------------------------------

func serveControlSocket(listener net.Listener, ctx *shimContext) {
	for {
		conn, err := listener.Accept()
		if err != nil {
			select {
			case <-ctx.shutdownDone:
				return
			default:
			}
			continue
		}
		go handleShimConn(conn, ctx)
	}
}

func handleShimConn(conn net.Conn, ctx *shimContext) {
	// TTY exec 模式下 conn 由 I/O 转发 goroutine 使用，不能在这里关闭
	// 使用标志控制是否关闭 conn
	closeConn := true
	defer func() {
		if closeConn {
			conn.Close()
		}
	}()

	ctx.pidMu.Lock()
	containerPID := ctx.containerPID
	ctx.pidMu.Unlock()

	var req types.ShimRequest
	if err := json.NewDecoder(conn).Decode(&req); err != nil {
		json.NewEncoder(conn).Encode(types.ShimResponse{Success: false, Message: err.Error()})
		return
	}

	switch req.Type {
	case ReqState:
		handleState(conn, ctx, &req)
	case ReqKill:
		handleKill(conn, ctx, &req)
	case ReqExitInfo:
		handleExitInfo(conn, ctx, &req)
	case ReqExec:
		handleExec(conn, ctx, &req, containerPID)
		// TTY 模式下 conn 由 I/O 转发 goroutine 使用，已经处理完毕
		// 设置 closeConn = false，避免 defer 重复关闭
		if req.Tty {
			closeConn = false
		}
	case ReqAttach:
		handleAttach(conn, ctx, &req)
		closeConn = false
	case ReqResize:
		handleResize(conn, ctx, &req)
	case ReqStart:
		handleStart(conn, ctx, &req)
	case ReqPause:
		handlePause(conn, ctx, &req)
	case ReqUnpause:
		handleUnpause(conn, ctx, &req)
	case ReqShutdown:
		handleShutdown(conn, ctx, &req)
	default:
		json.NewEncoder(conn).Encode(types.ShimResponse{Success: false, Message: fmt.Sprintf("未知请求: %s", req.Type)})
	}
}

// ---------------------------------------------------------------------------
// Takeover 模式
// ---------------------------------------------------------------------------

// runTakeover shim 接管模式：shim 崩溃后重启，接管已有容器进程
// 非 TTY 容器在 shim 崩溃后仍可存活（因为日志文件 fd 不受 shim 影响），
// 此时启动新的 shim 以 takeover 模式接管容器，恢复 Wait4 监控和控制 socket 服务
func runTakeover(containerID, bundlePath string, containerPID int) (retErr error) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[shim] 容器 %s: takeover 模式发生 panic: %v\n", containerID, r)
			retErr = fmt.Errorf("shim takeover panic: %v", r)
		}
	}()

	// 设置子进程收割者（收养孤儿进程）
	if err := setupSubreaper(); err != nil {
		return err
	}

	// 创建 shim 目录和控制 socket
	shimContainerDir := filepath.Join(shimDir, containerID)
	listener, err := createControlSocket(shimContainerDir, containerID)
	if err != nil {
		return err
	}
	defer listener.Close()

	// 写入 shim PID 文件
	writeShimPID(shimContainerDir)

	pid := containerPID
	exitInfo := &types.ExitInfo{}
	exitReady := make(chan struct{})
	shutdownDone := make(chan struct{})
	startReady := make(chan struct{})
	attachReady := make(chan struct{})

	ctx := &shimContext{
		containerID:  containerID,
		containerPID: pid,
		exitReady:    exitReady,
		exitInfo:     exitInfo,
		shutdownDone: shutdownDone,
		startReady:   startReady,
		attachReady:  attachReady,
		logFile:      nil,
		containerPTY: nil,
		attachConns:  make(map[string]*attachConn),
	}

	go serveControlSocket(listener, ctx)

	log.Printf("[shim] 容器 %s: takeover 模式，接管 PID=%d\n", containerID, containerPID)

	// 等待容器进程退出
	go waitContainerExit(containerPID, exitInfo, shimContainerDir, containerID, exitReady, &ctx.exitOnce)

	<-shutdownDone
	return nil
}

// ---------------------------------------------------------------------------
// 进程等待
// ---------------------------------------------------------------------------

func waitForContainerExit(pid int) int {
	for {
		var status syscall.WaitStatus
		wpid, err := syscall.Wait4(pid, &status, 0, nil)
		if err != nil {
			if err == syscall.ECHILD {
				if !utils.CheckProcessAlive(pid) {
					return -1
				}
				time.Sleep(100 * time.Millisecond)
				continue
			}
			return 1
		}
		if wpid == pid {
			if status.Exited() {
				return status.ExitStatus()
			}
			if status.Signaled() {
				return 128 + int(status.Signal())
			}
			return 1
		}
	}
}
