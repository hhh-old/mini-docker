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

// 存储位置 ： /var/lib/mini-docker/runtime/<containerID>/state.json
// 对齐 Docker: runc 的 state.json 是运行时状态的唯一来源
// 记录容器进程的 PID、运行状态（created/running/stopped）、bundle 路径 等纯运行时信息。这是容器运行时状态的"唯一来源"
// 对标 OCI runtime-spec State 定义：ociVersion/id/status/pid/bundle/annotations 为 REQUIRED 字段
// 由于像 runc 这样的 OCI 运行时在宿主机上是无状态的命令行工具（它不是一个一直运行的守护进程，执行完 runc create 后进程就退出了，linuxContainer对象也消亡了），为了让后续的 runc start、runc kill、runc delete 依然能找到这个容器，它必须在宿主机上找个地方把容器的动态运行状态存起来
type ContainerState struct {
	OCIVersion  string            `json:"ociVersion"`
	ID          string            `json:"id"`
	Pid         int               `json:"pid"`
	Bundle      string            `json:"bundle"`
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

// saveContainerState 保存容器状态（原子写入，对标 runc saveState）
// 使用临时文件 + os.Rename 实现原子写入，避免写入过程中崩溃导致 state.json 损坏
// os.Rename 在同一文件系统上是内核保证的原子操作，不存在中间状态
func saveContainerState(state *ContainerState) (retErr error) {
	dir := getContainerDir(state.ID)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("创建容器目录失败: %w", err)
	}

	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化状态失败: %w", err)
	}

	tmpFile, err := os.CreateTemp(dir, "state-")
	if err != nil {
		return fmt.Errorf("创建临时文件失败: %w", err)
	}
	tmpName := tmpFile.Name()

	defer func() {
		if retErr != nil {
			tmpFile.Close()
			os.Remove(tmpName)
		}
	}()

	if _, err := tmpFile.Write(data); err != nil {
		return fmt.Errorf("写入临时文件失败: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		return fmt.Errorf("关闭临时文件失败: %w", err)
	}

	return os.Rename(tmpName, getStatePath(state.ID))
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
