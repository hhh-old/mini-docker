package libcontainer

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"mini-docker/constants"
)

const (
	StateDir = constants.RuntimeDir
)

// ContainerState 容器持久化状态（统一存储，对标 Docker/runc 的 state.json）
// 合并了原 libcontainer.ContainerState 和 spec.State 的职责，
// 消除两者写入同一 state.json 文件但结构不同导致的覆盖风险。
// 对齐 Docker: runc 的 state.json 是运行时状态的唯一来源
type ContainerState struct {
	OCIVersion  string            `json:"ociVersion"`
	ID          string            `json:"id"`
	Pid         int               `json:"pid"`
	BundlePath  string            `json:"bundle_path"`
	Rootfs      string            `json:"rootfs"`
	Status      Status            `json:"status"`
	Annotations map[string]string `json:"annotations,omitempty"`
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

// LoadContainerState 公开加载容器状态（供 containerd 等外部包使用）
func LoadContainerState(id string) (*ContainerState, error) {
	return loadContainerState(id)
}

// ListContainerStates 扫描 runtime 目录，加载所有容器的状态
// 对齐 Docker: containerd 通过扫描 runc 的 state.json 列出所有任务
func ListContainerStates() ([]*ContainerState, error) {
	entries, err := os.ReadDir(StateDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var states []*ContainerState
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		state, err := loadContainerState(entry.Name())
		if err != nil {
			continue
		}
		states = append(states, state)
	}
	return states, nil
}
