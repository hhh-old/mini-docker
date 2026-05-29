//go:build linux

package libcontainer

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"mini-docker/libcontainer/cgroups"
	"mini-docker/libcontainer/configs"
	"mini-docker/spec"

	"golang.org/x/sys/unix"
)

// linuxContainer Linux 容器实现（对标 libcontainer/container_linux.go）
type linuxContainer struct {
	mu       sync.Mutex
	runState containerRunState
	config   *configs.Config
	cgm      cgroups.Manager
}

// toContainerState 从运行时状态 + 配置构造完整的 ContainerState（对标 runc 的 currentState()）
// 序列化到 state.json 时的唯一入口，确保 BundlePath/Rootfs/OCIVersion 等配置字段不遗漏
func (c *linuxContainer) toContainerState() *ContainerState {
	return &ContainerState{
		OCIVersion: c.runState.OCIVersion,
		ID:         c.runState.ID,
		Pid:        c.runState.Pid,
		Bundle:     c.config.BundlePath,
		Rootfs:     c.config.Rootfs,
		Status:     c.runState.Status,
	}
}

func (c *linuxContainer) State() *ContainerState {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.toContainerState()
}

func newLinuxContainer(id string, config *configs.Config) (*linuxContainer, error) {
	if err := Validate(config); err != nil {
		return nil, fmt.Errorf("配置验证失败: %w", err)
	}

	c := &linuxContainer{
		runState: containerRunState{
			ID:         id,
			Status:     StatusCreated,
			OCIVersion: config.OCIVersion,
		},
		config: config,
	}

	if config.Cgroups != nil {
		cgm, err := cgroups.NewManager(config.Cgroups, "mini-docker-"+id)
		if err != nil {
			return nil, fmt.Errorf("创建 cgroup 管理器失败: %w", err)
		}
		c.cgm = cgm
	}

	return c, nil
}

// 使用容器状态持久化文件state.json来恢复出容器对象linuxContainer
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

	bundlePath := state.Bundle
	if bundlePath == "" {
		bundlePath = getContainerDir(id)
	}

	ociSpec, err := spec.LoadSpec(bundlePath)
	if err != nil {
		return nil, fmt.Errorf("加载 OCI Spec 失败: %w", err)
	}

	config := spec.SpecToConfig(ociSpec, bundlePath)
	config.BundlePath = bundlePath
	if state.Rootfs != "" {
		config.Rootfs = state.Rootfs
	}

	lc := &linuxContainer{
		runState: containerRunState{
			ID:         state.ID,
			Pid:        state.Pid,
			Status:     state.Status,
			OCIVersion: state.OCIVersion,
		},
		config: config,
	}

	if config.Cgroups != nil {
		cgm, err := cgroups.NewManager(config.Cgroups, "mini-docker-"+id)
		if err == nil {
			lc.cgm = cgm
		}
	}

	return lc, nil
}

func (c *linuxContainer) ID() string {
	return c.runState.ID
}

// 调用这个方法来查询、更新容器状态
func (c *linuxContainer) Status() (Status, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.runState.Status == StatusRunning || c.runState.Status == StatusCreated || c.runState.Status == StatusPaused {
		if c.runState.Pid > 0 {
			if err := syscall.Kill(c.runState.Pid, 0); err != nil {
				c.runState.Status = StatusStopped
			}
		}
	}
	return c.runState.Status, nil
}

func (c *linuxContainer) Config() configs.Config {
	return *c.config
}

func (c *linuxContainer) Pid() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.runState.Pid
}

func (c *linuxContainer) Start(process *Process) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.startLocked(process)
}

func (c *linuxContainer) startLocked(process *Process) error {

	if c.runState.Status == StatusRunning {
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
	//setsid() 做了三件事:
	//1. 创建新的会话 (Session)  → init 进程成为新会话的首领
	//2. 创建新的进程组          → init 进程成为新进程组的组长
	//3. 断开与原控制终端的关联  → init 进程不再受宿主机终端影响
	//Setctty = true 让内核将 cmd.Stdin （即 PTY Slave）设为 init 进程的 控制终端,也就是Setctty 将 PTY Slave (/dev/pts/X) 设为它的控制终端
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
	c.runState.Pid = cmd.Process.Pid
	c.runState.Status = StatusCreated

	// Apply cgroup（对标 Docker/runc：cgroup 在 create 阶段应用）
	// 容器进程从第一行代码开始就受 cgroup 约束
	if c.cgm != nil {
		if err := c.cgm.Apply(c.runState.Pid); err != nil {
			log.Printf("警告: 应用 cgroup 失败: %v\n", err)
		}
	}

	if err := saveContainerState(c.toContainerState()); err != nil {
		return fmt.Errorf("保存容器状态失败: %w", err)
	}

	return nil
}

func (c *linuxContainer) Run(process *Process) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if err := c.startLocked(process); err != nil {
		return err
	}
	return c.execStartLocked()
}

// runtime start信号走的路径
func (c *linuxContainer) ExecStart() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.execStartLocked()
}

func (c *linuxContainer) execStartLocked() error {
	if c.runState.Status != StatusCreated {
		return fmt.Errorf("容器状态必须是 created，当前: %s", c.runState.Status)
	}
	return c.sendStartSignalLocked()
}

func (c *linuxContainer) sendStartSignalLocked() error {
	// 使用配置中的 BundlePath
	bundlePath := c.config.BundlePath
	if bundlePath == "" {
		bundlePath = getContainerDir(c.runState.ID)
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
	c.runState.Status = StatusRunning
	return saveContainerState(c.toContainerState())
}

func (c *linuxContainer) Destroy() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.runState.Status == StatusRunning || c.runState.Status == StatusCreated || c.runState.Status == StatusPaused {
		c.signalLocked(int(syscall.SIGKILL)) //给容器init机场呢发送一个杀死信号
		if c.runState.Pid > 0 {
			c.waitForProcessExit(c.runState.Pid, 10000)
		}
	}

	if c.cgm != nil {
		c.cgm.Destroy()
	}

	return removeContainerState(c.runState.ID)
}

func (c *linuxContainer) waitForProcessExit(pid int, timeoutMs int) {
	deadline := timeoutMs / 10
	for i := 0; i < deadline; i++ {
		if err := syscall.Kill(pid, 0); err != nil { //发一个探针消息过去查看是否进程已经终止， syscall.Kill(pid, 0) 中，信号参数 0 是一个特殊的保留值，它的含义是：不发送任何实际信号，仅检查目标进程是否存在以及当前用户是否有权限向其发送信号
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	log.Printf("警告: 等待容器进程 %d 退出超时\n", pid)
}

func (c *linuxContainer) Pause() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.runState.Status != StatusRunning {
		return fmt.Errorf("容器未在运行")
	}
	if c.cgm == nil {
		return fmt.Errorf("cgroup 管理器未初始化")
	}
	if err := c.cgm.Freeze(); err != nil {
		return err
	}
	c.runState.Status = StatusPaused
	return saveContainerState(c.toContainerState())
}

func (c *linuxContainer) Resume() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.runState.Status != StatusPaused {
		return fmt.Errorf("容器未暂停")
	}
	if c.cgm == nil {
		return fmt.Errorf("cgroup 管理器未初始化")
	}
	if err := c.cgm.Thaw(); err != nil {
		return err
	}
	c.runState.Status = StatusRunning
	return saveContainerState(c.toContainerState())
}

func (c *linuxContainer) Signal(sig int) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.signalLocked(sig)
}

func (c *linuxContainer) signalLocked(sig int) error {
	if c.runState.Pid <= 0 {
		return fmt.Errorf("容器 PID 无效")
	}
	//kill 在 Linux 中不仅仅是"杀死"进程，它的本质是 向进程发送信号 。不同信号有不同效果：
	//信号	    编号	效果
	//SIGTERM	15	优雅终止（进程可以捕获并做清理）
	//SIGKILL	9	强制杀死（进程无法捕获，立即终止）
	//SIGHUP	1	挂起（常用于重载配置）
	//SIGINT	2	中断（相当于 Ctrl+C）
	//SIGSTOP	19	暂停进程
	//SIGCONT	18	恢复暂停的进程
	return syscall.Kill(c.runState.Pid, syscall.Signal(sig))
}

func (c *linuxContainer) Exec(process *Process) error {
	c.mu.Lock()
	if c.runState.Status != StatusRunning {
		c.mu.Unlock()
		return fmt.Errorf("容器未在运行")
	}
	pid := c.runState.Pid
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
	if !isValidTransition(c.runState.Status, status) {
		return fmt.Errorf("非法状态转换: %s -> %s", c.runState.Status, status)
	}
	c.runState.Status = status
	return saveContainerState(c.toContainerState())
}

func isValidTransition(from, to Status) bool {
	validTransitions := map[Status][]Status{
		StatusCreated: {StatusRunning, StatusStopped},
		StatusRunning: {StatusPaused, StatusStopped},
		StatusPaused:  {StatusRunning, StatusStopped},
	}
	for _, allowed := range validTransitions[from] {
		if allowed == to {
			return true
		}
	}
	return false
}
