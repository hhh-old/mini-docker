//go:build linux

package libcontainer

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"mini-docker/libcontainer/cgroups"
	"mini-docker/libcontainer/configs"

	"golang.org/x/sys/unix"
)

// linuxContainer Linux 容器实现（对标 libcontainer/container_linux.go）
type linuxContainer struct {
	mu      sync.Mutex
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
		shortID := id
		if len(shortID) > 12 {
			shortID = shortID[:12]
		}
		cgm, err := cgroups.NewManager(config.Cgroups, "mini-docker-"+shortID)
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

	lc := &linuxContainer{
		id:     id,
		config: &config,
		pid:    state.Pid,
		status: state.Status,
	}

	if config.Cgroups != nil {
		shortID := id
		if len(shortID) > 12 {
			shortID = shortID[:12]
		}
		cgm, err := cgroups.NewManager(config.Cgroups, "mini-docker-"+shortID)
		if err == nil {
			lc.cgm = cgm
		}
	}

	return lc, nil
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
	c.mu.Lock()
	defer c.mu.Unlock()
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
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.pid
}

func (c *linuxContainer) Start(process *Process) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.status == StatusRunning {
		return fmt.Errorf("容器已在运行")
	}

	// FIFO 创建在 bundle 目录（宿主机文件系统），而非 rootfs 内部
	// 这样 init 进程可以在 pivot_root 之前用完整路径打开，不受 overlay/pivot_root 影响
	fifoPath := filepath.Join(c.config.BundlePath, ".start-fifo")

	// 创建 FIFO
	os.Remove(fifoPath)
	if err := unix.Mkfifo(fifoPath, 0600); err != nil {
		return fmt.Errorf("创建 FIFO 失败: %w", err)
	}

	// 创建 ready 信号 pipe,用来做子进程和当前进程的同步工作，当 cmd.Start()子进程做好前置工作以后给当前进程发送一个消息
	pipeRead, pipeWrite, err := os.Pipe()
	if err != nil {
		return fmt.Errorf("创建 pipe 失败: %w", err)
	}

	// 构建 init 命令（FIFO 使用 bundle 目录的完整路径）
	cmd := exec.Command("/proc/self/exe", "init", "--bundle", c.config.BundlePath, "--fifo", fifoPath)
	cmd.ExtraFiles = []*os.File{pipeWrite} // 传入 Pipe 写端
	cmd.SysProcAttr = &syscall.SysProcAttr{
		//Cloneflags 包含了 CLONE_NEWPID、CLONE_NEWNS、CLONE_NEWNET 等，指示内核在clone派生子进程时将其放入全新的隔离空间。
		Cloneflags: c.config.Namespaces.CloneFlags(), // 开启 Namespace 隔离！
	}
	if process.Terminal {
		cmd.SysProcAttr.Setctty = true
		cmd.SysProcAttr.Setsid = true
	}
	//将这个cmd进程(容器进程)的标准输入、输出、错误绑定到等号右边的东西上
	//tty模式：等号右边的是从shim进程中传递过来的tty设备对应的文件，打通shim进程和容器进程的标准输入、输出、错误的交互
	//非tty模式：等号右边的是从shim进程中传递过来的,非tty模式process.Stdin是nil，因为后台运行不需要shim传递输入到容器
	cmd.Stdin = process.Stdin
	cmd.Stdout = process.Stdout
	cmd.Stderr = process.Stderr

	// 启动 init 进程
	if err := cmd.Start(); err != nil {
		pipeRead.Close()
		pipeWrite.Close()
		os.Remove(fifoPath)
		return fmt.Errorf("启动 init 进程失败: %w", err)
	}

	c.initCmd = cmd

	// 关闭父进程的 pipe 写端
	pipeWrite.Close()

	// 等待 init 进程 ready 信号（带超时）
	readyDone := make(chan struct{})
	var readyErr error
	go func() {
		readyBuf := make([]byte, 16)
		n, err := pipeRead.Read(readyBuf) //阻塞点。等待容器init进程发送ready信号
		//pipeRead.Read 会一直卡住，直到以下两种情况发生之一：
		//成功：容器内 init 进程顺利完成初始化，向它的 pipeWrite 写入了就绪信号。此时 Read 返回，n > 0。
		//失败：容器内的进程在初始化阶段意外崩溃或被杀死了。由于进程死亡，操作系统会自动关闭它持有的 pipeWrite。一旦写端全部关闭，读端 Read 就会立刻收到 EOF 信号而返回，此时 n == 0，并且返回一个代表结束的 err
		pipeRead.Close()
		if err != nil || n == 0 {
			readyErr = fmt.Errorf("等待 ready 信号失败: %w", err)
		}
		close(readyDone)
	}()

	select {
	case <-readyDone: //主协程阻塞等待init进程的ready信号
		if readyErr != nil { // 如果 readyErr 记录了错误，说明子进程崩溃了，父进程赶紧将其杀灭清理
			cmd.Process.Kill()
			cmd.Wait()
			os.Remove(fifoPath)
			return readyErr
		}
		// 如果没有 error，说明子进程确实 ready 了，主程序顺利往下走
	case <-time.After(30 * time.Second): //等待超时
		pipeRead.Close()
		cmd.Process.Kill()
		cmd.Wait()
		os.Remove(fifoPath)
		return fmt.Errorf("等待 ready 信号超时")
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
	fifoPath := filepath.Join(bundlePath, ".start-fifo")

	// 打开 FIFO 写入端
	f, err := os.OpenFile(fifoPath, os.O_WRONLY, 0)
	if err != nil {
		return fmt.Errorf("打开 FIFO 失败: %w", err)
	}
	if _, err := f.Write([]byte("start\n")); err != nil {
		f.Close()
		return fmt.Errorf("写入 FIFO 失败: %w", err)
	}
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
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.status == StatusRunning || c.status == StatusCreated {
		c.Signal(int(syscall.SIGKILL))
		if c.initCmd != nil {
			c.initCmd.Process.Wait()
		} else {
			time.Sleep(100 * time.Millisecond)
		}
	}

	// 销毁 cgroup
	if c.cgm != nil {
		c.cgm.Destroy()
	}

	// 删除状态
	return removeContainerState(c.id)
}

func (c *linuxContainer) Pause() error {
	c.mu.Lock()
	defer c.mu.Unlock()

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
	c.mu.Lock()
	defer c.mu.Unlock()

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
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.pid <= 0 {
		return fmt.Errorf("容器 PID 无效")
	}
	return syscall.Kill(c.pid, syscall.Signal(sig))
}

func (c *linuxContainer) Exec(process *Process) error {
	c.mu.Lock()
	if c.status != StatusRunning {
		c.mu.Unlock()
		return fmt.Errorf("容器未在运行")
	}
	pid := c.pid
	c.mu.Unlock()

	args := []string{
		"nsenter",
		"-t", fmt.Sprintf("%d", pid),
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

func (c *linuxContainer) Set(configs.Resources) error {
	return nil
}

func (c *linuxContainer) SetStatus(status Status) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.status = status
	state := &ContainerState{
		ID:         c.id,
		Pid:        c.pid,
		BundlePath: c.config.BundlePath,
		Rootfs:     c.config.Rootfs,
		Status:     c.status,
	}
	return saveContainerState(state)
}
