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
		fmt.Fprintf(os.Stderr, "用法: mini-docker shim <containerID> <bundlePath>\n")
		os.Exit(1)
	}

	containerID := args[0]
	bundlePath := args[1]

	if err := run(containerID, bundlePath); err != nil {
		fmt.Fprintf(os.Stderr, "shim 错误: %v\n", err)
		os.Exit(1)
	}
}

// shim 进程与容器进程是“寄生”关系：它直接管理该容器的生命周期，持久化托管其 I/O 流和终端，并在容器进程退出时收集其状态码。
func run(containerID, bundlePath string) error {
	//  进程孤儿与僵尸回收（Subreaper 机制）
	//在 Linux 中，当父进程退出后，子进程会变成“孤儿进程”，默认会被系统 PID 1（systemd 等）收养。
	//当 mini-docker runtime 命令执行完 create 和 start 退出后，容器进程就成了孤儿。为了让 shim 能够监控并回收容器，代码第一步就执行了：
	//	unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0)
	//原理：这会告诉 Linux 内核，将此 shim 标记为 Subreaper（子孙进程收割者）。之后，所有由 runtime 创建、因 runtime 退出而成为孤儿的容器进程，都会被重定向 reparent（收养）到这个 shim 下。
	//这样，shim 就可以合法地对容器 PID 调用 wait 相关的系统调用，防止其变成“僵尸进程”，并精准捕捉其退出状态。shim进程不收养孤儿进程则shim无法wait容器进程。
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
	os.WriteFile(pidPath, []byte(fmt.Sprintf("%d", os.Getpid())), 0644)
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
		os.WriteFile(ptyInfoPath, []byte(containerPTY.Name), 0644)
		log.Printf("[shim] 容器 %s: TTY 模式, pty slave=%s\n", containerID[:12], containerPTY.Name)
	}

	logPath := filepath.Join(shimContainerDir, "container.log")
	var logFile *os.File
	if !isTTY { //在磁盘上创建并打开 container.log 文件，准备用于记录容器的 stdout 和 stderr 日志
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

	var stdoutPipeR, stderrPipeR *os.File
	//处理deamon进程发给shim进程的请求
	go serveControlSocket(listener, containerID, &containerPID, &pidMu, exitReady, exitInfo, shutdownDone, &shutdownOnce, containerPTY, attachReady, logFile)
	//shim 构建参数的逻辑:
	//基础参数（始终存在）：
	//  "runtime", "create", <containerID>, "--bundle", <bundlePath>
	//
	//TTY 模式追加：
	//  "--console", <ptyName>
	//
	//非 TTY 模式追加：
	//  "--stdout-fd", "3", "--stderr-fd", "4"
	//
	//### 实际调用示例
	//TTY 模式 ( -it )：
	// /proc/self/exe runtime create abc123 --bundle /var/lib/mini-docker/runtime/abc123/bundle --console /dev/pts/0
	//
	//非 TTY 模式 ( -d )：
	//
	///proc/self/exe runtime create abc123 --bundle /var/lib/mini-docker/runtime/abc123/bundle --stdout-fd 3 --stderr-fd 4

	log.Printf("[shim] 容器 %s: 调用 runtime create\n", containerID[:12])
	createArgs := []string{"runtime", "create", containerID, "--bundle", bundlePath}
	if isTTY && containerPTY != nil {
		createArgs = append(createArgs, "--console", containerPTY.Name)
		//在 Linux 的伪终端（PTY）管理和容器运行时设计中，这里关闭 Slave（从设备）句柄，主要原因：
		//确保 EOF（进程退出信号）的正确传递（最核心的原因）
		//这是 Linux 伪终端机制的一条硬性规则：
		//规则：只有当所有引用了 PTY Slave（从设备）端的文件描述符（FD）都被关闭后，内核才会在 PTY Master（主设备）端产生一个 EOF（在 Go 中读取会表现为 io.EOF 或特定的 EIO 错误）。
		//后果：如果 Shim 进程不关闭自己手中的 containerPTY.Slave 句柄，那么即便容器内部的 Shell 进程（如 /bin/sh）已经退出、且容器内所有对 Slave 的引用都已释放，由于 Shim 进程还拿着这个 Slave 句柄，内核就不会在 Master 端触发 EOF。
		//影响：这会导致 io.Copy(conn, containerPTY.Master) 拷贝协程永远阻塞在等待读取上，客户端也就无法感知到容器已经退出，最终导致控制台挂起、连接泄露和资源无法回收。
		//因此，Shim 必须在完成“桥梁”搭建后，第一时间斩断自己与 Slave 的直接文件描述符引用。
		containerPTY.Slave.Close() // Runtime 会自己重新打开 Slave 设备
		containerPTY.Slave = nil
	}
	if !isTTY {
		//如果是非 TTY 模式：
		//为了捕获容器进程未来的输出，创建了 os.Pipe 管道（分为写端 PipeW 和读端 PipeR），并通过 --stdout-fd 和 --stderr-fd 强行传递给子进程（文件描述符 3 和 4）
		//代码中的 os.Pipe() 创建的是操作系统的无名管道（Anonymous Pipe）。
		//管道是单向的数据通道，由内核在内存中维护。
		//它有且仅有两个端点：
		//W（Write，写入端）：往里面写数据。
		//R（Read，读取端）：从里面读数据。
		//数据从 W 端流进去，就会立刻从 R 端流出来

		//容器内进程 ──(输出)──> stdout (FD 1)
		//                       │ (Runtime 接线重定向)
		//                       ▼
		//                 stdoutPipeW (FD 3)  <── [Shim 主动关闭此处的引用]
		//                       │
		//             (操作系统管道，数据穿过 Namespace 屏障)
		//                       │
		//                       ▼
		//                 stdoutPipeR (读端)
		//                       │
		//                writeLogStream() ──(序列化为JSON)──> container.log 文件
		var stdoutPipeW, stderrPipeW *os.File
		stdoutPipeR, stdoutPipeW, err = os.Pipe() //shim 创建了管道。此时，W（写端）和 R（读端）都在 shim 进程手里
		if err != nil {
			logFile.Close()
			return fmt.Errorf("创建 stdout pipe 失败: %w", err)
		}
		stderrPipeR, stderrPipeW, err = os.Pipe()
		if err != nil {
			stdoutPipeR.Close()
			stdoutPipeW.Close()
			logFile.Close()
			return fmt.Errorf("创建 stderr pipe 失败: %w", err)
		}
		createArgs = append(createArgs,
			"--stdout-fd", fmt.Sprintf("%d", 3),
			"--stderr-fd", fmt.Sprintf("%d", 4))
		createCmd := exec.Command("/proc/self/exe", createArgs...)
		//文件描述符 (File Descriptor) 是什么？
		//文件描述符是操作系统内核用来 标识打开的文件/IO 资源 的整数句柄。

		//进程视角：
		//  FD 0 → stdin  (标准输入)
		//  FD 1 → stdout (标准输出)
		//  FD 2 → stderr (标准错误)
		//  FD 3 → 某个打开的文件
		//  FD 4 → 某个 socket
		//  FD 5 → 某个管道
		//  ...
		//每个进程都有一个 文件描述符表 ，内核维护：
		//
		//进程 A 的 FD 表
		//┌─────┬──────────────────┐
		//│ FD  │ 指向的内核对象     │
		//├─────┼──────────────────┤
		//│  0  │ /dev/pts/0 (终端) │
		//│  1  │ /dev/pts/0 (终端) │
		//│  2  │ /dev/pts/0 (终端) │
		//│  3  │ pipe:[12345]      │
		//│  4  │ socket:[67890]    │
		//└─────┴──────────────────┘

		//下面两句的含义：
		//createCmd 是一个 exec.Cmd 对象，代表即将执行的子进程（/proc/self/exe runtime create ...）。
		//createCmd.Stdout 指定子进程的 标准输出（fd 1）要写入到哪里。
		//createCmd.Stderr 指定子进程的 标准错误（fd 2）要写入到哪里。
		//os.Stdout 和 os.Stderr 是当前（shim）进程的标准输出和标准错误文件描述符。
		//因此，这行代码 将子进程的标准输出和标准错误重定向到当前进程的标准输出和标准错误。
		//达到的效果
		//子进程（runtime create）在创建容器过程中打印的所有日志、错误信息、状态输出，都会直接出现在 shim 进程的终端（或调用 shim 的上级进程所看到的输出流）上。
		//这样可以方便地观察容器创建阶段的运行情况，无需额外收集子进程的输出文件。
		//在调试或运行时，这些输出会和 shim 自身的日志混在一起打印，便于实时监控。
		//举个例子
		//如果 runtime create 内部执行 fmt.Println("create success")，这句话会被写到 createCmd.Stdout → 也就是 shim 的 os.Stdout
		createCmd.Stdout = os.Stdout
		createCmd.Stderr = os.Stderr
		createCmd.ExtraFiles = []*os.File{stdoutPipeW, stderrPipeW} //将“管道的写端”强行塞给子进程，这里传递给子进程的是文件描述符
		//文件描述符传递机制:
		//非 TTY 模式 下，shim 创建管道并通过 ExtraFiles 传递：
		//shim 进程                        runtime create 子进程
		//    │                                  │
		//    ├─ stdoutPipeW (写端) ──────► ExtraFiles[0] → FD 3
		//    ├─ stderrPipeW (写端) ──────► ExtraFiles[1] → FD 4
		//    ├─ stdoutPipeR (读端) ← 自己保留，读取容器输出
		//    └─ stderrPipeR (读端) ← 自己保留，读取容器输出

		//怎么确定stdoutPipeW, stderrPipeW这两个文件的文件描述符编号：
		//Go 的 exec.Cmd.ExtraFiles 的固定规则:
		//FD 指向
		//0 stdin（标准输入）
		//1 stdout（标准输出）
		//2 stderr（标准错误）
		//3 ExtraFiles[0]
		//4 ExtraFiles[1]
		//5 ExtraFiles[2]
		//... ...
		if err := createCmd.Run(); err != nil {
			stdoutPipeR.Close()
			stdoutPipeW.Close()
			stderrPipeR.Close()
			stderrPipeW.Close()
			logFile.Close()
			if containerPTY != nil {
				containerPTY.Close()
			}
			return fmt.Errorf("runtime create 失败: %w", err)
		}
		stdoutPipeW.Close()
		stderrPipeW.Close()
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
	log.Printf("[shim] 容器 %s: PID=%d, 状态=created\n", containerID[:12], containerPID)

	if !isTTY && logFile != nil && stdoutPipeR != nil && stderrPipeR != nil {
		//开启后台协程 writeLogStream，它们会从刚才创建的 PipeR 中源源不断地读取容器的输出，并转化为带时间戳的 JSON 写入 container.log，也就是从容器进程中读取其标准输出和标准错误
		go writeLogStream(stdoutPipeR, logFile, "stdout")
		go writeLogStream(stderrPipeR, logFile, "stderr")
	}

	if isTTY {
		log.Printf("[shim] 容器 %s: 等待 attach 连接...\n", containerID[:12])
		select {
		case <-attachReady:
			log.Printf("[shim] 容器 %s: attach 已连接, 启动容器\n", containerID[:12])
		case <-time.After(30 * time.Second):
			log.Printf("[shim] 容器 %s: attach 超时, 直接启动容器\n", containerID[:12])
		}
	}

	log.Printf("[shim] 容器 %s: 调用 runtime start\n", containerID[:12])
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

	log.Printf("[shim] 容器 %s: 用户进程已启动 (PID=%d)\n", containerID[:12], containerPID)

	go func() {
		exitOnce.Do(func() {
			exitCode := waitForContainerExit(containerPID) // 1. 阻塞等待系统调用 Wait4 返回,等待容器进程结束
			exitInfo.ExitCode = exitCode
			exitInfo.ExitedAt = time.Now().Format(time.RFC3339)

			exitData, _ := json.Marshal(exitInfo)
			exitPath := filepath.Join(shimContainerDir, "exit.json")
			os.WriteFile(exitPath, exitData, 0644)
			close(exitReady) // 2. 容器一旦退出，立刻关闭该通道，向其他协程广播

			log.Printf("[shim] 容器 %s: 已退出 (exit_code=%d)\n", containerID[:12], exitCode)
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

func writeLogStream(pipeR *os.File, logFile *os.File, stream string) {
	defer pipeR.Close()
	reader := io.Reader(pipeR)
	buf := make([]byte, constants.DefaultBufferSize)
	for {
		n, err := reader.Read(buf)
		if n > 0 {
			entry := types.LogEntry{
				Log:    string(buf[:n]),
				Stream: stream,
				Time:   time.Now().Format(time.RFC3339Nano),
			}
			data, _ := json.Marshal(entry)
			data = append(data, '\n')
			logFile.Write(data)
		}
		if err != nil {
			return
		}
	}
}

func waitForContainerExit(pid int) int {
	for {
		var status syscall.WaitStatus
		wpid, err := syscall.Wait4(pid, &status, 0, nil)
		if err != nil {
			if err == syscall.ECHILD {
				if utils.CheckProcessAlive(pid) != nil {
					return 0
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

func serveControlSocket(listener net.Listener, containerID string, containerPID *int, pidMu *sync.Mutex, exitReady <-chan struct{}, exitInfo *types.ExitInfo, shutdownDone chan struct{}, shutdownOnce *sync.Once, containerPTY *pty.PTY, attachReady chan struct{}, logFile *os.File) {
	for {
		conn, err := listener.Accept()
		if err != nil {
			select {
			case <-shutdownDone:
				return
			default:
			}
			continue
		}
		pidMu.Lock()
		pid := *containerPID
		pidMu.Unlock()
		go handleShimConn(conn, containerID, pid, exitReady, exitInfo, shutdownDone, shutdownOnce, containerPTY, attachReady, logFile)
	}
}

func handleShimConn(conn net.Conn, containerID string, containerPID int, exitReady <-chan struct{}, exitInfo *types.ExitInfo, shutdownDone chan struct{}, shutdownOnce *sync.Once, containerPTY *pty.PTY, attachReady chan struct{}, logFile *os.File) {
	defer conn.Close()

	var req types.ShimRequest
	if err := json.NewDecoder(conn).Decode(&req); err != nil {
		json.NewEncoder(conn).Encode(types.ShimResponse{Success: false, Message: err.Error()})
		return
	}

	switch req.Type {
	case "state":
		c, err := libcontainer.Load(containerID)
		if err != nil {
			json.NewEncoder(conn).Encode(types.ShimResponse{Success: false, Message: err.Error()})
			return
		}
		state := &spec.State{
			ID:     containerID,
			Status: spec.StatusCreated,
			Pid:    c.Pid(),
		}
		if containerPID > 0 {
			if utils.CheckProcessAlive(state.Pid) == nil {
				state.Status = spec.StatusRunning
			} else {
				state.Status = spec.StatusStopped
			}
		} else if state.Pid <= 0 {
			state.Status = spec.StatusStopped
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
		case <-exitReady: // 容器一旦退出，这里立刻响应，并将退出码返回给 Daemon
			json.NewEncoder(conn).Encode(types.ShimResponse{Success: true, Data: exitInfo})
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
		done := make(chan error, 1)
		go func() {
			done <- nsenterCmd.Run()
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
		if containerPTY == nil || containerPTY.Master == nil {
			json.NewEncoder(conn).Encode(types.ShimResponse{Success: false, Message: "容器未启用 TTY"})
			return
		}
		json.NewEncoder(conn).Encode(types.ShimResponse{Success: true, Stream: true})

		select {
		case <-attachReady:
		default:
			close(attachReady)
		}

		var once sync.Once
		done := make(chan struct{})

		go func() {
			defer once.Do(func() { close(done) })
			_, _ = io.Copy(containerPTY.Master, conn)
		}()
		go func() {
			defer once.Do(func() { close(done) })
			_, _ = io.Copy(conn, containerPTY.Master)
		}()

		select {
		case <-done:
		case <-exitReady:
			time.Sleep(100 * time.Millisecond)
		case <-shutdownDone:
		}

	case "resize":
		if containerPTY == nil || containerPTY.Master == nil {
			json.NewEncoder(conn).Encode(types.ShimResponse{Success: false, Message: "容器未启用 TTY"})
			return
		}
		if err := containerPTY.SetWinsize(req.Rows, req.Cols); err != nil {
			json.NewEncoder(conn).Encode(types.ShimResponse{Success: false, Message: err.Error()})
			return
		}
		json.NewEncoder(conn).Encode(types.ShimResponse{Success: true})

	case "shutdown":
		json.NewEncoder(conn).Encode(types.ShimResponse{Success: true})
		shutdownOnce.Do(func() { close(shutdownDone) })

	default:
		json.NewEncoder(conn).Encode(types.ShimResponse{Success: false, Message: fmt.Sprintf("未知请求: %s", req.Type)})
	}
}
