package metadata

import (
	"encoding/json"
	"fmt"

	bolt "go.etcd.io/bbolt"
)

// ContainerInfo 容器元数据（对齐 containerd containers.Container 概念）
// 这是 ContainerInfo 的唯一权威定义，containerstore 包通过类型别名引用
// containerstore.ContainerInfo = metadata.ContainerInfo，消除重复结构体和 JSON 桥接
//
// 对齐 containerd 架构原则：元数据与运行时状态分离
//   - ContainerInfo 只持久化创建时确定的静态配置、身份标识和 OCI 运行时类型
//   - 运行时状态（Pid/ShimPID/Status）由 TaskState 管理，仅存内存，始终从 shim 实时获取
//   - 网络/IP/cgroup/退出码/用户停止标记/重启计数等运行时/历史状态由 Daemon 自己维护，
//     存储在 containerstore.ContainerRuntimeState 中，不放在 ContainerInfo 里
type ContainerInfo struct {
	// ---- 身份标识 ----
	ID        string `json:"id"`
	Name      string `json:"name"`
	Image     string `json:"image"`
	CreatedAt string `json:"created_at"`

	// ---- 运行配置（创建时确定，不变） ----
	Cmd               []string `json:"cmd"`
	Tty               bool     `json:"tty"`
	RootFS            string   `json:"rootfs"`
	Network           string   `json:"network"`
	PortMap           string   `json:"port_map"`
	Volumes           []string `json:"volumes"`
	RestartPolicy     string   `json:"restart_policy"`
	MaxRestartRetries int      `json:"max_restart_retries"`
	Memory            string   `json:"memory"`
	CPUShares         string   `json:"cpu_shares"`

	// ---- 健康检查配置 ----
	HealthCmd      string `json:"health_cmd"`
	HealthInterval string `json:"health_interval"`
	HealthTimeout  string `json:"health_timeout"`
	HealthRetries  int    `json:"health_retries"`

	// ---- OCI Runtime 类型 ----
	Runtime string `json:"runtime"` // OCI Runtime 类型，默认 runc
}

// SaveContainer 保存容器元数据到 BucketContainers
func SaveContainer(tx *bolt.Tx, info *ContainerInfo) error {
	b := tx.Bucket(BucketContainers)
	if b == nil {
		return fmt.Errorf("containers bucket 不存在")
	}
	data, err := json.Marshal(info)
	if err != nil {
		return fmt.Errorf("序列化容器元数据失败: %w", err)
	}
	return b.Put([]byte(info.ID), data)
}

// LoadContainer 按 ID 加载容器元数据
func LoadContainer(tx *bolt.Tx, id string) (*ContainerInfo, error) {
	b := tx.Bucket(BucketContainers)
	if b == nil {
		return nil, fmt.Errorf("containers bucket 不存在")
	}
	data := b.Get([]byte(id))
	if data == nil {
		return nil, fmt.Errorf("容器 %s 不存在", id)
	}
	var info ContainerInfo
	if err := json.Unmarshal(data, &info); err != nil {
		return nil, fmt.Errorf("反序列化容器元数据失败: %w", err)
	}
	return &info, nil
}

// DeleteContainer 按 ID 删除容器元数据
func DeleteContainer(tx *bolt.Tx, id string) error {
	b := tx.Bucket(BucketContainers)
	if b == nil {
		return fmt.Errorf("containers bucket 不存在")
	}
	return b.Delete([]byte(id))
}

// ListContainers 列出所有容器元数据
func ListContainers(tx *bolt.Tx) ([]*ContainerInfo, error) {
	var containers []*ContainerInfo
	err := WalkContainers(tx, func(info *ContainerInfo) error {
		containers = append(containers, info)
		return nil
	})
	return containers, err
}

// WalkContainers 遍历所有容器
func WalkContainers(tx *bolt.Tx, fn func(*ContainerInfo) error) error {
	b := tx.Bucket(BucketContainers)
	if b == nil {
		return fmt.Errorf("containers bucket 不存在")
	}
	c := b.Cursor()
	for k, v := c.First(); k != nil; k, v = c.Next() {
		var info ContainerInfo
		if err := json.Unmarshal(v, &info); err != nil {
			continue
		}
		if err := fn(&info); err != nil {
			return err
		}
	}
	return nil
}

// LoadContainerByName 按名称查找容器
func LoadContainerByName(tx *bolt.Tx, name string) (*ContainerInfo, error) {
	var found *ContainerInfo
	err := WalkContainers(tx, func(info *ContainerInfo) error {
		if info.Name == name {
			found = info
			return fmt.Errorf("found") // 用错误中断遍历
		}
		return nil
	})
	if found != nil {
		return found, nil
	}
	if err != nil && found == nil {
		// 遍历出错且未找到
		return nil, fmt.Errorf("容器 %s 不存在", name)
	}
	return nil, fmt.Errorf("容器 %s 不存在", name)
}
