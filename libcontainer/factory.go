package libcontainer

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

const (
	// StateDir 容器状态存储目录（与 containerd service 的 runtimeDir 保持一致）
	StateDir = "/var/lib/mini-docker/runtime"

	// InitPipeFd init 进程的 ready 信号 pipe fd
	InitPipeFd = 3
)

// ContainerState 容器持久化状态
type ContainerState struct {
	ID         string `json:"id"`
	Pid        int    `json:"pid"`
	BundlePath string `json:"bundle_path"`
	Rootfs     string `json:"rootfs"`
	Status     Status `json:"status"`
}

// getContainerDir 获取容器状态目录
func getContainerDir(id string) string {
	return filepath.Join(StateDir, id)
}

// getStatePath 获取容器状态文件路径
func getStatePath(id string) string {
	return filepath.Join(getContainerDir(id), "state.json")
}

// saveContainerState 保存容器状态
func saveContainerState(state *ContainerState) error {
	dir := getContainerDir(state.ID)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("创建容器目录失败: %w", err)
	}

	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化状态失败: %w", err)
	}

	return os.WriteFile(getStatePath(state.ID), data, 0644)
}

// loadContainerState 加载容器状态
func loadContainerState(id string) (*ContainerState, error) {
	data, err := os.ReadFile(getStatePath(id))
	if err != nil {
		return nil, fmt.Errorf("读取容器状态失败: %w", err)
	}

	var state ContainerState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("解析容器状态失败: %w", err)
	}

	return &state, nil
}

// removeContainerState 删除容器状态
func removeContainerState(id string) error {
	return os.RemoveAll(getContainerDir(id))
}

// listContainerIDs 列出所有容器 ID
func listContainerIDs() ([]string, error) {
	entries, err := os.ReadDir(StateDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var ids []string
	for _, entry := range entries {
		if entry.IsDir() {
			statePath := filepath.Join(StateDir, entry.Name(), "state.json")
			if _, err := os.Stat(statePath); err == nil {
				ids = append(ids, entry.Name())
			}
		}
	}

	return ids, nil
}
