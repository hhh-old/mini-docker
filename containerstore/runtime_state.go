package containerstore

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// ContainerRuntimeState 是 Daemon 维护的容器运行时与历史状态。
// 对应 Docker 中 dockerd 自己维护的容器运行时状态，不持久化在 containerd 的 BoltDB 中。
type ContainerRuntimeState struct {
	VethHost      string `json:"veth_host"`      // 虚拟网卡主机端名称
	ContainerIP   string `json:"container_ip"`   // 容器 IP 地址
	OverlayMerged string `json:"overlay_merged"` // OverlayFS merged 目录路径
	CgroupName    string `json:"cgroup_name"`    // Cgroup 名称

	LastExitCode int    `json:"last_exit_code"` // 最近一次退出码
	LastExitedAt string `json:"last_exited_at"` // 最近一次退出时间

	UserStopped  bool `json:"user_stopped"`  // 用户是否手动停止该容器
	RestartCount int  `json:"restart_count"` // 当前连续重启次数
}

// RuntimeStatePath 返回容器运行时状态文件路径
func RuntimeStatePath(containerID string) string {
	return filepath.Join(containerDataDir, containerID, "runtime_state.json")
}

// SaveRuntimeState 保存容器运行时状态
func SaveRuntimeState(containerID string, state *ContainerRuntimeState) error {
	if state == nil {
		return fmt.Errorf("runtime state 不能为空")
	}
	path := RuntimeStatePath(containerID)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return fmt.Errorf("创建运行时状态目录失败: %w", err)
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化运行时状态失败: %w", err)
	}
	return os.WriteFile(path, data, 0644)
}

// LoadRuntimeState 加载容器运行时状态，不存在时返回 nil
func LoadRuntimeState(containerID string) (*ContainerRuntimeState, error) {
	path := RuntimeStatePath(containerID)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var state ContainerRuntimeState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("反序列化运行时状态失败: %w", err)
	}
	return &state, nil
}

// DeleteRuntimeState 删除容器运行时状态文件
func DeleteRuntimeState(containerID string) error {
	path := RuntimeStatePath(containerID)
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
