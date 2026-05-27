//go:build linux

// Package runtime 提供 OCI 运行时命令实现，对标 runc
//
// 这个包实现了 OCI runtime-spec 定义的命令行接口：
// - create: 创建容器环境（namespace + rootfs + cgroup）
// - start:  启动容器中的用户进程
// - kill:   向容器发送信号
// - delete: 删除容器，清理资源
// - state:  查询容器状态
//
// 使用 libcontainer 库实现核心功能。
package runtime

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"mini-docker/constants"
	"mini-docker/libcontainer"
	"mini-docker/libcontainer/configs"
	"mini-docker/spec"

	"golang.org/x/sys/unix"
)

// Create 对标 runc create：创建容器环境（namespace + rootfs），但不启动用户进程
func Create(args []string) {
	if len(args) < 2 {
		fmt.Fprintf(os.Stderr, "用法: mini-docker runtime create <id> --bundle <path> [--console <path>]\n")
		os.Exit(1)
	}

	containerID := args[0]
	bundlePath := ""
	consolePath := "" //PTY 设备路径,TTY模式下shim创建的从设备
	stdoutFd := -1    //
	stderrFd := -1
	for i := 1; i < len(args); i++ {
		if args[i] == "--bundle" {
			if i+1 >= len(args) {
				fmt.Fprintf(os.Stderr, "错误: --bundle 需要指定路径\n")
				os.Exit(1)
			}
			bundlePath = args[i+1]
			i++
		} else if args[i] == "--console" {
			if i+1 >= len(args) {
				fmt.Fprintf(os.Stderr, "错误: --console 需要指定路径\n")
				os.Exit(1)
			}
			consolePath = args[i+1]
			i++
		} else if args[i] == "--stdout-fd" {
			if i+1 >= len(args) {
				fmt.Fprintf(os.Stderr, "错误: --stdout-fd 需要指定值\n")
				os.Exit(1)
			}
			stdoutFd, _ = strconv.Atoi(args[i+1])
			i++
		} else if args[i] == "--stderr-fd" {
			if i+1 >= len(args) {
				fmt.Fprintf(os.Stderr, "错误: --stderr-fd 需要指定值\n")
				os.Exit(1)
			}
			stderrFd, _ = strconv.Atoi(args[i+1])
			i++
		}
	}
	if bundlePath == "" {
		fmt.Fprintf(os.Stderr, "错误: 必须指定 --bundle <path>\n")
		os.Exit(1)
	}

	ociSpec, err := spec.LoadSpec(bundlePath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "错误: 加载 OCI Spec 失败: %v\n", err)
		os.Exit(1)
	}

	config := convertToConfig(ociSpec, bundlePath)
	config.BundlePath = bundlePath

	container, err := libcontainer.New(containerID, config)
	if err != nil {
		fmt.Fprintf(os.Stderr, "错误: 创建容器失败: %v\n", err)
		os.Exit(1)
	}

	process := &libcontainer.Process{
		Args:     []string{"/bin/sh"},
		Terminal: ociSpec.Process != nil && ociSpec.Process.Terminal,
	}

	if ociSpec.Process != nil && len(ociSpec.Process.Args) > 0 {
		process.Args = ociSpec.Process.Args
	}

	// 设置容器进程的 I/O（stdin/stdout/stderr）
	// TTY 模式和非 TTY 模式的 I/O 处理完全不同：
	//   - TTY 模式（前台交互）：通过 PTY 双向通信，stdin/stdout/stderr 都连接到同一个 PTY slave
	//   - 非 TTY 模式（后台执行）：通过管道单向捕获输出，stdin 为 nil（无需用户输入）
	if consolePath != "" {
		// tty模式：stdin/stdout/stderr 都绑定到 PTY slave
		consoleFile, err := os.OpenFile(consolePath, os.O_RDWR, 0)
		if err != nil {
			fmt.Fprintf(os.Stderr, "错误: 打开 console %s 失败: %v\n", consolePath, err)
			os.Exit(1)
		}
		process.Stdin = consoleFile
		process.Stdout = consoleFile
		process.Stderr = consoleFile
		process.ConsoleFile = consoleFile
	} else {
		// 非tty模式：将外部传进来的"整型文件描述符（FD）"（例如整数 3 和 4），
		// 包装成 Go 语言标准的文件对象（*os.File），以便直接作为容器内进程的标准输出（Stdout）和标准错误（Stderr）
		// 非tty模式，shim进程没有传递给runtime create进程Stdin，
		// 因为后台运行的容器不需要用户交互，只需要捕获输出（日志），所以只需要另外两个Std
		// 当 stdoutFd 和 stderrFd 相同时（非 TTY 模式直接写日志文件），
		// 只创建一个 os.File 对象共享使用，避免同一 fd 被双重关闭
		if stdoutFd >= 0 && stderrFd >= 0 && stdoutFd == stderrFd {
			file := os.NewFile(uintptr(stdoutFd), "stdout")
			process.Stdout = file
			process.Stderr = file
		} else {
			if stdoutFd >= 0 {
				process.Stdout = os.NewFile(uintptr(stdoutFd), "stdout")
			}
			if stderrFd >= 0 {
				process.Stderr = os.NewFile(uintptr(stderrFd), "stderr")
			}
		}
	}

	//创建容器
	if err := container.Start(process); err != nil {
		fmt.Fprintf(os.Stderr, "错误: 启动容器失败: %v\n", err)
		os.Exit(1)
	}

	if process.ConsoleFile != nil {
		process.ConsoleFile.Close()
	}
	//runtime create 进程创建完容器进程以后，自身进程就结束了
	fmt.Println(container.Pid())
	os.Exit(0)
}

// Start 对标 runc start：向 init 进程发送 start 信号
func Start(args []string) {
	if len(args) < 1 {
		fmt.Fprintf(os.Stderr, "用法: mini-docker runtime start <id>\n")
		os.Exit(1)
	}

	containerID := args[0]

	// 加载容器
	container, err := libcontainer.Load(containerID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "错误: 加载容器失败: %v\n", err)
		os.Exit(1)
	}

	// 检查状态
	status, _ := container.Status()
	if status != libcontainer.StatusCreated {
		fmt.Fprintf(os.Stderr, "错误: 容器状态必须是 created，当前: %s\n", status)
		os.Exit(1)
	}

	// FIFO 创建在 bundle 目录（与 container.Start 中的路径一致）
	config := container.Config()
	fifoPath := filepath.Join(config.BundlePath, ".start-fifo")

	// 发送 start 信号（通过 FIFO）
	f, err := os.OpenFile(fifoPath, os.O_WRONLY, 0)
	if err != nil {
		fmt.Fprintf(os.Stderr, "错误: 打开 FIFO 失败: %v\n", err)
		os.Exit(1)
	}
	if _, err := f.Write([]byte("start\n")); err != nil {
		f.Close()
		fmt.Fprintf(os.Stderr, "错误: 写入 FIFO 失败: %v\n", err)
		os.Exit(1)
	}
	f.Close()
	os.Remove(fifoPath)

	if err := container.SetStatus(libcontainer.StatusRunning); err != nil {
		fmt.Fprintf(os.Stderr, "警告: 更新容器状态失败: %v\n", err)
	}

	os.Exit(0)
}

// Kill 对标 runc kill：向容器主进程发送信号
func Kill(args []string) {
	if len(args) < 2 {
		fmt.Fprintf(os.Stderr, "用法: mini-docker runtime kill <id> <signal>\n")
		os.Exit(1)
	}

	containerID := args[0]
	signalStr := args[1]

	sig, err := parseSignal(signalStr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "错误: 无效信号 %s: %v\n", signalStr, err)
		os.Exit(1)
	}

	container, err := libcontainer.Load(containerID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "错误: 加载容器失败: %v\n", err)
		os.Exit(1)
	}

	if err := container.Signal(int(sig)); err != nil {
		fmt.Fprintf(os.Stderr, "错误: 发送信号失败: %v\n", err)
		os.Exit(1)
	}

	if sig == syscall.SIGKILL || sig == syscall.SIGTERM {
		waitForExit(container.Pid(), 5000)
	}

	os.Exit(0)
}

// Delete 对标 runc delete：删除容器，清理资源
func Delete(args []string) {
	if len(args) < 1 {
		fmt.Fprintf(os.Stderr, "用法: mini-docker runtime delete <id>\n")
		os.Exit(1)
	}

	containerID := args[0]

	container, err := libcontainer.Load(containerID)
	if err != nil {
		stateDir := filepath.Join(constants.RuntimeDir, containerID)
		if _, statErr := os.Stat(stateDir); statErr == nil {
			os.RemoveAll(stateDir)
		}
		os.Exit(0)
	}

	if err := container.Destroy(); err != nil {
		fmt.Fprintf(os.Stderr, "错误: 删除容器失败: %v\n", err)
		os.Exit(1)
	}

	os.Exit(0)
}

// State 对标 runc state：查询容器当前状态
func State(args []string) {
	if len(args) < 1 {
		fmt.Fprintf(os.Stderr, "用法: mini-docker runtime state <id>\n")
		os.Exit(1)
	}

	containerID := args[0]

	container, err := libcontainer.Load(containerID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "错误: 加载容器失败: %v\n", err)
		os.Exit(1)
	}

	status, _ := container.Status()
	state := struct {
		ID     string `json:"id"`
		Status string `json:"status"`
		Pid    int    `json:"pid"`
	}{
		ID:     container.ID(),
		Status: string(status),
		Pid:    container.Pid(),
	}

	data, _ := json.MarshalIndent(state, "", "  ")
	fmt.Println(string(data))
	os.Exit(0)
}

// convertToConfig 将 OCI Spec 转换为 libcontainer 配置
func convertToConfig(s *spec.Spec, bundlePath string) *configs.Config {
	config := &configs.Config{
		Hostname: s.Hostname,
	}

	if s.Root != nil {
		rootfs := s.Root.Path
		if !filepath.IsAbs(rootfs) {
			rootfs = filepath.Join(bundlePath, rootfs)
		}
		config.Rootfs = filepath.Clean(rootfs)
		config.ReadonlyRootfs = s.Root.Readonly
	}

	if s.Process != nil {
		config.Args = s.Process.Args
		config.Env = s.Process.Env
		config.Cwd = s.Process.Cwd
	}

	if s.Linux != nil {
		for _, ns := range s.Linux.Namespaces {
			config.Namespaces = append(config.Namespaces, configs.Namespace{
				Type: ns.Type,
				Path: ns.Path,
			})
		}

		if s.Linux.Resources != nil {
			config.Cgroups = &configs.Resources{}
			if s.Linux.Resources.Memory != nil {
				config.Cgroups.Memory = &configs.Memory{
					Limit: s.Linux.Resources.Memory.Limit,
				}
			}
			if s.Linux.Resources.CPU != nil {
				config.Cgroups.CPU = &configs.CPU{
					Shares: s.Linux.Resources.CPU.Shares,
					Cpus:   s.Linux.Resources.CPU.Cpus,
				}
			}
			if s.Linux.Resources.Pids != nil {
				config.Cgroups.Pids = &configs.Pids{
					Limit: s.Linux.Resources.Pids.Limit,
				}
			}
		}
	}

	for _, m := range s.Mounts {
		config.Mounts = append(config.Mounts, &configs.Mount{
			Destination: m.Destination,
			Type:        m.Type,
			Source:      m.Source,
			Options:     m.Options,
		})
	}

	return config
}

func waitForExit(pid int, timeoutMs int) {
	deadline := timeoutMs / 10
	for i := 0; i < deadline; i++ {
		if err := unix.Kill(pid, 0); err != nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func parseSignal(s string) (syscall.Signal, error) {
	if sig, err := strconv.Atoi(s); err == nil {
		return syscall.Signal(sig), nil
	}
	s = strings.ToUpper(s)
	s = strings.TrimPrefix(s, "SIG")
	sigMap := map[string]syscall.Signal{
		"HUP":  syscall.SIGHUP,
		"INT":  syscall.SIGINT,
		"QUIT": syscall.SIGQUIT,
		"ILL":  syscall.SIGILL,
		"TRAP": syscall.SIGTRAP,
		"ABRT": syscall.SIGABRT,
		"FPE":  syscall.SIGFPE,
		"KILL": syscall.SIGKILL,
		"SEGV": syscall.SIGSEGV,
		"PIPE": syscall.SIGPIPE,
		"ALRM": syscall.SIGALRM,
		"TERM": syscall.SIGTERM,
		"USR1": syscall.SIGUSR1,
		"USR2": syscall.SIGUSR2,
		"CHLD": syscall.SIGCHLD,
		"CONT": syscall.SIGCONT,
		"STOP": syscall.SIGSTOP,
		"TSTP": syscall.SIGTSTP,
		"TTIN": syscall.SIGTTIN,
		"TTOU": syscall.SIGTTOU,
	}
	if sig, ok := sigMap[s]; ok {
		return sig, nil
	}
	return 0, fmt.Errorf("未知信号: %s", s)
}
