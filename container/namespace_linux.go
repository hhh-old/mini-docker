//go:build linux

package container

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"

	"golang.org/x/sys/unix"
)

const (
	//这些都是整型常量,是十六进制的，类似：
	//const CLONE_NEWUTS  = 0x04000000
	//const CLONE_NEWIPC  = 0x08000000
	//const CLONE_NEWPID  = 0x20000000
	//每一位是一个01开关，NewNamespaceFlags函数使用按位或（|）操作符将它们组合起来。
	CLONE_NEWUTS  = unix.CLONE_NEWUTS // 隔离主机名和域名 (Hostname)
	CLONE_NEWIPC  = unix.CLONE_NEWIPC // 隔离进程间通信 (信号量、消息队列等)
	CLONE_NEWPID  = unix.CLONE_NEWPID // 隔离进程编号 (让容器内出现 PID=1 的进程)
	CLONE_NEWNS   = unix.CLONE_NEWNS  // 隔离挂载点 (文件系统视图)
	CLONE_NEWNET  = unix.CLONE_NEWNET // 隔离网络 (网卡、IP、路由表)
	CLONE_NEWUSER = unix.CLONE_NEWUSER
)

// NewNamespaceFlags 创建默认的 Namespace 标志位组合
// 对齐 Docker 的默认隔离：UTS + IPC + PID + Mount + Network
func NewNamespaceFlags() uintptr {
	return uintptr(
		unix.CLONE_NEWUTS |
			unix.CLONE_NEWIPC |
			unix.CLONE_NEWPID |
			unix.CLONE_NEWNS |
			unix.CLONE_NEWNET,
	)
}

// NewUserNamespaceFlags 创建包含 User Namespace 的标志位组合
// User Namespace 允许容器内 root 映射为宿主机普通用户（rootless 容器）
//
// Docker 的 User Namespace 映射：
// ┌──────────────┬──────────────┐
// │ 容器内 UID   │ 宿主机 UID   │
// ├──────────────┼──────────────┤
// │ 0 (root)     │ 100000       │
// │ 1            │ 100001       │
// │ ...          │ ...          │
// │ 65534        │ 165534       │
// └──────────────┴──────────────┘
//
// 这样即使容器内进程自认为是 root，实际上在宿主机只是普通用户，
// 无法破坏宿主机文件系统。
func NewUserNamespaceFlags() uintptr {
	return uintptr(
		unix.CLONE_NEWUTS |
			unix.CLONE_NEWIPC |
			unix.CLONE_NEWPID |
			unix.CLONE_NEWNS |
			unix.CLONE_NEWNET |
			unix.CLONE_NEWUSER,
	)
}

func GetNamespacePath(pid int, nsType string) string {
	return fmt.Sprintf("/proc/%d/ns/%s", pid, nsType)
}

func ForkWithNamespaces(cmd *exec.Cmd, flags uintptr) (*os.Process, error) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags: flags,
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("启动子进程失败: %w", err)
	}

	return cmd.Process, nil
}

func SetNamespace(nsPath string) error {
	fd, err := unix.Open(nsPath, unix.O_RDONLY, 0)
	if err != nil {
		return fmt.Errorf("打开 namespace 文件失败 %s: %w", nsPath, err)
	}
	defer unix.Close(fd)

	if err := unix.Setns(fd, 0); err != nil {
		return fmt.Errorf("setns 失败: %w", err)
	}

	return nil
}

func setCloneFlags(cmd *exec.Cmd, flags uintptr, tty bool) {
	//通过 Cloneflags 在 fork 时创建新的命名空间
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags: flags,
	}
	//终端控制设置（ -it 的效果）
	if tty {
		cmd.SysProcAttr.Setctty = true // 设置控制终端
		cmd.SysProcAttr.Setsid = true  // 创建新的会话
	}

	// 如果包含 User Namespace，配置 UID/GID 映射
	if flags&uintptr(unix.CLONE_NEWUSER) != 0 {
		// Go 的 SysProcAttr 支持 User Namespace 的 UID/GID 映射
		// 容器内 UID 0 → 宿主机当前用户
		cmd.SysProcAttr.UidMappings = []syscall.SysProcIDMap{
			{ContainerID: 0, HostID: os.Getuid(), Size: 1},
		}
		cmd.SysProcAttr.GidMappings = []syscall.SysProcIDMap{
			{ContainerID: 0, HostID: os.Getgid(), Size: 1},
		}
	}
}

func setHostname(name string) error {
	return unix.Sethostname([]byte(name))
}

func sendSignal(pid int, sig int) error {
	return unix.Kill(pid, syscall.Signal(sig))
}

func checkProcessAlive(pid int) error {
	process, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return process.Signal(syscall.Signal(0))
}

func syscallExec(argv0 string, argv []string, envv []string) error {
	return syscall.Exec(argv0, argv, envv)
}
