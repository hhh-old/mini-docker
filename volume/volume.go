package volume

/*
=======================================================================
  Volume 数据卷 —— 对齐 Docker 的数据持久化机制
=======================================================================

  Docker Volume 的核心问题：
  容器的文件系统基于 OverlayFS，容器退出后 upper 层（可写层）被删除，
  所有数据丢失。Volume 就是解决这个问题的。

  Docker Volume 的两种形式：
  ┌─────────────────────────────────────────────────────────────┐
  │  1. Bind Mount（绑定挂载）                                   │
  │     -v /host/path:/container/path                           │
  │     直接将宿主机目录挂载到容器中，性能最好                     │
  │                                                              │
  │  2. Named Volume（命名卷）                                   │
  │     -v mydata:/container/path                                │
  │     Docker 管理的存储卷，数据在 /var/lib/docker/volumes/     │
  └─────────────────────────────────────────────────────────────┘

  Docker Volume 存储结构：
  /var/lib/docker/volumes/
  └── <volume-name>/
      ├── _data/       ← 实际数据目录
      └── metadata     ← 卷元数据（可选，Docker 不一定使用）

  mini-docker 的实现：
  /var/lib/mini-docker/volumes/
  └── <volume-name>/
      ├── _data/          ← 实际数据目录
      └── metadata.json   ← 卷元数据

  底层原理：
  Volume 的本质就是 mount --bind：
  mount --bind /host/path /container/path

  bind mount 让内核将一个目录的 inode 指向另一个目录的 inode，
  两个路径访问的是同一块磁盘数据，零拷贝。

=======================================================================
*/

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const (
	volumeStorePath = "/var/lib/mini-docker/volumes"
)

// VolumeInfo 数据卷元数据
type VolumeInfo struct {
	Name      string `json:"name"`
	Driver    string `json:"driver"`     // "local"（目前只支持 local）
	MountPath string `json:"mount_path"` // 宿主机上的数据路径
	CreatedAt string `json:"created_at"`
}

// VolumeMount 表示一个 Volume 挂载点
type VolumeMount struct {
	Source      string // 源路径（宿主机路径 或 volume 名）
	Destination string // 容器内目标路径
	Type        string // "bind" 或 "volume"
	ReadOnly    bool   // 是否只读
}

// Create 创建命名数据卷
func Create(name string) (*VolumeInfo, error) {
	if name == "" {
		return nil, fmt.Errorf("卷名不能为空")
	}

	volPath := filepath.Join(volumeStorePath, name)
	dataPath := filepath.Join(volPath, "_data")

	// 检查是否已存在
	metadataPath := filepath.Join(volPath, "metadata.json")
	if _, err := os.Stat(metadataPath); err == nil {
		return nil, fmt.Errorf("卷 %s 已存在", name)
	}

	// 创建数据目录
	if err := os.MkdirAll(dataPath, 0755); err != nil {
		return nil, fmt.Errorf("创建卷目录失败: %w", err)
	}

	info := &VolumeInfo{
		Name:      name,
		Driver:    "local",
		MountPath: dataPath,
		CreatedAt: time.Now().Format("2006-01-02 15:04:05"),
	}

	// 保存元数据
	data, err := json.MarshalIndent(info, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("序列化卷元数据失败: %w", err)
	}

	if err := os.WriteFile(metadataPath, data, 0644); err != nil {
		return nil, fmt.Errorf("保存卷元数据失败: %w", err)
	}

	return info, nil
}

// List 列出所有数据卷
func List() ([]*VolumeInfo, error) {
	if err := os.MkdirAll(volumeStorePath, 0755); err != nil {
		return nil, err
	}

	entries, err := os.ReadDir(volumeStorePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var volumes []*VolumeInfo
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		metadataPath := filepath.Join(volumeStorePath, entry.Name(), "metadata.json")
		data, err := os.ReadFile(metadataPath)
		if err != nil {
			continue
		}

		var vol VolumeInfo
		if err := json.Unmarshal(data, &vol); err != nil {
			continue
		}

		volumes = append(volumes, &vol)
	}

	return volumes, nil
}

// Remove 删除数据卷
func Remove(name string) error {
	volPath := filepath.Join(volumeStorePath, name)
	if _, err := os.Stat(volPath); os.IsNotExist(err) {
		return fmt.Errorf("卷 %s 不存在", name)
	}

	return os.RemoveAll(volPath)
}

// Inspect 查看数据卷详情
func Inspect(name string) (*VolumeInfo, error) {
	metadataPath := filepath.Join(volumeStorePath, name, "metadata.json")
	data, err := os.ReadFile(metadataPath)
	if err != nil {
		return nil, fmt.Errorf("卷 %s 不存在", name)
	}

	var vol VolumeInfo
	if err := json.Unmarshal(data, &vol); err != nil {
		return nil, fmt.Errorf("解析卷元数据失败: %w", err)
	}

	return &vol, nil
}

// ParseVolumeMount 解析 -v 参数
// 支持格式：
//   -v /host/path:/container/path         → bind mount
//   -v /host/path:/container/path:ro       → bind mount (read-only)
//   -v volume-name:/container/path         → named volume
//   -v volume-name:/container/path:ro      → named volume (read-only)
func ParseVolumeMount(volumeSpec string) (*VolumeMount, error) {
	parts := splitVolumeSpec(volumeSpec)
	if len(parts) < 2 {
		return nil, fmt.Errorf("无效的卷挂载格式，应为 source:destination[:ro]")
	}

	source := parts[0]
	destination := parts[1]
	readOnly := false

	if len(parts) >= 3 && parts[2] == "ro" {
		readOnly = true
	}

	// 判断是 bind mount 还是 named volume
	mountType := "volume"
	if isPath(source) {
		mountType = "bind"
	}

	return &VolumeMount{
		Source:      source,
		Destination: destination,
		Type:        mountType,
		ReadOnly:    readOnly,
	}, nil
}

// ResolveMountPath 解析挂载源的宿主机路径
// bind mount → 直接使用 source 路径
// named volume → /var/lib/mini-docker/volumes/<name>/_data
func ResolveMountPath(mount *VolumeMount) (string, error) {
	if mount.Type == "bind" {
		// 确保宿主机目录存在
		if err := os.MkdirAll(mount.Source, 0755); err != nil {
			return "", fmt.Errorf("创建宿主机目录失败: %w", err)
		}
		return mount.Source, nil
	}

	// named volume
	dataPath := filepath.Join(volumeStorePath, mount.Source, "_data")
	if err := os.MkdirAll(dataPath, 0755); err != nil {
		return "", fmt.Errorf("创建卷数据目录失败: %w", err)
	}

	// 如果卷不存在，自动创建
	metadataPath := filepath.Join(volumeStorePath, mount.Source, "metadata.json")
	if _, err := os.Stat(metadataPath); os.IsNotExist(err) {
		info := &VolumeInfo{
			Name:      mount.Source,
			Driver:    "local",
			MountPath: dataPath,
			CreatedAt: time.Now().Format("2006-01-02 15:04:05"),
		}
		data, _ := json.MarshalIndent(info, "", "  ")
		os.WriteFile(metadataPath, data, 0644)
	}

	return dataPath, nil
}

// splitVolumeSpec 分割卷挂载规格字符串
// 处理 Windows 路径特殊情况 (C:\path)
func splitVolumeSpec(spec string) []string {
	// 简单按 : 分割，但要注意 Windows 盘符
	var parts []string
	current := ""
	for i, ch := range spec {
		if ch == ':' {
			// 跳过 Windows 盘符后的冒号（如 C:）
			if i == 1 && len(current) == 1 && current[0] >= 'A' && current[0] <= 'Z' {
				current += string(ch)
				continue
			}
			parts = append(parts, current)
			current = ""
		} else {
			current += string(ch)
		}
	}
	if current != "" {
		parts = append(parts, current)
	}
	return parts
}

// isPath 判断字符串是否为文件路径（以 / 或字母盘开头）
func isPath(s string) bool {
	if len(s) == 0 {
		return false
	}
	return s[0] == '/' || (len(s) >= 2 && s[1] == ':')
}
