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
// 这个包是runc的接口层
package runtime

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"

	"mini-docker/constants"
	"mini-docker/libcontainer"
	"mini-docker/libcontainer/configs"
	"mini-docker/spec"
)

var containerIDPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]+$`)

func validateContainerID(id string) {
	if !containerIDPattern.MatchString(id) {
		fmt.Fprintf(os.Stderr, "错误: 容器 ID 格式无效，必须匹配 [a-zA-Z0-9][a-zA-Z0-9_.-]+\n")
		os.Exit(1)
	}
}

// Create 对标 runc create：创建容器环境（namespace + rootfs），但不启动用户进程
func Create(args []string) {
	if len(args) < 2 {
		fmt.Fprintf(os.Stderr, "用法: mini-docker runtime create <id> --bundle <path> [--console <path>]\n")
		os.Exit(1)
	}

	containerID := args[0]
	validateContainerID(containerID)
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
	validateContainerID(containerID)

	container, err := libcontainer.Load(containerID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "错误: 加载容器失败: %v\n", err)
		os.Exit(1)
	}

	status, _ := container.Status()
	if status != libcontainer.StatusCreated {
		fmt.Fprintf(os.Stderr, "错误: 容器状态必须是 created，当前: %s\n", status)
		os.Exit(1)
	}

	if err := container.ExecStart(); err != nil {
		fmt.Fprintf(os.Stderr, "错误: 启动容器失败: %v\n", err)
		os.Exit(1)
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
	validateContainerID(containerID)
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

	container.Status()

	if err := container.Signal(int(sig)); err != nil {
		fmt.Fprintf(os.Stderr, "错误: 发送信号失败: %v\n", err)
		os.Exit(1)
	}

	if sig == syscall.SIGKILL || sig == syscall.SIGTERM {
		libcontainer.WaitForProcessExit(container.Pid(), 5000)
	}

	os.Exit(0)
}

// Delete 对标 runc delete：删除容器，清理资源
// OCI 规范要求：对 running/paused 容器必须带 --force 才能删除（先 kill 再清理）
func Delete(args []string) {
	if len(args) < 1 {
		fmt.Fprintf(os.Stderr, "用法: mini-docker runtime delete <id> [--force]\n")
		os.Exit(1)
	}

	containerID := args[0]
	validateContainerID(containerID)
	force := false
	for i := 1; i < len(args); i++ {
		if args[i] == "--force" {
			force = true
		}
	}

	container, err := libcontainer.Load(containerID)
	if err != nil {
		stateDir := filepath.Join(constants.RuntimeDir, containerID)
		if _, statErr := os.Stat(stateDir); statErr == nil {
			os.RemoveAll(stateDir)
		}
		os.Exit(0)
	}

	status, _ := container.Status()
	if !force && (status == libcontainer.StatusRunning || status == libcontainer.StatusPaused) {
		fmt.Fprintf(os.Stderr, "错误: 容器正在运行，请先停止或使用 --force\n")
		os.Exit(1)
	}

	if err := container.Destroy(); err != nil {
		fmt.Fprintf(os.Stderr, "错误: 删除容器失败: %v\n", err)
		os.Exit(1)
	}

	os.Exit(0)
}

// State 对标 runc state：查询容器当前状态
// 对标 OCI runtime-spec Query State：MUST return the state of a container as specified in the State section
func State(args []string) {
	if len(args) < 1 {
		fmt.Fprintf(os.Stderr, "用法: mini-docker runtime state <id>\n")
		os.Exit(1)
	}

	containerID := args[0]
	validateContainerID(containerID)

	container, err := libcontainer.Load(containerID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "错误: 加载容器失败: %v\n", err)
		os.Exit(1)
	}

	container.Status()

	state := container.State()

	data, _ := json.MarshalIndent(state, "", "  ")
	fmt.Println(string(data))
	os.Exit(0)
}

// Exec 对标 runc exec：在已运行的容器内执行新进程
// 参数: <containerID> <command> [args...]
func Exec(args []string) {
	if len(args) < 2 {
		fmt.Fprintf(os.Stderr, "用法: mini-docker runtime exec <id> <command> [args...]\n")
		os.Exit(1)
	}

	containerID := args[0]
	validateContainerID(containerID)

	container, err := libcontainer.Load(containerID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "错误: 加载容器失败: %v\n", err)
		os.Exit(1)
	}

	process := &libcontainer.Process{
		Args: args[1:],
	}

	if err := container.Exec(process); err != nil {
		fmt.Fprintf(os.Stderr, "错误: 在容器内执行命令失败: %v\n", err)
		os.Exit(1)
	}

	os.Exit(0)
}

// Pause 对标 runc pause：暂停容器
func Pause(args []string) {
	if len(args) < 1 {
		fmt.Fprintf(os.Stderr, "用法: mini-docker runtime pause <id>\n")
		os.Exit(1)
	}

	containerID := args[0]
	validateContainerID(containerID)

	container, err := libcontainer.Load(containerID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "错误: 加载容器失败: %v\n", err)
		os.Exit(1)
	}

	if err := container.Pause(); err != nil {
		fmt.Fprintf(os.Stderr, "错误: 暂停容器失败: %v\n", err)
		os.Exit(1)
	}

	os.Exit(0)
}

// Resume 对标 runc resume：恢复暂停的容器
func Resume(args []string) {
	if len(args) < 1 {
		fmt.Fprintf(os.Stderr, "用法: mini-docker runtime resume <id>\n")
		os.Exit(1)
	}

	containerID := args[0]
	validateContainerID(containerID)

	container, err := libcontainer.Load(containerID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "错误: 加载容器失败: %v\n", err)
		os.Exit(1)
	}

	if err := container.Resume(); err != nil {
		fmt.Fprintf(os.Stderr, "错误: 恢复容器失败: %v\n", err)
		os.Exit(1)
	}

	os.Exit(0)
}

// convertToConfig 将 OCI Spec 转换为 libcontainer 配置
func convertToConfig(s *spec.Spec, bundlePath string) *configs.Config {
	return spec.SpecToConfig(s, bundlePath)
}

// kill 在 docker 中不仅仅是"杀死"进程，它的本质是 向进程发送信号 。不同信号有不同效果：
// 信号	    编号	效果
// SIGTERM	15	优雅终止（进程可以捕获并做清理）
// SIGKILL	9	强制杀死（进程无法捕获，立即终止）
// SIGHUP	1	挂起（常用于重载配置）
// SIGINT	2	中断（相当于 Ctrl+C）
// SIGSTOP	19	暂停进程
// SIGCONT	18	恢复暂停的进程
func parseSignal(s string) (syscall.Signal, error) {
	if sig, err := strconv.Atoi(s); err == nil {
		if sig < 1 || sig > 64 {
			return 0, fmt.Errorf("信号编号 %d 超出有效范围 (1-64)", sig)
		}
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
