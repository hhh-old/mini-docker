//go:build linux

package shim

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"mini-docker/libcontainer"
	"mini-docker/spec"

	"golang.org/x/sys/unix"
)

const (
	shimDir = "/var/run/mini-docker/shim"
)

type ExitInfo struct {
	ExitCode int    `json:"exit_code"`
	ExitedAt string `json:"exited_at"`
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

// Run shim 进程主入口（对标 containerd-shim）
// 参数: containerID, bundlePath, configJSON
// 流程:
//  1. 设置 PR_SET_CHILD_SUBREAPER（收养孤儿进程）
//  2. 创建控制 socket
//  3. 调用 runtime create（exec 子进程，等待退出）
//  4. 调用 runtime start（exec 子进程，等待退出）
//  5. 等待容器进程退出（回收僵尸）
//  6. 保存退出信息
//  7. 保持运行，提供控制 socket 服务
func Run(args []string) {
	if len(args) < 3 {
		fmt.Fprintf(os.Stderr, "用法: mini-docker shim <containerID> <bundlePath> <configJSON>\n")
		os.Exit(1)
	}

	containerID := args[0]
	bundlePath := args[1]
	configJSON := args[2]

	if err := run(containerID, bundlePath, configJSON); err != nil {
		fmt.Fprintf(os.Stderr, "shim 错误: %v\n", err)
		os.Exit(1)
	}
}

func run(containerID, bundlePath, configJSON string) error {
	// 1. 设置 PR_SET_CHILD_SUBREAPER
	//    当 runtime (create/start) 退出时，init 进程被 reparent 到此 shim
	if err := unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0); err != nil {
		return fmt.Errorf("设置 PR_SET_CHILD_SUBREAPER 失败: %w", err)
	}

	// 2. 创建 shim 目录和控制 socket
	shimContainerDir := filepath.Join(shimDir, containerID)
	if err := os.MkdirAll(shimContainerDir, 0755); err != nil {
		return fmt.Errorf("创建 shim 目录失败: %w", err)
	}

	socketPath := filepath.Join(shimContainerDir, "shim.sock")
	os.Remove(socketPath)
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		return fmt.Errorf("创建控制 socket 失败: %w", err)
	}
	defer listener.Close()

	// 3. 写 shim PID 文件
	pidPath := filepath.Join(shimContainerDir, "shim.pid")
	os.WriteFile(pidPath, []byte(fmt.Sprintf("%d", os.Getpid())), 0644)

	// 4. 写 config.json 到 bundle 目录
	configPath := filepath.Join(bundlePath, "config.json")
	if err := os.WriteFile(configPath, []byte(configJSON), 0644); err != nil {
		return fmt.Errorf("写入 config.json 失败: %w", err)
	}

	// 5. 提前启动 socket 服务（daemon 在 waitForSocket 后会立即连接查询状态）
	//    此时 containerPID 尚未确定，使用 0 作为占位，后续更新
	var containerPID int
	var exitOnce sync.Once
	exitInfo := &ExitInfo{}
	exitReady := make(chan struct{})

	go serveControlSocket(listener, containerID, &containerPID, exitReady, exitInfo)

	// 6. 调用 runtime create（阻塞等待其退出）
	log.Printf("[shim] 容器 %s: 调用 runtime create\n", containerID[:12])
	createCmd := exec.Command("/proc/self/exe", "runtime", "create", containerID, "--bundle", bundlePath)
	createCmd.Stdout = os.Stdout
	createCmd.Stderr = os.Stderr
	if err := createCmd.Run(); err != nil {
		return fmt.Errorf("runtime create 失败: %w", err)
	}
	// runtime create 已退出，init 进程已被此 shim 收养（SUBREAPER）

	// 7. 获取容器 PID（通过 libcontainer 加载容器状态）
	c, err := libcontainer.Load(containerID)
	if err != nil {
		return fmt.Errorf("加载容器失败: %w", err)
	}
	containerPID = c.Pid()
	log.Printf("[shim] 容器 %s: PID=%d, 状态=created\n", containerID[:12], containerPID)

	// 8. 调用 runtime start（阻塞等待其退出）
	log.Printf("[shim] 容器 %s: 调用 runtime start\n", containerID[:12])
	startCmd := exec.Command("/proc/self/exe", "runtime", "start", containerID)
	startCmd.Stdout = os.Stdout
	startCmd.Stderr = os.Stderr
	if err := startCmd.Run(); err != nil {
		return fmt.Errorf("runtime start 失败: %w", err)
	}
	// runtime start 已退出，init 进程已 exec 为用户进程
	log.Printf("[shim] 容器 %s: 用户进程已启动 (PID=%d)\n", containerID[:12], containerPID)

	// 9. 异步等待容器进程退出
	go func() {
		exitOnce.Do(func() {
			exitCode := waitForContainerExit(containerPID)
			exitInfo.ExitCode = exitCode
			exitInfo.ExitedAt = time.Now().Format(time.RFC3339)

			exitData, _ := json.Marshal(exitInfo)
			exitPath := filepath.Join(shimContainerDir, "exit.json")
			os.WriteFile(exitPath, exitData, 0644)
			close(exitReady)

			log.Printf("[shim] 容器 %s: 已退出 (exit_code=%d)\n", containerID[:12], exitCode)
		})
	}()

	// 10. 阻塞等待退出信号（socket 服务已在 goroutine 中运行）
	<-exitReady
	return nil
}

func waitForContainerExit(pid int) int {
	for {
		var status syscall.WaitStatus
		wpid, err := syscall.Wait4(pid, &status, 0, nil)
		if err != nil {
			if err == syscall.ECHILD {
				if checkProcessAlive(pid) != nil {
					return 0
				}
				time.Sleep(100 * time.Millisecond)
				continue
			}
			return 1
		}
		if wpid == pid {
			if status.Exited() {
				return status.ExitStatus()
			}
			if status.Signaled() {
				return 128 + int(status.Signal())
			}
			return 1
		}
	}
}

func checkProcessAlive(pid int) error {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return proc.Signal(syscall.Signal(0))
}

func serveControlSocket(listener net.Listener, containerID string, containerPID *int, exitReady <-chan struct{}, exitInfo *ExitInfo) {
	for {
		conn, err := listener.Accept()
		if err != nil {
			select {
			case <-exitReady:
				// 容器已退出，继续监听直到 shutdown
			default:
			}
			continue
		}
		go handleShimConn(conn, containerID, *containerPID, exitReady, exitInfo)
	}
}

func handleShimConn(conn net.Conn, containerID string, containerPID int, exitReady <-chan struct{}, exitInfo *ExitInfo) {
	defer conn.Close()

	var req ShimRequest
	if err := json.NewDecoder(conn).Decode(&req); err != nil {
		json.NewEncoder(conn).Encode(ShimResponse{Success: false, Message: err.Error()})
		return
	}

	switch req.Type {
	case "state":
		c, err := libcontainer.Load(containerID)
		if err != nil {
			json.NewEncoder(conn).Encode(ShimResponse{Success: false, Message: err.Error()})
			return
		}
		state := &spec.State{
			ID:     containerID,
			Status: spec.StatusRunning,
			Pid:    c.Pid(),
		}
		if checkProcessAlive(state.Pid) != nil {
			state.Status = spec.StatusStopped
		}
		json.NewEncoder(conn).Encode(ShimResponse{Success: true, Data: state})

	case "kill":
		sig := syscall.Signal(req.Signal)
		if err := unix.Kill(containerPID, sig); err != nil {
			json.NewEncoder(conn).Encode(ShimResponse{Success: false, Message: err.Error()})
			return
		}
		json.NewEncoder(conn).Encode(ShimResponse{Success: true})

	case "exit_info":
		select {
		case <-exitReady:
			json.NewEncoder(conn).Encode(ShimResponse{Success: true, Data: exitInfo})
		case <-time.After(5 * time.Second):
			json.NewEncoder(conn).Encode(ShimResponse{Success: false, Message: "容器尚未退出"})
		}

	case "exec":
		nsenterCmd := exec.Command("nsenter",
			"-t", fmt.Sprintf("%d", containerPID),
			"-m", "-u", "-i", "-n", "-p", "--")
		nsenterCmd.Args = append(nsenterCmd.Args, req.Args...)
		nsenterCmd.Stdin = conn
		nsenterCmd.Stdout = conn
		nsenterCmd.Stderr = conn
		nsenterCmd.Run()
		json.NewEncoder(conn).Encode(ShimResponse{Success: true})

	case "shutdown":
		json.NewEncoder(conn).Encode(ShimResponse{Success: true})
		os.Exit(0)

	default:
		json.NewEncoder(conn).Encode(ShimResponse{Success: false, Message: fmt.Sprintf("未知请求: %s", req.Type)})
	}
}
