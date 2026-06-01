//go:build linux

package shim

import (
	"encoding/json"
	"fmt"
	"os/exec"
)

// getContainerPIDFromRuntime 通过 runtime state 命令获取容器 PID
// 对齐 Docker/containerd: shim 通过 runtime 接口获取容器状态，而非直接访问 libcontainer
func getContainerPIDFromRuntime(containerID string) (int, error) {
	cmd := exec.Command("/proc/self/exe", "runtime", "state", containerID)
	output, err := cmd.Output()
	if err != nil {
		return 0, fmt.Errorf("runtime state 执行失败: %w", err)
	}

	var state struct {
		Pid int `json:"pid"`
	}
	if err := json.Unmarshal(output, &state); err != nil {
		return 0, fmt.Errorf("解析 runtime state 输出失败: %w", err)
	}
	return state.Pid, nil
}

// getContainerStateViaRuntime 通过 runtime state 命令获取容器状态
// 对齐 Docker/containerd: shim 通过 runtime 接口获取容器状态，而非直接访问 libcontainer
func getContainerStateViaRuntime(containerID string) (map[string]interface{}, error) {
	cmd := exec.Command("/proc/self/exe", "runtime", "state", containerID)
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("runtime state 执行失败: %w", err)
	}

	var state map[string]interface{}
	if err := json.Unmarshal(output, &state); err != nil {
		return nil, fmt.Errorf("解析 runtime state 输出失败: %w", err)
	}
	return state, nil
}

// runRuntimeCommand 执行 runtime 子命令（pause/resume 等）
// 对齐 Docker/containerd: shim 通过调用 runtime 命令操作容器，而非直接访问 libcontainer
func runRuntimeCommand(subCmd, containerID string) error {
	cmd := exec.Command("/proc/self/exe", "runtime", subCmd, containerID)
	cmd.Stdout = exec.Command("/proc/self/exe").Stdout
	cmd.Stderr = exec.Command("/proc/self/exe").Stderr
	return cmd.Run()
}

// runRuntimeCommandWithSignal 执行带信号的 runtime 子命令（kill）
// 对齐 Docker/containerd: shim 通过调用 runtime kill 发送信号，而非直接调用系统 kill
func runRuntimeCommandWithSignal(subCmd, containerID string, signal int) error {
	cmd := exec.Command("/proc/self/exe", "runtime", subCmd, containerID, fmt.Sprintf("%d", signal))
	cmd.Stdout = exec.Command("/proc/self/exe").Stdout
	cmd.Stderr = exec.Command("/proc/self/exe").Stderr
	return cmd.Run()
}
