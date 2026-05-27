//go:build linux

package shim

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"

	"mini-docker/constants"
	"mini-docker/libcontainer"
	"mini-docker/pty"
	"mini-docker/spec"
	"mini-docker/types"
	"mini-docker/utils"

	"golang.org/x/sys/unix"
)

const (
	shimDir = constants.ShimDir
)

type shimContext struct {
	containerID  string
	containerPID *int
	pidMu        *sync.Mutex
	exitReady    <-chan struct{}
	exitInfo     *types.ExitInfo
	shutdownDone chan struct{}
	shutdownOnce *sync.Once
	containerPTY *pty.PTY
	attachReady  chan struct{}
	startReady   chan struct{}
	startOnce    *sync.Once
	attachOnce   *sync.Once
	logFile      *os.File
}

// Run shim 进程主入口（对标 containerd-shim）
// 参数: containerID, bundlePath, configJSON
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
		fmt.Fprintf(os.Stderr, "用法: mini-docker shim <containerID> <bundlePath> [--takeover <pid>]\n")
		os.Exit(1)
	}

	containerID := args[0]
	bundlePath := args[1]

	// 解析可选参数：--takeover <pid> 用于 shim 崩溃后重启接管已有容器
	takeoverPID := 0
	for i := 2; i < len(args); i++ {
		if args[i] == "--takeover" && i+1 < len(args) {
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
		if err := run(containerID, bundlePath); err != nil {
			fmt.Fprintf(os.Stderr, "shim 错误: %v\n", err)
			os.Exit(1)
		}
	}
}

// shim 进程与容器进程是"寄生"关系：它直接管理该容器的生命周期，持久化托管其 I/O 流和终端，并在容器进程退出时收集其状态码。
func run(containerID, bundlePath string) (retErr error) {
	// panic 恢复：防止 shim 因意外 panic 而崩溃导致容器失控
	// 对齐 Docker: containerd-shim 也有类似的 panic recovery 机制
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[shim] 容器 %s: 发生 panic: %v\n", containerID, r)
			retErr = fmt.Errorf("shim panic: %v", r)
		}
	}()

	//  进程孤儿与僵尸回收（Subreaper 机制）
	//在 Linux 中，当父进程退出后，子进程会变成"孤儿进程"，默认会被系统 PID 1（systemd 等）收养。
	//当 mini-docker runtime 命令执行完 create 和 start 退出后，容器进程就成了孤儿。为了让 shim 能够监控并回收容器，代码第一步就执行了：
	//	unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0)
	//原理：这会告诉 Linux 内核，将此 shim 标记为 Subreaper（子孙进程收割者）。之后，所有由 runtime 创建、因 runtime 退出而成为孤儿的容器进程，都会被重定向 reparent（收养）到这个 shim 下。
	//这样，shim 就可以合法地对容器 PID 调用 wait 相关的系统调用，防止其变成"僵尸进程"，并精准捕捉其退出状态。shim进程不收养孤儿进程则shim无法wait容器进程。
	if err := unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0); err != nil {
		return fmt.Errorf("设置 PR_SET_CHILD_SUBREAPER 失败: %w", err)
	}

	// 2. 创建 shim 目录和控制 socket
	shimContainerDir := filepath.Join(shimDir, containerID)
	if err := os.MkdirAll(shimContainerDir, 0755); err != nil {
		return fmt.Errorf("创建 shim 目录失败: %w", err)
	}

	socketPath := filepath.Join(shimContainerDir, "shim.sock")
	os.Remove(socketPath)
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		return fmt.Errorf("创建控制 socket 失败: %w", err)
	}
	defer listener.Close()

	// 3. 写 shim PID 文件
	pidPath := filepath.Join(shimContainerDir, "shim.pid")
	if err := os.WriteFile(pidPath, []byte(fmt.Sprintf("%d", os.Getpid())), 0644); err != nil {
		log.Printf("写入文件 %s 失败: %v\n", pidPath, err)
	}
	//加载容器配置
	ociSpec, err := spec.LoadSpec(bundlePath)
	if err != nil {
		return fmt.Errorf("加载 OCI Spec 失败: %w", err)
	}
	isTTY := ociSpec != nil && ociSpec.Process != nil && ociSpec.Process.Terminal //是否使用前台交互式终端访问容器
	//伪终端（PTY）与日志的持续托管
	//如果用户启动了交互式容器（-it），容器需要绑定一个终端。
	//shim 通过 pty.Open() 打开一对伪终端：Master（主设备，留在 shim 侧）和 Slave（从设备，传递给容器作为输入输出）。
	//即便最外层的客户端连接（如用户拔掉网线、CLI 退出），由于 shim 依然拿着 Master 端，容器的终端就不会由于收到 SIGHUP 信号而崩溃。
	var containerPTY *pty.PTY
	if isTTY {
		containerPTY, err = pty.Open()
		if err != nil {
			return fmt.Errorf("创建 pty 失败: %w", err)
		}
		ptyInfoPath := filepath.Join(shimContainerDir, "pty.info")
		if err := os.WriteFile(ptyInfoPath, []byte(containerPTY.Name), 0644); err != nil {
			log.Printf("写入文件 %s 失败: %v\n", ptyInfoPath, err)
		}
		log.Printf("[shim] 容器 %s: TTY 模式, pty slave=%s\n", containerID, containerPTY.Name)
	}

	logPath := filepath.Join(shimContainerDir, "container.log")
	var logFile *os.File
	if !isTTY { //在磁盘上创建并打开 container.log 文件，准备用于记录容器的 stdout 和 stderr 日志
		// 日志轮转：如果日志文件超过最大大小，先截断（防止日志无限增长）
		if info, err := os.Stat(logPath); err == nil && info.Size() > constants.MaxContainerLogSize {
			os.Truncate(logPath, 0)
		}
		logFile, err = os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
		if err != nil {
			return fmt.Errorf("创建日志文件失败: %w", err)
		}
	}

	var containerPID int
	var pidMu sync.Mutex
	var exitOnce sync.Once
	var shutdownOnce sync.Once
	exitInfo := &types.ExitInfo{}
	exitReady := make(chan struct{})    //容器进程退出信号
	shutdownDone := make(chan struct{}) //Shim 进程退出与销毁信号,谁来触发:由外部（Daemon 客户端）通过控制套接字发送 "shutdown" 请求来触发
	attachReady := make(chan struct{})
	startReady := make(chan struct{})
	var startOnce sync.Once
	var attachOnce sync.Once
	//处理deamon进程发给shim进程的请求
	ctx := &shimContext{
		containerID:  containerID,
		containerPID: &containerPID,
		pidMu:        &pidMu,
		exitReady:    exitReady,
		exitInfo:     exitInfo,
		shutdownDone: shutdownDone,
		shutdownOnce: &shutdownOnce,
		containerPTY: containerPTY,
		attachReady:  attachReady,
		startReady:   startReady,
		startOnce:    &startOnce,
		attachOnce:   &attachOnce,
		logFile:      logFile,
	}
	go serveControlSocket(listener, ctx)
	//shim 构建参数的逻辑:
	//基础参数（始终存在）：
	//  "runtime", "create", <containerID>, "--bundle", <bundlePath>
	//
	//TTY 模式追加：
	//  "--console", <ptyName>
	//
	//非 TTY 模式追加：
	//  "--stdout-fd", "3", "--stderr-fd", "3"
	//
	//### 实际调用示例
	//TTY 模式 ( -it )：
	// /proc/self/exe runtime create abc123 --bundle /var/lib/mini-docker/runtime/abc123/bundle --console /dev/pts/0
	//
	//非 TTY 模式 ( -d )：
	//
	///proc/self/exe runtime create abc123 --bundle /var/lib/mini-docker/runtime/abc123/bundle --stdout-fd 3 --stderr-fd 3

	log.Printf("[shim] 容器 %s: 调用 runtime create\n", containerID)
	createArgs := []string{"runtime", "create", containerID, "--bundle", bundlePath}
	if isTTY && containerPTY != nil {
		createArgs = append(createArgs, "--console", containerPTY.Name)
		//在 Linux 的伪终端（PTY）管理和容器运行时设计中，这里关闭 Slave（从设备）句柄，主要原因：
		//确保 EOF（进程退出信号）的正确传递（最核心的原因）
		//这是 Linux 伪终端机制的一条硬性规则：
		//规则：只有当所有引用了 PTY Slave（从设备）端的文件描述符（FD）都被关闭后，内核才会在 PTY Master（主设备）端产生一个 EOF（在 Go 中读取会表现为 io.EOF 或特定的 EIO 错误）。
		//后果：如果 Shim 进程不关闭自己手中的 containerPTY.Slave 句柄，那么即便容器内部的 Shell 进程（如 /bin/sh）已经退出、且容器内所有对 Slave 的引用都已释放，由于 Shim 进程还拿着这个 Slave 句柄，内核就不会在 Master 端触发 EOF。
		//影响：这会导致 io.Copy(conn, containerPTY.Master) 拷贝协程永远阻塞在等待读取上，客户端也就无法感知到容器已经退出，最终导致控制台挂起、连接泄露和资源无法回收。
		//因此，Shim 必须在完成"桥梁"搭建后，第一时间斩断自己与 Slave 的直接文件描述符引用。
		containerPTY.Slave.Close() // Runtime 会自己重新打开 Slave 设备
		containerPTY.Slave = nil
	}
	if !isTTY {
		// 非 TTY 模式：直接传递日志文件 fd 给容器进程
		// 容器进程的 stdout 和 stderr 直接写入日志文件，无需管道中转
		// 优势：shim 崩溃时容器不会收到 SIGPIPE 信号（因为没有管道读端被关闭），非 TTY 容器可存活
		// 对齐 Docker：Docker 的 containerd-shim 也使用日志文件直接写入方式
		//
		//容器内进程 ──(输出)──> stdout (FD 1) ──> 日志文件 (FD 3)
		//容器内进程 ──(输出)──> stderr (FD 2) ──> 日志文件 (FD 3)
		//（stdout 和 stderr 都指向同一个日志文件 fd，以 O_APPEND 模式写入保证原子性）
		createArgs = append(createArgs,
			"--stdout-fd", fmt.Sprintf("%d", 3),
			"--stderr-fd", fmt.Sprintf("%d", 3))
		createCmd := exec.Command("/proc/self/exe", createArgs...)
		createCmd.Stdout = os.Stdout
		createCmd.Stderr = os.Stderr
		createCmd.ExtraFiles = []*os.File{logFile}
		if err := createCmd.Run(); err != nil {
			logFile.Close()
			if containerPTY != nil {
				containerPTY.Close()
			}
			return fmt.Errorf("runtime create 失败: %w", err)
		}
	} else {
		createCmd := exec.Command("/proc/self/exe", createArgs...)
		//将新启动的 runtime create 子进程的 Stdout（标准输出）和 Stderr（标准错误），绑定到当前 shim 进程的 Stdout 和 Stderr 上
		//通过这行重定向，runtime create 在创建容器时输出的所有日志、甚至报错崩溃信息（比如 Namespace 创建失败、pivot_root 失败等），都会被自动、原封不动地写入到 shim 进程的s输入输出中。
		//注意runtime create 进程并不是容器进程，容器进程是由他创建的进程
		createCmd.Stdout = os.Stdout
		createCmd.Stderr = os.Stderr
		if err := createCmd.Run(); err != nil {
			if containerPTY != nil {
				containerPTY.Close()
			}
			return fmt.Errorf("runtime create 失败: %w", err)
		}
	}

	c, err := libcontainer.Load(containerID)
	if err != nil {
		if containerPTY != nil {
			containerPTY.Close()
		}
		return fmt.Errorf("加载容器失败: %w", err)
	}
	pidMu.Lock()
	containerPID = c.Pid()
	pidMu.Unlock()
	log.Printf("[shim] 容器 %s: PID=%d, 状态=created\n", containerID, containerPID)

	// 写入 created 文件，通知 Daemon 容器已创建（PID 可用）
	createdPath := filepath.Join(shimContainerDir, "created")
	if err := os.WriteFile(createdPath, []byte(fmt.Sprintf("%d", containerPID)), 0644); err != nil {
		log.Printf("写入文件 %s 失败: %v\n", createdPath, err)
	}

	// 等待 Daemon 发送 start 信号（对齐 Docker: create 与 start 分离）
	// Docker 流程: runc create → 设置网络/cgroup → runc start
	// mini-docker: runtime create → Daemon 设置网络 → shim 收到 start → runtime start
	log.Printf("[shim] 容器 %s: 等待 start 信号...\n", containerID)
	select {
	case <-startReady:
		log.Printf("[shim] 容器 %s: 收到 start 信号\n", containerID)
	case <-shutdownDone:
		log.Printf("[shim] 容器 %s: 收到 shutdown 信号，退出\n", containerID)
		return nil
	case <-time.After(5 * time.Minute):
		log.Printf("[shim] 容器 %s: 等待 start 信号超时\n", containerID)
		return nil
	}

	if isTTY {
		log.Printf("[shim] 容器 %s: 等待 attach 连接...\n", containerID)
		select {
		case <-attachReady:
			log.Printf("[shim] 容器 %s: attach 已连接, 启动容器\n", containerID)
		case <-time.After(30 * time.Second):
			log.Printf("[shim] 容器 %s: attach 超时, 直接启动容器\n", containerID)
		}
	}

	log.Printf("[shim] 容器 %s: 调用 runtime start\n", containerID)
	startCmd := exec.Command("/proc/self/exe", "runtime", "start", containerID)
	//将新启动的 runtime start 子进程的 Stdout（标准输出）和 Stderr（标准错误），绑定到当前 shim 进程的 Stdout 和 Stderr 上
	//通过这行重定向，runtime create 在创建容器时输出的所有日志、甚至报错崩溃信息（比如 Namespace 创建失败、pivot_root 失败等），都会被自动、原封不动地写入到 shim 进程的输入输出中。
	startCmd.Stdout = os.Stdout
	startCmd.Stderr = os.Stderr
	if err := startCmd.Run(); err != nil {
		if containerPTY != nil {
			containerPTY.Close()
		}
		return fmt.Errorf("runtime start 失败: %w", err)
	}

	log.Printf("[shim] 容器 %s: 用户进程已启动 (PID=%d)\n", containerID, containerPID)

	go func() {
		exitOnce.Do(func() {
			exitCode := waitForContainerExit(containerPID) // 1. 阻塞等待系统调用 Wait4 返回,等待容器进程结束
			exitInfo.ExitCode = exitCode
			exitInfo.ExitedAt = time.Now().Format(time.RFC3339)

			exitData, _ := json.Marshal(exitInfo)
			exitPath := filepath.Join(shimContainerDir, "exit.json")
			if err := os.WriteFile(exitPath, exitData, 0644); err != nil {
				log.Printf("写入文件 %s 失败: %v\n", exitPath, err)
			}
			close(exitReady) // 2. 容器一旦退出，立刻关闭该通道，向其他协程广播

			log.Printf("[shim] 容器 %s: 已退出 (exit_code=%d)\n", containerID, exitCode)
		})
	}()

	<-shutdownDone           // 1. 阻塞等待关闭信号
	if containerPTY != nil { // 2. 收到信号，关闭伪终端主设备
		containerPTY.Close()
	}
	if logFile != nil {
		logFile.Close() // 3. 关闭日志文件
	}
	return nil // 4. 函数返回，Shim 进程退出
}

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
	if err := unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0); err != nil {
		return fmt.Errorf("设置 PR_SET_CHILD_SUBREAPER 失败: %w", err)
	}

	// 创建 shim 目录和控制 socket
	shimContainerDir := filepath.Join(shimDir, containerID)
	if err := os.MkdirAll(shimContainerDir, 0755); err != nil {
		return fmt.Errorf("创建 shim 目录失败: %w", err)
	}

	socketPath := filepath.Join(shimContainerDir, "shim.sock")
	os.Remove(socketPath)
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		return fmt.Errorf("创建控制 socket 失败: %w", err)
	}
	defer listener.Close()

	// 写入 shim PID 文件
	pidPath := filepath.Join(shimContainerDir, "shim.pid")
	if err := os.WriteFile(pidPath, []byte(fmt.Sprintf("%d", os.Getpid())), 0644); err != nil {
		log.Printf("写入文件 %s 失败: %v\n", pidPath, err)
	}

	pid := containerPID
	var pidMu sync.Mutex
	var exitOnce sync.Once
	var shutdownOnce sync.Once
	exitInfo := &types.ExitInfo{}
	exitReady := make(chan struct{})
	shutdownDone := make(chan struct{})
	startReady := make(chan struct{})
	attachReady := make(chan struct{})
	var startOnce sync.Once
	var attachOnce sync.Once

	ctx := &shimContext{
		containerID:  containerID,
		containerPID: &pid,
		pidMu:        &pidMu,
		exitReady:    exitReady,
		exitInfo:     exitInfo,
		shutdownDone: shutdownDone,
		shutdownOnce: &shutdownOnce,
		startReady:   startReady,
		startOnce:    &startOnce,
		attachReady:  attachReady,
		attachOnce:   &attachOnce,
	}

	go serveControlSocket(listener, ctx)

	log.Printf("[shim] 容器 %s: takeover 模式，接管 PID=%d\n", containerID, containerPID)

	// 等待容器进程退出
	go func() {
		exitOnce.Do(func() {
			exitCode := waitForContainerExit(containerPID)
			exitInfo.ExitCode = exitCode
			exitInfo.ExitedAt = time.Now().Format(time.RFC3339)

			exitData, _ := json.Marshal(exitInfo)
			exitPath := filepath.Join(shimContainerDir, "exit.json")
			if err := os.WriteFile(exitPath, exitData, 0644); err != nil {
				log.Printf("写入文件 %s 失败: %v\n", exitPath, err)
			}
			close(exitReady)
			log.Printf("[shim] 容器 %s: 已退出 (exit_code=%d)\n", containerID, exitCode)
		})
	}()

	<-shutdownDone
	return nil
}

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
	defer conn.Close()

	ctx.pidMu.Lock()
	containerPID := *ctx.containerPID
	ctx.pidMu.Unlock()

	var req types.ShimRequest
	if err := json.NewDecoder(conn).Decode(&req); err != nil {
		json.NewEncoder(conn).Encode(types.ShimResponse{Success: false, Message: err.Error()})
		return
	}

	switch req.Type {
	case "state":
		c, err := libcontainer.Load(ctx.containerID)
		if err != nil {
			json.NewEncoder(conn).Encode(types.ShimResponse{Success: false, Message: err.Error()})
			return
		}
		state := &libcontainer.ContainerState{
			ID:     ctx.containerID,
			Pid:    c.Pid(),
			Status: libcontainer.StatusCreated,
		}
		if containerPID > 0 {
			if utils.CheckProcessAlive(state.Pid) {
				state.Status = libcontainer.StatusRunning
			} else {
				state.Status = libcontainer.StatusStopped
			}
		} else if state.Pid <= 0 {
			state.Status = libcontainer.StatusStopped
		}
		json.NewEncoder(conn).Encode(types.ShimResponse{Success: true, Data: state})

	case "kill":
		if containerPID <= 0 {
			json.NewEncoder(conn).Encode(types.ShimResponse{Success: false, Message: "容器进程尚未启动"})
			return
		}
		sig := syscall.Signal(req.Signal)
		if err := unix.Kill(containerPID, sig); err != nil {
			json.NewEncoder(conn).Encode(types.ShimResponse{Success: false, Message: err.Error()})
			return
		}
		json.NewEncoder(conn).Encode(types.ShimResponse{Success: true})

	case "exit_info":
		select {
		case <-ctx.exitReady: // 容器一旦退出，这里立刻响应，并将退出码返回给 Daemon
			json.NewEncoder(conn).Encode(types.ShimResponse{Success: true, Data: ctx.exitInfo})
		case <-time.After(5 * time.Second):
			json.NewEncoder(conn).Encode(types.ShimResponse{Success: false, Message: "容器尚未退出"})
		}

	case "exec":
		if containerPID <= 0 {
			json.NewEncoder(conn).Encode(types.ShimResponse{Success: false, Message: "容器进程尚未启动"})
			return
		}
		json.NewEncoder(conn).Encode(types.ShimResponse{Success: true, Stream: req.Tty})
		nsenterCmd := exec.Command("nsenter",
			"-t", fmt.Sprintf("%d", containerPID),
			"-m", "-u", "-i", "-n", "-p", "--")
		nsenterCmd.Args = append(nsenterCmd.Args, req.Args...)
		nsenterCmd.Stdin = conn
		nsenterCmd.Stdout = conn
		nsenterCmd.Stderr = conn

		if err := nsenterCmd.Start(); err != nil {
			log.Printf("[shim] exec nsenter 启动失败: %v\n", err)
			return
		}

		execPID := nsenterCmd.Process.Pid
		c, loadErr := libcontainer.Load(ctx.containerID)
		if loadErr == nil {
			cfg := c.Config()
			if cfg.Cgroups != nil && cfg.Cgroups.CgroupName != "" {
				cgroupPath := filepath.Join(constants.CgroupRootPath, cfg.Cgroups.CgroupName)
				cgroupProcsPath := filepath.Join(cgroupPath, "cgroup.procs")
				if _, statErr := os.Stat(cgroupPath); statErr == nil {
					if err := os.WriteFile(cgroupProcsPath, []byte(fmt.Sprintf("%d", execPID)), 0644); err != nil {
						log.Printf("写入文件 %s 失败: %v\n", cgroupProcsPath, err)
					}
				}
			}
		}

		done := make(chan error, 1)
		go func() {
			done <- nsenterCmd.Wait()
		}()

		select {
		case <-done:
		case <-time.After(60 * time.Second):
			nsenterCmd.Process.Kill()
			<-done
		}
		if !req.Tty {
			return
		}

	case "attach":
		if ctx.containerPTY == nil || ctx.containerPTY.Master == nil {
			json.NewEncoder(conn).Encode(types.ShimResponse{Success: false, Message: "容器未启用 TTY"})
			return
		}
		json.NewEncoder(conn).Encode(types.ShimResponse{Success: true, Stream: true})
		//发送attachReady 信号
		ctx.attachOnce.Do(func() { close(ctx.attachReady) })

		var once sync.Once
		//标志I/O 转发是否结束
		done := make(chan struct{})
		//两个 goroutine 实现了 全双工 的终端 I/O：
		//goroutine 1:  conn ──io.Copy──> containerPTY.Master ──(内核PTM)──> Slave ──> 容器stdin
		//goroutine 2:  容器stdout ──> Slave ──(内核PTM)──> containerPTY.Master ──io.Copy──> conn
		//- io.Copy(Master, conn) ：从 daemon 连接读取用户输入，写入 PTY Master，内核将数据转发到 Slave 端，容器进程从 stdin 读到
		//- io.Copy(conn, Master) ：从 PTY Master 读取容器输出（内核从 Slave 端转发过来的），写入 daemon 连接，最终到达用户终端
		//sync.Once + done channel 确保只要 任一方向的 Copy 结束 （比如用户按了 Ctrl+D 关闭输入，或容器退出导致 Master EOF），done channel 只关闭一次，触发整个 attach 会话结束。
		go func() {
			defer once.Do(func() { close(done) })
			//io.Copy(dst=Master, src=conn),从 conn （daemon 发来的 Unix 域接字连接）读取数据，写入 ctx.containerPTY.Master （PTY 的 Master 端）
			_, _ = io.Copy(ctx.containerPTY.Master, conn) // 阻塞等待deamon进程传来输入,退出条件:daemon 关闭 conn 的写端（用户退出终端 / Ctrl+D）
		}()
		go func() {
			defer once.Do(func() { close(done) })
			_, _ = io.Copy(conn, ctx.containerPTY.Master) // 阻塞等待容器输出
			// 退出条件:1.容器进程退出 → Slave 端所有 FD 关闭 → 内核在 Master 端产生 EOF → Master.Read() 返回 EOF
			//		   2.shim 关闭 Master（shutdown 时 containerPTY.Close()）
		}()
		//阻塞住当前handleShimConn goroutine,不然handleShimConn处理完,defer conn.Close() 会关闭连接，I/O 转发就断了
		select {
		case <-done: // I/O 转发中某一方向结束（连接断开或 EOF）
		case <-ctx.exitReady:
			time.Sleep(100 * time.Millisecond)
		case <-ctx.shutdownDone:
		}

	case "resize":
		if ctx.containerPTY == nil || ctx.containerPTY.Master == nil {
			json.NewEncoder(conn).Encode(types.ShimResponse{Success: false, Message: "容器未启用 TTY"})
			return
		}
		if err := ctx.containerPTY.SetWinsize(req.Rows, req.Cols); err != nil {
			json.NewEncoder(conn).Encode(types.ShimResponse{Success: false, Message: err.Error()})
			return
		}
		json.NewEncoder(conn).Encode(types.ShimResponse{Success: true})

	case "start":
		ctx.startOnce.Do(func() { close(ctx.startReady) })
		json.NewEncoder(conn).Encode(types.ShimResponse{Success: true})

	case "pause":
		c, err := libcontainer.Load(ctx.containerID)
		if err != nil {
			json.NewEncoder(conn).Encode(types.ShimResponse{Success: false, Message: fmt.Sprintf("加载容器失败: %v", err)})
			return
		}
		if err := c.Pause(); err != nil {
			json.NewEncoder(conn).Encode(types.ShimResponse{Success: false, Message: fmt.Sprintf("暂停容器失败: %v", err)})
			return
		}
		json.NewEncoder(conn).Encode(types.ShimResponse{Success: true})

	case "unpause":
		c, err := libcontainer.Load(ctx.containerID)
		if err != nil {
			json.NewEncoder(conn).Encode(types.ShimResponse{Success: false, Message: fmt.Sprintf("加载容器失败: %v", err)})
			return
		}
		if err := c.Resume(); err != nil {
			json.NewEncoder(conn).Encode(types.ShimResponse{Success: false, Message: fmt.Sprintf("恢复容器失败: %v", err)})
			return
		}
		json.NewEncoder(conn).Encode(types.ShimResponse{Success: true})

	case "shutdown":
		json.NewEncoder(conn).Encode(types.ShimResponse{Success: true})
		ctx.shutdownOnce.Do(func() { close(ctx.shutdownDone) })

	default:
		json.NewEncoder(conn).Encode(types.ShimResponse{Success: false, Message: fmt.Sprintf("未知请求: %s", req.Type)})
	}
}
