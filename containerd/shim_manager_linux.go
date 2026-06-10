//go:build linux

package containerd

/*
=======================================================================
  Shim 进程管理（对齐 Docker: containerd 对 shim 的生命周期管理）

  包含 shim 进程的连接、通信、创建、删除、重启等底层操作。
  这些函数被 Task 处理器调用，但逻辑上属于独立的 shim 管理领域。

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
	"mini-docker/containerstore"
	"mini-docker/spec"
	"mini-docker/types"
	"mini-docker/utils"
)

// ExitInfo 退出信息类型别名见 types.go

// resolveShimDir 解析指定容器的 shim 工作目录路径
func resolveShimDir(containerID string) string {
	return filepath.Join(constants.ShimDir, containerID)
}

// connectShim 通过 Unix socket 连接指定容器的 shim 进程
func connectShim(containerID string) (net.Conn, error) {
	shimContainerDir := resolveShimDir(containerID)
	socketPath := filepath.Join(shimContainerDir, "shim.sock")
	conn, err := net.DialTimeout("unix", socketPath, 5*time.Second)
	if err != nil {
		return nil, fmt.Errorf("连接 shim 失败: %w", err)
	}
	return conn, nil
}

// shimCall 向 shim 发送请求并等待响应（不关心返回数据）
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

// shimCallWithData 向 shim 发送请求并将响应数据反序列化到 result
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

// readExitInfoFromFile 读取 shim 退出时持久化的 exit.json 文件
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

// waitForSocket 轮询等待 Unix socket 文件可连接，超时返回错误
func waitForSocket(path string, timeout time.Duration) error {
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

// buildOCISpec 根据容器存储信息生成 OCI Runtime Spec
func buildOCISpec(info *containerstore.ContainerInfo) *spec.Spec {
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
		CgroupName:    info.CgroupName,
	})
}

// isShimAlive 通过尝试连接 socket 判断 shim 进程是否还活着
func isShimAlive(containerID string) bool {
	shimContainerDir := resolveShimDir(containerID)
	socketPath := filepath.Join(shimContainerDir, "shim.sock")
	conn, err := net.DialTimeout("unix", socketPath, constants.ShimConnectTimeout)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// readShimPID 从 shim.pid 文件中读取 shim 进程 PID，失败返回 0
func readShimPID(containerID string) int {
	pidPath := filepath.Join(resolveShimDir(containerID), "shim.pid")
	data, err := os.ReadFile(pidPath)
	if err != nil {
		return 0
	}
	var pid int
	fmt.Sscanf(strings.TrimSpace(string(data)), "%d", &pid)
	return pid
}

// deleteTask 删除容器任务：停止 shim 和容器进程，清理运行时目录
func deleteTask(containerID string) error {
	conn, err := connectShim(containerID)
	if err == nil {
		json.NewEncoder(conn).Encode(types.ShimRequest{Type: "shutdown"})
		conn.Close()
		shimPID := readShimPID(containerID)
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
		shimPID := readShimPID(containerID)
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

	stateDir := filepath.Join(constants.RuntimeDir, containerID)
	os.RemoveAll(stateDir)

	shimContainerDir := resolveShimDir(containerID)
	os.RemoveAll(shimContainerDir)

	return nil
}

// shutdownShim 向 shim 发送 shutdown 请求并等待其退出
func shutdownShim(containerID string) {
	conn, err := connectShim(containerID)
	if err != nil {
		return
	}
	json.NewEncoder(conn).Encode(types.ShimRequest{Type: "shutdown"})
	conn.Close()
	time.Sleep(constants.ShutdownWaitTime)
}

// restartShim 重启 shim 并以 --takeover 接管已存在的容器进程，返回新 shim 的 PID
func restartShim(containerID string, containerPID int) (int, error) {
	bundlePath := filepath.Join(constants.RuntimeDir, containerID, "bundle")
	if _, err := os.Stat(bundlePath); err != nil {
		return 0, fmt.Errorf("bundle 目录不存在: %w", err)
	}

	shimContainerDir := resolveShimDir(containerID)
	os.Remove(filepath.Join(shimContainerDir, "shim.sock"))
	os.Remove(filepath.Join(shimContainerDir, "shim.pid"))
	os.Remove(filepath.Join(shimContainerDir, "created"))
	os.Remove(filepath.Join(shimContainerDir, "exit.json"))

	cmd := newShimCommand([]string{"shim", containerID, bundlePath, "--takeover", fmt.Sprintf("%d", containerPID)})

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
	if err := waitForSocket(socketPath, constants.SocketWaitTimeout); err != nil {
		cmd.Process.Kill()
		cmd.Wait()
		return 0, fmt.Errorf("shim socket 未就绪: %w", err)
	}

	return shimPID, nil
}

// waitForCreate 轮询等待 shim 创建容器完成，从 created 文件读取容器 PID
func waitForCreate(containerID string, timeout time.Duration) (int, error) {
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

// attachToShim 向 shim 发送 attach 请求，建立 I/O 透传的连接
func attachToShim(containerID string) (net.Conn, error) {
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

// execTaskStream 在运行中的容器内执行命令，返回与 shim 通信的连接
func execTaskStream(containerID string, args []string, tty bool) (net.Conn, error) {
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

// newShimCommand 创建 shim 进程命令
// containerd 独立进程后，使用 /proc/self/exe 仍然可行（同一个二进制）
func newShimCommand(args []string) *exec.Cmd {
	cmd := exec.Command("/proc/self/exe", args...)
	cmd.SysProcAttr = newShimSysProcAttr()
	return cmd
}
