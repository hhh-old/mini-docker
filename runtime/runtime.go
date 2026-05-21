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

	"mini-docker/libcontainer"
	"mini-docker/libcontainer/configs"

	"golang.org/x/sys/unix"
)

// Create 对标 runc create：创建容器环境（namespace + rootfs），但不启动用户进程
func Create(args []string) {
	if len(args) < 2 {
		fmt.Fprintf(os.Stderr, "用法: mini-docker runtime create <id> --bundle <path>\n")
		os.Exit(1)
	}

	containerID := args[0]
	bundlePath := ""
	for i := 1; i < len(args)-1; i++ {
		if args[i] == "--bundle" {
			bundlePath = args[i+1]
			break
		}
	}
	if bundlePath == "" {
		fmt.Fprintf(os.Stderr, "错误: 必须指定 --bundle <path>\n")
		os.Exit(1)
	}

	// 加载 OCI Spec
	ociSpec, err := loadOCISpec(bundlePath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "错误: 加载 OCI Spec 失败: %v\n", err)
		os.Exit(1)
	}

	// 转换为 libcontainer 配置
	config := convertToConfig(ociSpec, bundlePath)
	config.BundlePath = bundlePath

	// 创建容器
	container, err := libcontainer.New(containerID, config)
	if err != nil {
		fmt.Fprintf(os.Stderr, "错误: 创建容器失败: %v\n", err)
		os.Exit(1)
	}

	// 启动容器（阻塞在 FIFO 等待 start 信号）
	process := &libcontainer.Process{
		Args:     []string{"/bin/sh"},
		Terminal: ociSpec.Process != nil && ociSpec.Process.Terminal,
	}

	if ociSpec.Process != nil && len(ociSpec.Process.Args) > 0 {
		process.Args = ociSpec.Process.Args
	}

	if err := container.Start(process); err != nil {
		fmt.Fprintf(os.Stderr, "错误: 启动容器失败: %v\n", err)
		os.Exit(1)
	}

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
	f.Write([]byte("start\n"))
	f.Close()
	os.Remove(fifoPath)

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
		// 容器可能已经不存在，尝试清理状态文件
		os.RemoveAll(fmt.Sprintf("/var/lib/mini-docker/runtime/%s", containerID))
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
		Status: status.String(),
		Pid:    container.Pid(),
	}

	data, _ := json.MarshalIndent(state, "", "  ")
	fmt.Println(string(data))
	os.Exit(0)
}

// loadOCISpec 加载 OCI Spec
func loadOCISpec(bundlePath string) (*ociSpec, error) {
	configPath := bundlePath + "/config.json"
	data, err := os.ReadFile(configPath)
	if err != nil {
		return nil, err
	}

	var spec ociSpec
	if err := json.Unmarshal(data, &spec); err != nil {
		return nil, err
	}

	return &spec, nil
}

// convertToConfig 将 OCI Spec 转换为 libcontainer 配置
func convertToConfig(spec *ociSpec, bundlePath string) *configs.Config {
	config := &configs.Config{
		Hostname: spec.Hostname,
	}

	if spec.Root != nil {
		rootfs := spec.Root.Path
		if !filepath.IsAbs(rootfs) {
			rootfs = filepath.Join(bundlePath, rootfs)
		}
		config.Rootfs = filepath.Clean(rootfs)
		config.ReadonlyRootfs = spec.Root.Readonly
	}

	if spec.Process != nil {
		config.Args = spec.Process.Args
		config.Env = spec.Process.Env
		config.Cwd = spec.Process.Cwd
		if spec.Process.Terminal {
			// 标记需要终端
		}
	}

	if spec.Linux != nil {
		for _, ns := range spec.Linux.Namespaces {
			config.Namespaces = append(config.Namespaces, configs.Namespace{
				Type: ns.Type,
				Path: ns.Path,
			})
		}

		if spec.Linux.Resources != nil {
			config.Cgroups = &configs.Resources{}
			if spec.Linux.Resources.Memory != nil {
				config.Cgroups.Memory = &configs.Memory{
					Limit: spec.Linux.Resources.Memory.Limit,
				}
			}
			if spec.Linux.Resources.CPU != nil {
				config.Cgroups.CPU = &configs.CPU{
					Shares: spec.Linux.Resources.CPU.Shares,
					Cpus:   spec.Linux.Resources.CPU.Cpus,
				}
			}
			if spec.Linux.Resources.Pids != nil {
				config.Cgroups.Pids = &configs.Pids{
					Limit: spec.Linux.Resources.Pids.Limit,
				}
			}
		}
	}

	// 转换挂载点
	for _, m := range spec.Mounts {
		config.Mounts = append(config.Mounts, &configs.Mount{
			Destination: m.Destination,
			Type:        m.Type,
			Source:      m.Source,
			Options:     m.Options,
		})
	}

	return config
}

type ociSpec struct {
	OCIVersion string `json:"ociVersion"`
	Hostname   string `json:"hostname"`
	Root       *struct {
		Path     string `json:"path"`
		Readonly bool   `json:"readonly"`
	} `json:"root"`
	Process *struct {
		Terminal bool     `json:"terminal"`
		Args     []string `json:"args"`
		Env      []string `json:"env"`
		Cwd      string   `json:"cwd"`
	} `json:"process"`
	Mounts []struct {
		Destination string   `json:"destination"`
		Type        string   `json:"type"`
		Source      string   `json:"source"`
		Options     []string `json:"options"`
	} `json:"mounts"`
	Linux *struct {
		Namespaces []struct {
			Type string `json:"type"`
			Path string `json:"path"`
		} `json:"namespaces"`
		Resources *struct {
			Memory *struct {
				Limit *int64 `json:"limit"`
			} `json:"memory"`
			CPU *struct {
				Shares *int64  `json:"shares"`
				Quota  *int64  `json:"quota"`
				Period *int64  `json:"period"`
				Cpus   *string `json:"cpus"`
			} `json:"cpu"`
			Pids *struct {
				Limit *int64 `json:"limit"`
			} `json:"pids"`
		} `json:"resources"`
	} `json:"linux"`
}

func waitForExit(pid int, timeoutMs int) {
	deadline := timeoutMs / 100
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
