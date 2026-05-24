package containerd

import (
	"encoding/json"
	"fmt"
	"mini-docker/cgroup"
	"mini-docker/utils"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"mini-docker/constants"
	"mini-docker/spec"
	"mini-docker/types"
)

const (
	runtimeDir = constants.RuntimeDir
	shimDir    = constants.ShimDir
)

type Service struct{}

func NewService() *Service {
	return &Service{}
}

type TaskConfig struct {
	ID            string
	OCIConfigJSON string
	Image         string
	Hostname      string
	Cmd           []string
	Tty           bool
	Memory        string
	CPUShares     string
	Network       string
	PortMap       string
	RestartPolicy string
	Volumes       []string
	RootFSPath    string
	Overlay       *types.OverlayDirs
}

// 启动shim进程，启动容器
func (s *Service) CreateTask(config *TaskConfig) (shimPID int, err error) {
	if config.ID == "" {
		return 0, fmt.Errorf("容器 ID 不能为空")
	}
	bundlePath := filepath.Join(runtimeDir, config.ID, "bundle")
	ociSpec := buildOCISpec(config)
	if err := spec.SaveSpec(ociSpec, bundlePath); err != nil {
		return 0, fmt.Errorf("保存 config.json 失败: %w", err)
	}
	//启动shim进程
	cmd := exec.Command("/proc/self/exe",
		"shim", config.ID, bundlePath)
	cmd.SysProcAttr = newShimSysProcAttr()
	logDir := filepath.Join(filepath.Dir(constants.DaemonLogPath), "shim")
	if err := os.MkdirAll(logDir, 0755); err != nil {
		return 0, fmt.Errorf("创建 shim 日志目录失败: %w", err)
	}
	logPath := filepath.Join(logDir, config.ID+".log")
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
	socketPath := filepath.Join(shimDir, config.ID, "shim.sock")
	if err := waitForSocket(socketPath, constants.SocketWaitTimeout); err != nil {
		cmd.Process.Kill()
		cmd.Wait()
		return 0, fmt.Errorf("shim socket 未就绪: %w", err)
	}

	return shimPID, nil
}

func (s *Service) KillTask(containerID string, signal syscall.Signal) error {
	conn, err := connectShim(containerID)
	if err != nil {
		return err
	}
	defer conn.Close()

	req := types.ShimRequest{Type: "kill", Signal: int(signal)}
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return fmt.Errorf("发送请求失败: %w", err)
	}

	var resp types.ShimResponse
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		return fmt.Errorf("读取响应失败: %w", err)
	}
	if !resp.Success {
		return fmt.Errorf("kill 失败: %s", resp.Message)
	}
	return nil
}

// 向指定的 Shim 发送 state 请求，获取容器当前的运行状态（如 running, stopped 等）
func (s *Service) GetTaskState(containerID string) (*spec.State, error) {
	conn, err := connectShim(containerID)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	req := types.ShimRequest{Type: "state"}
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return nil, fmt.Errorf("发送请求失败: %w", err)
	}

	var resp types.ShimResponse
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		return nil, fmt.Errorf("读取响应失败: %w", err)
	}
	if !resp.Success {
		return nil, fmt.Errorf("查询状态失败: %s", resp.Message)
	}

	data, _ := json.Marshal(resp.Data)
	var state spec.State
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("解析状态失败: %w", err)
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
	defer conn.Close()

	req := types.ShimRequest{Type: "exit_info"}
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return nil, fmt.Errorf("发送请求失败: %w", err)
	}

	var resp types.ShimResponse
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		return nil, fmt.Errorf("读取响应失败: %w", err)
	}
	if !resp.Success {
		return nil, fmt.Errorf("%s", resp.Message)
	}

	data, _ := json.Marshal(resp.Data)
	var info ExitInfo
	if err := json.Unmarshal(data, &info); err != nil {
		return nil, fmt.Errorf("解析退出信息失败: %w", err)
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
				if proc, e := os.FindProcess(shimPID); e == nil && proc.Signal(syscall.Signal(0)) != nil {
					exited = true
					break
				}
				time.Sleep(100 * time.Millisecond)
			}
			if !exited {
				if proc, e := os.FindProcess(shimPID); e == nil {
					proc.Signal(syscall.SIGKILL)
					proc.Wait()
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
				proc.Wait()
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

func (s *Service) PauseTask(containerID string) error {
	state, err := s.GetTaskState(containerID)
	if err != nil {
		return err
	}
	if state.Status != spec.StatusRunning {
		return fmt.Errorf("容器未在运行")
	}

	cgroupName := constants.CgroupPrefix + utils.FormatShortID(containerID)
	cg := &cgroup.CgroupManager{CgroupName: cgroupName}
	return cg.Freeze()
}

func (s *Service) ResumeTask(containerID string) error {
	cgroupName := constants.CgroupPrefix + utils.FormatShortID(containerID)
	cg := &cgroup.CgroupManager{CgroupName: cgroupName}
	return cg.Thaw()
}

func (s *Service) ExecTask(containerID string, args []string) error {
	conn, err := connectShim(containerID)
	if err != nil {
		return err
	}
	defer conn.Close()

	req := types.ShimRequest{Type: "exec", Args: args}
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return fmt.Errorf("发送请求失败: %w", err)
	}

	var resp types.ShimResponse
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		return fmt.Errorf("读取响应失败: %w", err)
	}
	if !resp.Success {
		return fmt.Errorf("%s", resp.Message)
	}

	if resp.Stream {
		return nil
	}

	buf := make([]byte, 32*1024)
	for {
		n, err := conn.Read(buf)
		if n > 0 {
			os.Stdout.Write(buf[:n])
		}
		if err != nil {
			break
		}
	}
	return nil
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

	return conn, nil
}

func (s *Service) ResizeTask(containerID string, rows, cols uint16) error {
	conn, err := connectShim(containerID)
	if err != nil {
		return err
	}
	defer conn.Close()

	req := types.ShimRequest{Type: "resize", Rows: rows, Cols: cols}
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return fmt.Errorf("发送 resize 请求失败: %w", err)
	}

	var resp types.ShimResponse
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		return fmt.Errorf("读取 resize 响应失败: %w", err)
	}
	if !resp.Success {
		return fmt.Errorf("resize 失败: %s", resp.Message)
	}
	return nil
}

func (s *Service) ListTasks() ([]spec.State, error) {
	entries, err := os.ReadDir(runtimeDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var states []spec.State
	for _, entry := range entries {
		stateDir := filepath.Join(runtimeDir, entry.Name())
		state, err := spec.LoadState(stateDir)
		if err != nil {
			continue
		}
		if state.Status == spec.StatusRunning || state.Status == spec.StatusCreated {
			proc, err := os.FindProcess(state.Pid)
			if err != nil || proc.Signal(syscall.Signal(0)) != nil {
				state.Status = spec.StatusStopped
			}
		}
		states = append(states, *state)
	}
	return states, nil
}

type ExitInfo = types.ExitInfo

func resolveShimDir(containerID string) string {
	directPath := filepath.Join(shimDir, containerID)
	if _, err := os.Stat(directPath); err == nil {
		return directPath
	}
	entries, err := os.ReadDir(shimDir)
	if err != nil {
		return directPath
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), containerID) {
			return filepath.Join(shimDir, entry.Name())
		}
	}
	return directPath
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

func buildOCISpec(config *TaskConfig) *spec.Spec {
	s := spec.DefaultSpec(&spec.RunConfig{
		Tty:           config.Tty,
		Memory:        config.Memory,
		CPUShares:     config.CPUShares,
		Image:         config.Image,
		ImageRootFS:   config.RootFSPath,
		Cmd:           config.Cmd,
		Volumes:       config.Volumes,
		Hostname:      config.Hostname,
		Network:       config.Network,
		RestartPolicy: config.RestartPolicy,
	})

	if config.Overlay != nil {
		if s.Annotations == nil {
			s.Annotations = make(map[string]string)
		}
		s.Annotations["mini-docker.overlay.merged"] = config.Overlay.Merged
		s.Annotations["mini-docker.overlay.upper"] = config.Overlay.Upper
		s.Annotations["mini-docker.overlay.work"] = config.Overlay.Work
	}

	if config.PortMap != "" {
		if s.Annotations == nil {
			s.Annotations = make(map[string]string)
		}
		s.Annotations["mini-docker.port-map"] = config.PortMap
	}

	return s
}

func GetContainerSpec(containerID string) *spec.Spec {
	stateDir := filepath.Join(runtimeDir, containerID)
	s, err := spec.LoadState(stateDir)
	if err != nil {
		return nil
	}
	configPath := filepath.Join(s.Bundle, "config.json")
	data, err := os.ReadFile(configPath)
	if err != nil {
		return nil
	}
	var ociSpec spec.Spec
	if err := json.Unmarshal(data, &ociSpec); err != nil {
		return nil
	}
	return &ociSpec
}

func GetContainerImage(containerID string) string {
	ociSpec := GetContainerSpec(containerID)
	if ociSpec == nil || ociSpec.Annotations == nil {
		return ""
	}
	return ociSpec.Annotations["mini-docker.image"]
}

func GetContainerCmd(containerID string) []string {
	ociSpec := GetContainerSpec(containerID)
	if ociSpec == nil || ociSpec.Process == nil {
		return nil
	}
	return ociSpec.Process.Args
}

func GetContainerRestartPolicy(containerID string) string {
	ociSpec := GetContainerSpec(containerID)
	if ociSpec == nil || ociSpec.Annotations == nil {
		return "no"
	}
	if policy, ok := ociSpec.Annotations["mini-docker.restart-policy"]; ok {
		return policy
	}
	return "no"
}

func IsShimAlive(containerID string) bool {
	socketPath := filepath.Join(shimDir, containerID, "shim.sock")
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
