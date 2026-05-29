package containerd

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"mini-docker/constants"
	"mini-docker/container"
	"mini-docker/libcontainer"
	"mini-docker/spec"
	"mini-docker/types"
	"mini-docker/utils"
)

const (
	runtimeDir = constants.RuntimeDir
	shimDir    = constants.ShimDir
)

type Service struct{}

func NewService() *Service {
	return &Service{}
}

func (s *Service) CreateTask(info *container.ContainerInfo) (shimPID int, err error) {
	if info.ID == "" {
		return 0, fmt.Errorf("容器 ID 不能为空")
	}
	bundlePath := filepath.Join(runtimeDir, info.ID, "bundle")
	ociSpec := buildOCISpec(info)
	if err := spec.SaveSpec(ociSpec, bundlePath); err != nil {
		return 0, fmt.Errorf("保存 config.json 失败: %w", err)
	}
	//启动shim进程
	cmd := exec.Command("/proc/self/exe",
		"shim", info.ID, bundlePath)
	cmd.SysProcAttr = newShimSysProcAttr()
	logDir := filepath.Join(filepath.Dir(constants.DaemonLogPath), "shim")
	if err := os.MkdirAll(logDir, 0755); err != nil {
		return 0, fmt.Errorf("创建 shim 日志目录失败: %w", err)
	}
	logPath := filepath.Join(logDir, info.ID+".log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	//这段代码的作用是重定向 Shim 进程的标准输出（Stdout）和标准错误（Stderr）到指定的日志文件中
	if err == nil {
		//在 Linux 中，一个进程在运行时，默认有三个标准的 I/O 通道：
		//Stdout（标准输出，文件描述符 1）：进程正常打印的信息（如 fmt.Println）。
		//Stderr（标准错误，文件描述符 2）：进程报错、异常崩溃（Panic）或输出的错误日志。
		//代码中的 cmd 代表即将启动的 Shim 进程（即后台监管容器的垫片进程）
		cmd.Stdout = logFile
		cmd.Stderr = logFile
	}

	if err := cmd.Start(); err != nil {
		if logFile != nil {
			logFile.Close()
		}
		return 0, fmt.Errorf("启动 shim 失败: %w", err)
	}

	if logFile != nil {
		logFile.Close()
	}

	shimPID = cmd.Process.Pid
	//同步等待就绪：调用 waitForSocket 轮询等待 Shim 创建的 Unix 套接字 shim.sock 出现。一旦就绪，说明 Shim 已经成功启动并做好了管理准备，此时才向 Daemon 返回
	socketPath := filepath.Join(shimDir, info.ID, "shim.sock")
	if err := waitForSocket(socketPath, constants.SocketWaitTimeout); err != nil {
		cmd.Process.Kill()
		cmd.Wait()
		return 0, fmt.Errorf("shim socket 未就绪: %w", err)
	}

	return shimPID, nil
}

func (s *Service) KillTask(containerID string, signal syscall.Signal) error {
	return shimCall(containerID, types.ShimRequest{Type: "kill", Signal: int(signal)})
}

// 向指定的 Shim 发送 state 请求，获取容器当前的运行状态（如 running, stopped 等）
// Shim 不在线时降级为从磁盘加载 state.json
func (s *Service) GetTaskState(containerID string) (*libcontainer.ContainerState, error) {
	conn, err := connectShim(containerID)
	if err != nil {
		return libcontainer.LoadContainerState(containerID)
	}
	conn.Close()

	var state libcontainer.ContainerState
	if err := shimCallWithData(containerID, types.ShimRequest{Type: "state"}, &state); err != nil {
		return nil, err
	}
	return &state, nil
}

// 如果容器还在运行或 Shim 还活着，通过套接字向 Shim 发送 exit_info 请求获取。
// 优雅的降级处理：如果 Shim 已经退出（连接失败），则退一步直接去读取磁盘上的归档文件 exit.json（调用 readExitInfoFromFile）
func (s *Service) GetExitInfo(containerID string) (*ExitInfo, error) {
	conn, err := connectShim(containerID)
	if err != nil {
		return readExitInfoFromFile(containerID)
	}
	conn.Close()

	var info ExitInfo
	if err := shimCallWithData(containerID, types.ShimRequest{Type: "exit_info"}, &info); err != nil {
		return nil, err
	}
	return &info, nil
}

func (s *Service) DeleteTask(containerID string) error {
	conn, err := connectShim(containerID)
	if err == nil {
		json.NewEncoder(conn).Encode(types.ShimRequest{Type: "shutdown"})
		conn.Close()
		shimPID := ReadShimPID(containerID)
		if shimPID > 0 {
			exited := false
			for i := 0; i < 30; i++ {
				if proc, e := os.FindProcess(shimPID); e == nil {
					if proc.Signal(syscall.Signal(0)) != nil {
						exited = true
						break
					}
				} else {
					exited = true
					break
				}
				time.Sleep(100 * time.Millisecond)
			}
			if !exited {
				if proc, e := os.FindProcess(shimPID); e == nil {
					proc.Signal(syscall.SIGKILL)
					for i := 0; i < 50; i++ {
						if proc.Signal(syscall.Signal(0)) != nil {
							break
						}
						time.Sleep(100 * time.Millisecond)
					}
				}
			}
		} else {
			time.Sleep(2 * time.Second)
		}
	} else {
		shimPID := ReadShimPID(containerID)
		if shimPID > 0 {
			if proc, e := os.FindProcess(shimPID); e == nil {
				proc.Signal(syscall.SIGKILL)
				for i := 0; i < 50; i++ {
					if proc.Signal(syscall.Signal(0)) != nil {
						break
					}
					time.Sleep(100 * time.Millisecond)
				}
			}
		}
	}

	containerPID := 0
	if info, err := readExitInfoFromFile(containerID); err != nil || info == nil {
		createdPath := filepath.Join(resolveShimDir(containerID), "created")
		if data, err := os.ReadFile(createdPath); err == nil {
			fmt.Sscanf(strings.TrimSpace(string(data)), "%d", &containerPID)
		}
	}

	if containerPID > 0 && utils.CheckProcessAlive(containerPID) {
		if proc, err := os.FindProcess(containerPID); err == nil {
			proc.Signal(syscall.SIGKILL)
			for i := 0; i < 50; i++ {
				if !utils.CheckProcessAlive(containerPID) {
					break
				}
				time.Sleep(100 * time.Millisecond)
			}
		}
	}

	stateDir := filepath.Join(runtimeDir, containerID)
	os.RemoveAll(stateDir)

	shimContainerDir := resolveShimDir(containerID)
	os.RemoveAll(shimContainerDir)

	return nil
}

func (s *Service) ShutdownShim(containerID string) {
	conn, err := connectShim(containerID)
	if err != nil {
		return
	}
	json.NewEncoder(conn).Encode(types.ShimRequest{Type: "shutdown"})
	conn.Close()
	time.Sleep(constants.ShutdownWaitTime)
}

// RestartShim 重启 shim 进程以接管已有的非 TTY 容器
// 用于 shim 崩溃后恢复：非 TTY 容器在 shim 崩溃后仍可存活（日志文件 fd 不受影响），
// 启动新的 shim 以 takeover 模式接管容器，恢复 Wait4 监控和控制 socket 服务
func (s *Service) RestartShim(containerID string, containerPID int) (int, error) {
	bundlePath := filepath.Join(runtimeDir, containerID, "bundle")
	if _, err := os.Stat(bundlePath); err != nil {
		return 0, fmt.Errorf("bundle 目录不存在: %w", err)
	}

	// 清理旧的 shim 资源（保留 container.log）
	shimContainerDir := resolveShimDir(containerID)
	os.Remove(filepath.Join(shimContainerDir, "shim.sock"))
	os.Remove(filepath.Join(shimContainerDir, "shim.pid"))
	os.Remove(filepath.Join(shimContainerDir, "created"))
	os.Remove(filepath.Join(shimContainerDir, "exit.json"))

	// 启动新的 shim 进程（takeover 模式）
	cmd := exec.Command("/proc/self/exe",
		"shim", containerID, bundlePath,
		"--takeover", fmt.Sprintf("%d", containerPID))
	cmd.SysProcAttr = newShimSysProcAttr()
	logDir := filepath.Join(filepath.Dir(constants.DaemonLogPath), "shim")
	logPath := filepath.Join(logDir, containerID+".log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err == nil {
		cmd.Stdout = logFile
		cmd.Stderr = logFile
	}
	if err := cmd.Start(); err != nil {
		if logFile != nil {
			logFile.Close()
		}
		return 0, fmt.Errorf("启动 shim 失败: %w", err)
	}
	if logFile != nil {
		logFile.Close()
	}

	shimPID := cmd.Process.Pid

	// 等待 shim socket 就绪
	socketPath := filepath.Join(shimContainerDir, "shim.sock")
	if err := waitForSocket(socketPath, constants.SocketWaitTimeout); err != nil {
		cmd.Process.Kill()
		cmd.Wait()
		return 0, fmt.Errorf("shim socket 未就绪: %w", err)
	}

	return shimPID, nil
}

func (s *Service) PauseTask(containerID string) error {
	return shimCall(containerID, types.ShimRequest{Type: "pause"})
}

func (s *Service) ResumeTask(containerID string) error {
	return shimCall(containerID, types.ShimRequest{Type: "unpause"})
}

// WaitForCreate 等待容器创建完成（对齐 Docker: create 与 start 分离）
// 通过轮询 shim 的 created 文件获取容器 PID
func (s *Service) WaitForCreate(containerID string, timeout time.Duration) (int, error) {
	shimContainerDir := resolveShimDir(containerID)
	createdPath := filepath.Join(shimContainerDir, "created")

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(createdPath)
		if err == nil {
			pid, _ := strconv.Atoi(strings.TrimSpace(string(data)))
			if pid > 0 {
				return pid, nil
			}
		}
		time.Sleep(constants.PollInterval)
	}
	return 0, fmt.Errorf("等待容器 %s 创建超时", containerID)
}

// StartTask 通知 shim 执行 runtime start（对齐 Docker: Daemon → shim → runc start）
func (s *Service) StartTask(containerID string) error {
	return shimCall(containerID, types.ShimRequest{Type: "start"})
}

func (s *Service) ExecTaskStream(containerID string, args []string, tty bool) (net.Conn, error) {
	conn, err := connectShim(containerID)
	if err != nil {
		return nil, err
	}

	req := types.ShimRequest{Type: "exec", Args: args, Tty: tty}
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		conn.Close()
		return nil, fmt.Errorf("发送请求失败: %w", err)
	}

	var resp types.ShimResponse
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		conn.Close()
		return nil, fmt.Errorf("读取响应失败: %w", err)
	}
	if !resp.Success {
		conn.Close()
		return nil, fmt.Errorf("%s", resp.Message)
	}

	return conn, nil
}

// AttachTask 连接到容器的 TTY，返回一个可用于双向 I/O 转发的连接
// 对齐 Docker 的 attach 行为：通过 shim 的 attach 命令建立流式 I/O 通道
func (s *Service) AttachTask(containerID string) (net.Conn, error) {
	conn, err := connectShim(containerID)
	if err != nil {
		return nil, err
	}

	req := types.ShimRequest{Type: "attach"}
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		conn.Close()
		return nil, fmt.Errorf("发送 attach 请求失败: %w", err)
	}

	var resp types.ShimResponse
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		conn.Close()
		return nil, fmt.Errorf("读取 attach 响应失败: %w", err)
	}
	if !resp.Success {
		conn.Close()
		return nil, fmt.Errorf("attach 失败: %s", resp.Message)
	}
	//AttachTask 返回的 shimConn （ net.Conn ）是一条 持久化的双向流通道
	return conn, nil
}

func (s *Service) ResizeTask(containerID string, rows, cols uint16) error {
	return shimCall(containerID, types.ShimRequest{Type: "resize", Rows: rows, Cols: cols})
}

// ListTasks 扫描 runtime 目录，列出所有容器任务的状态
// 对齐 Docker: containerd 通过扫描 runc 的 state.json 列出所有任务
func (s *Service) ListTasks() ([]*libcontainer.ContainerState, error) {
	states, err := libcontainer.ListContainerStates()
	if err != nil {
		return nil, err
	}

	for _, state := range states {
		if state.Status == libcontainer.StatusRunning || state.Status == libcontainer.StatusCreated {
			proc, err := os.FindProcess(state.Pid)
			if err != nil || proc.Signal(syscall.Signal(0)) != nil {
				state.Status = libcontainer.StatusStopped
			}
		}
	}
	return states, nil
}

type ExitInfo = types.ExitInfo

func resolveShimDir(containerID string) string {
	return filepath.Join(shimDir, containerID)
}

func connectShim(containerID string) (net.Conn, error) {
	shimContainerDir := resolveShimDir(containerID)
	socketPath := filepath.Join(shimContainerDir, "shim.sock")
	conn, err := net.DialTimeout("unix", socketPath, 5*time.Second)
	if err != nil {
		return nil, fmt.Errorf("连接 shim 失败: %w", err)
	}
	return conn, nil
}

// shimCall 向 shim 发送请求并等待响应，仅检查 Success 字段
func shimCall(containerID string, req types.ShimRequest) error {
	conn, err := connectShim(containerID)
	if err != nil {
		return err
	}
	defer conn.Close()

	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return fmt.Errorf("发送%s请求失败: %w", req.Type, err)
	}

	var resp types.ShimResponse
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		return fmt.Errorf("读取%s响应失败: %w", req.Type, err)
	}
	if !resp.Success {
		return fmt.Errorf("%s失败: %s", req.Type, resp.Message)
	}
	return nil
}

// shimCallWithData 向 shim 发送请求，成功时将 resp.Data 反序列化到 result
func shimCallWithData(containerID string, req types.ShimRequest, result any) error {
	conn, err := connectShim(containerID)
	if err != nil {
		return err
	}
	defer conn.Close()

	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return fmt.Errorf("发送%s请求失败: %w", req.Type, err)
	}

	var resp types.ShimResponse
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		return fmt.Errorf("读取%s响应失败: %w", req.Type, err)
	}
	if !resp.Success {
		return fmt.Errorf("%s失败: %s", req.Type, resp.Message)
	}

	data, _ := json.Marshal(resp.Data)
	if err := json.Unmarshal(data, result); err != nil {
		return fmt.Errorf("解析%s数据失败: %w", req.Type, err)
	}
	return nil
}

func readExitInfoFromFile(containerID string) (*ExitInfo, error) {
	shimContainerDir := resolveShimDir(containerID)
	exitPath := filepath.Join(shimContainerDir, "exit.json")
	data, err := os.ReadFile(exitPath)
	if err != nil {
		return nil, fmt.Errorf("读取退出信息失败: %w", err)
	}
	var info ExitInfo
	if err := json.Unmarshal(data, &info); err != nil {
		return nil, fmt.Errorf("解析退出信息失败: %w", err)
	}
	return &info, nil
}

func waitForSocket(path string, timeout time.Duration) error {
	// 1. 计算“截止时间”（当前时间 + 传入的超时时长，比如 10 秒）
	deadline := time.Now().Add(timeout)
	// 2. 开启循环：只要当前时间还没过截止时间，就一直尝试
	for time.Now().Before(deadline) {
		// 3. 尝试连接 Unix 套接字
		// "unix" 表示 Unix Domain Socket，path 是套接字文件路径（如 .../shim.sock）
		conn, err := net.DialTimeout("unix", path, constants.ShimConnectTimeout)
		// 4. 判断连接是否成功
		if err == nil {
			// 如果 err 为 nil，说明连接成功，套接字已经准备好了！
			conn.Close() // 立刻关闭这个临时测试的连接，释放文件描述符，防止连接泄露
			return nil   // 成功返回，代表可以开始通信了
		}
		// 5. 如果连接失败（err != nil），说明套接字还没创建好
		//    让当前协程睡眠一小会儿（如 50ms），防止进入“死循环”导致 CPU 占满（Busy-waiting）
		time.Sleep(constants.PollInterval)
	}
	return fmt.Errorf("等待 socket %s 超时", path)
}

func buildOCISpec(info *container.ContainerInfo) *spec.Spec {
	return spec.DefaultSpec(&spec.SpecConfig{
		Tty:           info.Tty,
		Memory:        info.Memory,
		CPUShares:     info.CPUShares,
		Image:         info.Image,
		RootFS:        info.RootFS,
		Cmd:           info.Cmd,
		Volumes:       info.Volumes,
		Hostname:      info.Name,
		Network:       info.Network,
		RestartPolicy: info.RestartPolicy,
		OverlayMerged: info.OverlayMerged,
		OverlayUpper:  info.OverlayUpper,
		OverlayWork:   info.OverlayWork,
		PortMap:       info.PortMap,
		CgroupName:    info.CgroupName,
	})
}

func IsShimAlive(containerID string) bool {
	shimContainerDir := resolveShimDir(containerID)
	socketPath := filepath.Join(shimContainerDir, "shim.sock")
	conn, err := net.DialTimeout("unix", socketPath, constants.ShimConnectTimeout)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

func ReadShimPID(containerID string) int {
	pidPath := filepath.Join(resolveShimDir(containerID), "shim.pid")
	data, err := os.ReadFile(pidPath)
	if err != nil {
		return 0
	}
	var pid int
	fmt.Sscanf(strings.TrimSpace(string(data)), "%d", &pid)
	return pid
}

func ReadExitInfo(containerID string) (*ExitInfo, error) {
	return readExitInfoFromFile(containerID)
}
