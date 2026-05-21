//go:build linux

package libcontainer

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"mini-docker/libcontainer/cgroups"
	"mini-docker/libcontainer/configs"

	"golang.org/x/sys/unix"
)

// linuxContainer Linux 容器实现（对标 libcontainer/container_linux.go）
type linuxContainer struct {
	id      string
	config  *configs.Config
	pid     int
	status  Status
	cgm     cgroups.Manager
	initCmd *exec.Cmd
}

func newLinuxContainer(id string, config *configs.Config) (*linuxContainer, error) {
	if err := Validate(config); err != nil {
		return nil, fmt.Errorf("配置验证失败: %w", err)
	}

	c := &linuxContainer{
		id:     id,
		config: config,
		status: StatusCreated,
	}

	if config.Cgroups != nil {
		cgm, err := cgroups.NewManager(config.Cgroups, "mini-docker-"+id[:12])
		if err != nil {
			return nil, fmt.Errorf("创建 cgroup 管理器失败: %w", err)
		}
		c.cgm = cgm
	}

	return c, nil
}

func loadLinuxContainer(id string) (*linuxContainer, error) {
	state, err := loadContainerState(id)
	if err != nil {
		return nil, err
	}

	// 检查进程是否存活
	if state.Pid > 0 {
		if err := syscall.Kill(state.Pid, 0); err != nil {
			state.Status = StatusStopped
		}
	}

	// 从 bundle 路径加载配置，如果没有则从默认目录加载
	configPath := filepath.Join(getContainerDir(id), "config.json")
	if state.BundlePath != "" {
		configPath = filepath.Join(state.BundlePath, "config.json")
	}

	configData, err := os.ReadFile(configPath)
	if err != nil {
		return nil, fmt.Errorf("读取容器配置失败: %w", err)
	}

	var config configs.Config
	if err := json.Unmarshal(configData, &config); err != nil {
		return nil, fmt.Errorf("解析容器配置失败: %w", err)
	}

	config.BundlePath = state.BundlePath
	config.Rootfs = state.Rootfs

	return &linuxContainer{
		id:     id,
		config: &config,
		pid:    state.Pid,
		status: state.Status,
	}, nil
}

func listLinuxContainers() ([]Container, error) {
	ids, err := listContainerIDs()
	if err != nil {
		return nil, err
	}

	var containers []Container
	for _, id := range ids {
		c, err := loadLinuxContainer(id)
		if err != nil {
			continue
		}
		containers = append(containers, c)
	}

	return containers, nil
}

func (c *linuxContainer) ID() string {
	return c.id
}

func (c *linuxContainer) Status() (Status, error) {
	if c.status == StatusRunning || c.status == StatusCreated {
		if err := syscall.Kill(c.pid, 0); err != nil {
			c.status = StatusStopped
		}
	}
	return c.status, nil
}

func (c *linuxContainer) Config() configs.Config {
	return *c.config
}

func (c *linuxContainer) Pid() int {
	return c.pid
}

func (c *linuxContainer) Start(process *Process) error {
	if c.status == StatusRunning {
		return fmt.Errorf("容器已在运行")
	}

	// FIFO 创建在 bundle 目录（宿主机文件系统），而非 rootfs 内部
	// 这样 init 进程可以在 pivot_root 之前用完整路径打开，不受 overlay/pivot_root 影响
	fifoPath := filepath.Join(c.config.BundlePath, ".start-fifo")

	// 创建 FIFO
	if err := unix.Mkfifo(fifoPath, 0600); err != nil {
		return fmt.Errorf("创建 FIFO 失败: %w", err)
	}

	// 创建 ready 信号 pipe
	pipeRead, pipeWrite, err := os.Pipe()
	if err != nil {
		return fmt.Errorf("创建 pipe 失败: %w", err)
	}

	// 构建 init 命令（FIFO 使用 bundle 目录的完整路径）
	cmd := exec.Command("/proc/self/exe", "init", "--bundle", c.config.BundlePath, "--fifo", fifoPath)
	cmd.ExtraFiles = []*os.File{pipeWrite}
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags: c.config.Namespaces.CloneFlags(),
	}
	if process.Terminal {
		cmd.SysProcAttr.Setctty = true
		cmd.SysProcAttr.Setsid = true
	}

	cmd.Stdin = process.Stdin
	cmd.Stdout = process.Stdout
	cmd.Stderr = process.Stderr

	// 启动 init 进程
	if err := cmd.Start(); err != nil {
		os.Remove(fifoPath)
		return fmt.Errorf("启动 init 进程失败: %w", err)
	}

	c.initCmd = cmd

	// 关闭父进程的 pipe 写端
	pipeWrite.Close()

	// 等待 init 进程 ready 信号
	readyBuf := make([]byte, 16)
	n, err := pipeRead.Read(readyBuf)
	pipeRead.Close()
	if err != nil || n == 0 {
		os.Remove(fifoPath)
		return fmt.Errorf("等待 ready 信号失败: %w", err)
	}

	// 保存容器状态
	c.pid = cmd.Process.Pid
	c.status = StatusCreated

	state := &ContainerState{
		ID:         c.id,
		Pid:        c.pid,
		BundlePath: c.config.BundlePath,
		Rootfs:     c.config.Rootfs,
		Status:     c.status,
	}
	if err := saveContainerState(state); err != nil {
		return fmt.Errorf("保存容器状态失败: %w", err)
	}

	return nil
}

func (c *linuxContainer) Run(process *Process) error {
	// 1. 创建并启动（阻塞在 FIFO）
	if err := c.Start(process); err != nil {
		return err
	}

	// 2. 发送 start 信号
	return c.sendStartSignal()
}

func (c *linuxContainer) sendStartSignal() error {
	// 使用配置中的 BundlePath
	bundlePath := c.config.BundlePath
	if bundlePath == "" {
		bundlePath = getContainerDir(c.id)
	}
	fifoPath := filepath.Join(bundlePath, "start-fifo")

	// 打开 FIFO 写入端
	f, err := os.OpenFile(fifoPath, os.O_WRONLY, 0)
	if err != nil {
		return fmt.Errorf("打开 FIFO 失败: %w", err)
	}
	f.Write([]byte("start\n"))
	f.Close()

	// 清理 FIFO
	os.Remove(fifoPath)

	// 更新状态
	c.status = StatusRunning
	state := &ContainerState{
		ID:         c.id,
		Pid:        c.pid,
		BundlePath: bundlePath,
		Rootfs:     c.config.Rootfs,
		Status:     c.status,
	}
	return saveContainerState(state)
}

func (c *linuxContainer) Destroy() error {
	// 如果容器还在运行，先停止
	if c.status == StatusRunning || c.status == StatusCreated {
		c.Signal(int(syscall.SIGKILL))
		time.Sleep(100 * time.Millisecond)
	}

	// 销毁 cgroup
	if c.cgm != nil {
		c.cgm.Destroy()
	}

	// 删除状态
	return removeContainerState(c.id)
}

func (c *linuxContainer) Pause() error {
	if c.status != StatusRunning {
		return fmt.Errorf("容器未在运行")
	}
	if c.cgm == nil {
		return fmt.Errorf("cgroup 管理器未初始化")
	}
	if err := c.cgm.Freeze(); err != nil {
		return err
	}
	c.status = StatusPaused
	return nil
}

func (c *linuxContainer) Resume() error {
	if c.status != StatusPaused {
		return fmt.Errorf("容器未暂停")
	}
	if c.cgm == nil {
		return fmt.Errorf("cgroup 管理器未初始化")
	}
	if err := c.cgm.Thaw(); err != nil {
		return err
	}
	c.status = StatusRunning
	return nil
}

func (c *linuxContainer) Signal(sig int) error {
	if c.pid <= 0 {
		return fmt.Errorf("容器 PID 无效")
	}
	return syscall.Kill(c.pid, syscall.Signal(sig))
}

func (c *linuxContainer) Exec(process *Process) error {
	if c.status != StatusRunning {
		return fmt.Errorf("容器未在运行")
	}

	args := []string{
		"nsenter",
		"-t", fmt.Sprintf("%d", c.pid),
		"-m", "-u", "-i", "-n", "-p", "--",
	}
	args = append(args, process.Args...)

	cmd := exec.Command(args[0], args[1:]...)
	cmd.Stdin = process.Stdin
	cmd.Stdout = process.Stdout
	cmd.Stderr = process.Stderr
	return cmd.Run()
}

func (c *linuxContainer) Stats() (*Stats, error) {
	stats := &Stats{}
	if c.cgm != nil {
		cgStats, err := c.cgm.GetStats()
		if err != nil {
			return nil, err
		}
		stats.CgroupStats = cgStats
	}
	return stats, nil
}

func (c *linuxContainer) Set(config configs.Resources) error {
	if c.cgm == nil {
		return fmt.Errorf("cgroup 管理器未初始化")
	}
	return c.cgm.Set(&config)
}
