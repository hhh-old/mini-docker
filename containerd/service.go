package containerd

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"mini-docker/spec"
)

const (
	runtimeDir = "/var/lib/mini-docker/runtime"
	shimDir    = "/var/run/mini-docker/shim"
)

type Service struct{}

func NewService() *Service {
	return &Service{}
}

type TaskConfig struct {
	ID            string
	BundlePath    string
	OCIConfigJSON string
	Image         string
	Hostname      string
	Cmd           []string
	Tty           bool
	Memory        string
	CpuShares     string
	Network       string
	PortMap       string
	RestartPolicy string
	Volumes       []string
	RootFSPath    string
	Overlay       *OverlayConfig
}

type OverlayConfig struct {
	Merged string
	Upper  string
	Work   string
}

type ShimRequest struct {
	Type   string   `json:"type"`
	Signal int      `json:"signal,omitempty"`
	Args   []string `json:"args,omitempty"`
}

type ShimResponse struct {
	Success bool        `json:"success"`
	Message string      `json:"message,omitempty"`
	Data    interface{} `json:"data,omitempty"`
}

// CreateTask 创建容器任务
// 1. 准备 OCI bundle（生成 config.json + overlay 目录）
// 2. fork shim 进程（shim 内部调用 runtime create + start）
func (s *Service) CreateTask(config *TaskConfig) (shimPID int, err error) {
	if config.ID == "" {
		return 0, fmt.Errorf("容器 ID 不能为空")
	}

	bundlePath := config.BundlePath //似乎没必要写在字段里面
	if bundlePath == "" {
		bundlePath = filepath.Join(runtimeDir, config.ID, "bundle")
	}
	if err := os.MkdirAll(bundlePath, 0755); err != nil {
		return 0, fmt.Errorf("创建 bundle 目录失败: %w", err)
	}

	ociSpec := buildOCISpec(config)
	specJSON, err := json.Marshal(ociSpec)
	if err != nil {
		return 0, fmt.Errorf("序列化 OCI Spec 失败: %w", err)
	}

	cmd := exec.Command("/proc/self/exe",
		"shim", config.ID, bundlePath, string(specJSON))
	cmd.SysProcAttr = newShimSysProcAttr()
	logDir := "/var/log/mini-docker/shim"
	os.MkdirAll(logDir, 0755)
	logPath := filepath.Join(logDir, config.ID+".log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err == nil {
		cmd.Stdout = logFile
		cmd.Stderr = logFile
	}

	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("启动 shim 失败: %w", err)
	}

	shimPID = cmd.Process.Pid

	socketPath := filepath.Join(shimDir, config.ID, "shim.sock")
	if err := waitForSocket(socketPath, 15*time.Second); err != nil {
		return 0, fmt.Errorf("shim socket 未就绪: %w", err)
	}

	return shimPID, nil
}

// KillTask 通过 shim 控制 socket 发送信号
func (s *Service) KillTask(containerID string, signal syscall.Signal) error {
	conn, err := connectShim(containerID)
	if err != nil {
		return err
	}
	defer conn.Close()

	req := ShimRequest{Type: "kill", Signal: int(signal)}
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return fmt.Errorf("发送请求失败: %w", err)
	}

	var resp ShimResponse
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		return fmt.Errorf("读取响应失败: %w", err)
	}
	if !resp.Success {
		return fmt.Errorf("kill 失败: %s", resp.Message)
	}
	return nil
}

// GetTaskState 通过 shim 查询容器状态
func (s *Service) GetTaskState(containerID string) (*spec.State, error) {
	conn, err := connectShim(containerID)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	req := ShimRequest{Type: "state"}
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return nil, fmt.Errorf("发送请求失败: %w", err)
	}

	var resp ShimResponse
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

// GetExitInfo 获取容器退出信息
func (s *Service) GetExitInfo(containerID string) (*ExitInfo, error) {
	conn, err := connectShim(containerID)
	if err != nil {
		return readExitInfoFromFile(containerID)
	}
	defer conn.Close()

	req := ShimRequest{Type: "exit_info"}
	json.NewEncoder(conn).Encode(req)

	var resp ShimResponse
	json.NewDecoder(conn).Decode(&resp)
	if !resp.Success {
		return nil, fmt.Errorf("%s", resp.Message)
	}

	data, _ := json.Marshal(resp.Data)
	var info ExitInfo
	json.Unmarshal(data, &info)
	return &info, nil
}

// DeleteTask 删除容器任务
func (s *Service) DeleteTask(containerID string) error {
	conn, err := connectShim(containerID)
	if err == nil {
		json.NewEncoder(conn).Encode(ShimRequest{Type: "shutdown"})
		conn.Close()
		time.Sleep(500 * time.Millisecond)
	}

	stateDir := filepath.Join(runtimeDir, containerID)
	os.RemoveAll(stateDir)

	shimContainerDir := filepath.Join(shimDir, containerID)
	os.RemoveAll(shimContainerDir)

	return nil
}

// PauseTask 暂停容器（通过 cgroup freezer）
func (s *Service) PauseTask(containerID string) error {
	state, err := s.GetTaskState(containerID)
	if err != nil {
		return err
	}
	if state.Status != spec.StatusRunning {
		return fmt.Errorf("容器未在运行")
	}

	cgroupName := "mini-docker-" + containerID[:12]
	cgroupPath := filepath.Join("/sys/fs/cgroup", cgroupName)
	os.MkdirAll(cgroupPath, 0755)
	os.WriteFile(filepath.Join(cgroupPath, "cgroup.procs"),
		[]byte(fmt.Sprintf("%d", state.Pid)), 0644)
	return os.WriteFile(filepath.Join(cgroupPath, "cgroup.freeze"), []byte("1"), 0644)
}

// ResumeTask 恢复容器
func (s *Service) ResumeTask(containerID string) error {
	cgroupName := "mini-docker-" + containerID[:12]
	cgroupPath := filepath.Join("/sys/fs/cgroup", cgroupName)
	return os.WriteFile(filepath.Join(cgroupPath, "cgroup.freeze"), []byte("0"), 0644)
}

// ExecTask 在容器内执行命令
func (s *Service) ExecTask(containerID string, args []string) error {
	conn, err := connectShim(containerID)
	if err != nil {
		return err
	}
	defer conn.Close()

	req := ShimRequest{Type: "exec", Args: args}
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return fmt.Errorf("发送请求失败: %w", err)
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

// ListTasks 列出所有任务
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

type ExitInfo struct {
	ExitCode int    `json:"exit_code"`
	ExitedAt string `json:"exited_at"`
}

func connectShim(containerID string) (net.Conn, error) {
	socketPath := filepath.Join(shimDir, containerID, "shim.sock")
	conn, err := net.DialTimeout("unix", socketPath, 5*time.Second)
	if err != nil {
		return nil, fmt.Errorf("连接 shim 失败: %w", err)
	}
	return conn, nil
}

func readExitInfoFromFile(containerID string) (*ExitInfo, error) {
	exitPath := filepath.Join(shimDir, containerID, "exit.json")
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
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("unix", path, 500*time.Millisecond)
		if err == nil {
			conn.Close()
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("等待 socket %s 超时", path)
}

func buildOCISpec(config *TaskConfig) *spec.Spec {
	s := spec.DefaultSpec(&spec.RunConfig{
		Tty:           config.Tty,
		Memory:        config.Memory,
		CpuShares:     config.CpuShares,
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

func GetContainerImage(containerID string) string {
	stateDir := filepath.Join(runtimeDir, containerID)
	s, err := spec.LoadState(stateDir)
	if err != nil {
		return ""
	}
	configPath := filepath.Join(s.Bundle, "config.json")
	data, err := os.ReadFile(configPath)
	if err != nil {
		return ""
	}
	var ociSpec spec.Spec
	if err := json.Unmarshal(data, &ociSpec); err != nil {
		return ""
	}
	if ociSpec.Annotations != nil {
		return ociSpec.Annotations["mini-docker.image"]
	}
	return ""
}

func GetContainerCmd(containerID string) []string {
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
	if ociSpec.Process != nil {
		return ociSpec.Process.Args
	}
	return nil
}

func GetContainerRestartPolicy(containerID string) string {
	stateDir := filepath.Join(runtimeDir, containerID)
	s, err := spec.LoadState(stateDir)
	if err != nil {
		return "no"
	}
	configPath := filepath.Join(s.Bundle, "config.json")
	data, err := os.ReadFile(configPath)
	if err != nil {
		return "no"
	}
	var ociSpec spec.Spec
	if err := json.Unmarshal(data, &ociSpec); err != nil {
		return "no"
	}
	if ociSpec.Annotations != nil {
		if policy, ok := ociSpec.Annotations["mini-docker.restart-policy"]; ok {
			return policy
		}
	}
	return "no"
}

func FormatShortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

func IsShimAlive(containerID string) bool {
	socketPath := filepath.Join(shimDir, containerID, "shim.sock")
	conn, err := net.DialTimeout("unix", socketPath, 2*time.Second)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

func ReadShimPID(containerID string) int {
	pidPath := filepath.Join(shimDir, containerID, "shim.pid")
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

func ParseVolumeList(volsStr string) []string {
	if volsStr == "" {
		return nil
	}
	return strings.Split(volsStr, "|")
}
