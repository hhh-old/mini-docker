//go:build linux

package shim

/*
=======================================================================
  Shim Manager 服务 —— 对齐 containerd 的 shim 生命周期管理

  将 shim_manager_linux.go 中的包级函数封装为 ShimManager 结构体，
  使其成为插件系统中的一员，生命周期由 Plugin Manager 统一管理。

  对齐真实 containerd 架构：
  - Shim Manager 是 Task Service 的底层依赖
  - Task Service 通过 Shim Manager 管理所有 shim 进程
  - Shim Manager 负责 shim 的连接、通信、创建、删除、重启

=======================================================================
*/

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
	"mini-docker/containerd/metadata"
	"mini-docker/containerstore"
	"mini-docker/libcontainer"
	"mini-docker/spec"
	"mini-docker/types"
	"mini-docker/utils"
)

// ExitInfo 退出信息类型别名
type ExitInfo = types.ExitInfo

// ShimManager 管理 shim 进程的生命周期
// 对齐 containerd: shim 管理是 Task Service 的底层依赖
type ShimManager struct {
	metaDB *metadata.DB
}

// NewShimManager 创建 ShimManager 实例
func NewShimManager(metaDB *metadata.DB) *ShimManager {
	return &ShimManager{metaDB: metaDB}
}

// resolveShimDir 解析指定容器的 shim 工作目录路径
func (m *ShimManager) resolveShimDir(containerID string) string {
	return filepath.Join(constants.ShimDir, containerID)
}

// Connect 通过 Unix socket 连接指定容器的 shim 进程
func (m *ShimManager) Connect(containerID string) (net.Conn, error) {
	shimContainerDir := m.resolveShimDir(containerID)
	socketPath := filepath.Join(shimContainerDir, "shim.sock")
	conn, err := net.DialTimeout("unix", socketPath, 5*time.Second)
	if err != nil {
		return nil, fmt.Errorf("连接 shim 失败: %w", err)
	}
	return conn, nil
}

// Call 向 shim 发送请求并等待响应（不关心返回数据）
func (m *ShimManager) Call(containerID string, req types.ShimRequest) error {
	conn, err := m.Connect(containerID)
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

// CallWithData 向 shim 发送请求并将响应数据反序列化到 result
func (m *ShimManager) CallWithData(containerID string, req types.ShimRequest, result any) error {
	conn, err := m.Connect(containerID)
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

// ReadExitInfoFromFile 读取 shim 退出时持久化的 exit.json 文件
func (m *ShimManager) ReadExitInfoFromFile(containerID string) (*ExitInfo, error) {
	shimContainerDir := m.resolveShimDir(containerID)
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

// WaitForSocket 轮询等待 Unix socket 文件可连接，超时返回错误
func (m *ShimManager) WaitForSocket(path string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("unix", path, constants.ShimConnectTimeout)
		if err == nil {
			conn.Close()
			return nil
		}
		time.Sleep(constants.PollInterval)
	}
	return fmt.Errorf("等待 socket %s 超时", path)
}

// BuildOCISpec 根据容器存储信息生成 OCI Runtime Spec
func (m *ShimManager) BuildOCISpec(info *containerstore.ContainerInfo, cgroupName string) *spec.Spec {
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
		PortMap:       info.PortMap,
		CgroupName:    cgroupName,
	})
}

// IsAlive 通过尝试连接 socket 判断 shim 进程是否还活着
func (m *ShimManager) IsAlive(containerID string) bool {
	shimContainerDir := m.resolveShimDir(containerID)
	socketPath := filepath.Join(shimContainerDir, "shim.sock")

	// 先校验 shim 进程是否存活：socket 文件残留但进程已死时不能误判为存活
	if shimPID := m.ReadPID(containerID); shimPID > 0 {
		proc, err := os.FindProcess(shimPID)
		if err != nil || proc.Signal(syscall.Signal(0)) != nil {
			return false
		}
	}

	conn, err := net.DialTimeout("unix", socketPath, constants.ShimConnectTimeout)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// ReadPID 从 shim.pid 文件中读取 shim 进程 PID，失败返回 0
func (m *ShimManager) ReadPID(containerID string) int {
	pidPath := filepath.Join(m.resolveShimDir(containerID), "shim.pid")
	data, err := os.ReadFile(pidPath)
	if err != nil {
		return 0
	}
	var pid int
	fmt.Sscanf(strings.TrimSpace(string(data)), "%d", &pid)
	return pid
}

// Delete 删除容器任务：停止 shim 和容器进程，清理运行时目录
func (m *ShimManager) Delete(containerID string) error {
	conn, err := m.Connect(containerID)
	if err == nil {
		json.NewEncoder(conn).Encode(types.ShimRequest{Type: "shutdown"})
		conn.Close()
		shimPID := m.ReadPID(containerID)
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
		shimPID := m.ReadPID(containerID)
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
	if info, err := m.ReadExitInfoFromFile(containerID); err != nil || info == nil {
		createdPath := filepath.Join(m.resolveShimDir(containerID), "created")
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

	stateDir := filepath.Join(constants.RuntimeDir, containerID)
	os.RemoveAll(stateDir)

	shimContainerDir := m.resolveShimDir(containerID)
	os.RemoveAll(shimContainerDir)

	return nil
}

// Shutdown 向 shim 发送 shutdown 请求并等待其退出
func (m *ShimManager) Shutdown(containerID string) {
	conn, err := m.Connect(containerID)
	if err != nil {
		return
	}
	json.NewEncoder(conn).Encode(types.ShimRequest{Type: "shutdown"})
	conn.Close()
	time.Sleep(constants.ShutdownWaitTime)
}

// Restart 重启 shim 并以 --takeover 接管已存在的容器进程，返回新 shim 的 PID
func (m *ShimManager) Restart(containerID string, containerPID int) (int, error) {
	bundlePath := filepath.Join(constants.RuntimeDir, containerID, "bundle")
	if _, err := os.Stat(bundlePath); err != nil {
		return 0, fmt.Errorf("bundle 目录不存在: %w", err)
	}

	shimContainerDir := m.resolveShimDir(containerID)
	os.Remove(filepath.Join(shimContainerDir, "shim.sock"))
	os.Remove(filepath.Join(shimContainerDir, "shim.pid"))
	os.Remove(filepath.Join(shimContainerDir, "created"))
	os.Remove(filepath.Join(shimContainerDir, "exit.json"))

	cmd := m.NewCommand([]string{"shim", containerID, bundlePath, "--takeover", fmt.Sprintf("%d", containerPID)})

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

	socketPath := filepath.Join(shimContainerDir, "shim.sock")
	if err := m.WaitForSocket(socketPath, constants.SocketWaitTimeout); err != nil {
		cmd.Process.Kill()
		cmd.Wait()
		return 0, fmt.Errorf("shim socket 未就绪: %w", err)
	}

	return shimPID, nil
}

// WaitForCreate 轮询等待 shim 创建容器完成，从 created 文件读取容器 PID
func (m *ShimManager) WaitForCreate(containerID string, timeout time.Duration) (int, error) {
	shimContainerDir := m.resolveShimDir(containerID)
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

// Attach 向 shim 发送 attach 请求，建立 I/O 透传的连接
func (m *ShimManager) Attach(containerID string) (net.Conn, error) {
	conn, err := m.Connect(containerID)
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

// ExecStream 在运行中的容器内执行命令，返回与 shim 通信的连接
func (m *ShimManager) ExecStream(containerID string, args []string, tty bool) (net.Conn, error) {
	conn, err := m.Connect(containerID)
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

// NewCommand 创建 shim 进程命令
// containerd 独立进程后，使用 /proc/self/exe 仍然可行（同一个二进制）
func (m *ShimManager) NewCommand(args []string) *exec.Cmd {
	cmd := exec.Command("/proc/self/exe", args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	return cmd
}

// GetState 获取容器运行时状态
// 对齐 containerd: 通过 shim 协议获取，不暴露 runtime 层类型给上层
// 在线：通过 shim socket 获取（shim State RPC 返回 container_id/pid/status）
// 离线：从 state.json 文件读取并转换
// ShimManager 是 containerd 与 runtime 的边界层，内部使用 libcontainer 是合理的
func (m *ShimManager) GetState(containerID string) (*metadata.TaskState, error) {
	// 在线：通过 shim socket 获取
	conn, err := m.Connect(containerID)
	if err == nil {
		conn.Close()
		var state metadata.TaskState
		if err := m.CallWithData(containerID, types.ShimRequest{Type: "state"}, &state); err == nil {
			state.ShimPID = m.ReadPID(containerID)
			return &state, nil
		}
	}

	// 离线 fallback：从 state.json 读取并转换
	cs, err := libcontainer.LoadContainerState(containerID)
	if err != nil {
		return nil, fmt.Errorf("获取任务状态失败: %w", err)
	}
	return &metadata.TaskState{
		ContainerID: cs.ID,
		PID:         cs.Pid,
		ShimPID:     m.ReadPID(containerID),
		Status:      cs.Status,
	}, nil
}

// ListStates 列出所有容器运行时状态
// 对齐 containerd: 通过扫描 state.json 获取，不暴露 runtime 层类型给上层
// 对状态为 Running/Created 的容器，额外检测进程是否真正存活，不存活则修正为 Stopped
// 原因：容器可能被 OOM kill 或外部信号杀死，state.json 未及时更新
func (m *ShimManager) ListStates() ([]*metadata.TaskState, error) {
	containerStates, err := libcontainer.ListContainerStates()
	if err != nil {
		return nil, fmt.Errorf("列出任务失败: %w", err)
	}

	states := make([]*metadata.TaskState, 0, len(containerStates))
	for _, cs := range containerStates {
		ts := &metadata.TaskState{
			ContainerID: cs.ID,
			PID:         cs.Pid,
			ShimPID:     m.ReadPID(cs.ID),
			Status:      cs.Status,
		}
		// 检查进程是否真正存活
		if ts.Status == containerstore.StatusRunning || ts.Status == containerstore.StatusCreated {
			proc, err := os.FindProcess(ts.PID)
			if err != nil || proc.Signal(syscall.Signal(0)) != nil {
				ts.Status = containerstore.StatusStopped
			}
		}
		states = append(states, ts)
	}
	return states, nil
}
