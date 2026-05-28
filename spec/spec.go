package spec

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"mini-docker/libcontainer/configs"
	"mini-docker/utils"
	"mini-docker/volume"
)

// Spec 对应 OCI runtime-spec 的 config.json
// 参考: https://github.com/opencontainers/runtime-spec/blob/main/config.md
type Spec struct {
	OCIVersion  string            `json:"ociVersion"`
	Process     *Process          `json:"process"`
	Root        *Root             `json:"root"`
	Hostname    string            `json:"hostname"`
	Mounts      []configs.Mount   `json:"mounts"`
	Linux       *Linux            `json:"linux,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

type Process struct {
	Terminal     bool                  `json:"terminal"`
	User         User                  `json:"user"`
	Args         []string              `json:"args"` //要执行的命令及其参数，index为0的是命令
	Env          []string              `json:"env"`
	Cwd          string                `json:"cwd"` //Cwd 是 Current Working Directory （当前工作目录）的缩写
	Capabilities *configs.Capabilities `json:"capabilities,omitempty"`
}

type User struct {
	UID uint32 `json:"uid"`
	GID uint32 `json:"gid"`
}

type Root struct {
	Path     string `json:"path"`
	Readonly bool   `json:"readonly"`
}

type Linux struct {
	Namespaces    []configs.Namespace `json:"namespaces"`
	Resources     *configs.Resources  `json:"resources,omitempty"`
	MaskedPaths   []string            `json:"maskedPaths,omitempty"`
	ReadonlyPaths []string            `json:"readonlyPaths,omitempty"`
}

const CurrentOCIVersion = "1.0.0"

func SaveSpec(s *Spec, bundlePath string) error {
	if err := os.MkdirAll(bundlePath, 0755); err != nil {
		return fmt.Errorf("创建 bundle 目录失败: %w", err)
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化 OCI Spec 失败: %w", err)
	}
	return os.WriteFile(filepath.Join(bundlePath, "config.json"), data, 0644)
}

func LoadSpec(bundlePath string) (*Spec, error) {
	data, err := os.ReadFile(filepath.Join(bundlePath, "config.json"))
	if err != nil {
		return nil, fmt.Errorf("读取 config.json 失败: %w", err)
	}
	var s Spec
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("解析 config.json 失败: %w", err)
	}
	return &s, nil
}

type SpecConfig struct {
	Tty           bool
	Memory        string
	CPUShares     string
	Image         string
	RootFS        string
	Cmd           []string
	Volumes       []string
	Hostname      string
	Network       string
	RestartPolicy string
	OverlayMerged string
	OverlayUpper  string
	OverlayWork   string
	PortMap       string
}

func DefaultSpec(config *SpecConfig) *Spec {
	s := &Spec{
		OCIVersion: CurrentOCIVersion,
		Process: &Process{
			Terminal: config.Tty,
			User:     User{UID: 0, GID: 0},
			Args:     config.Cmd,
			Env: []string{
				"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
				"HOME=/root",
				"TERM=xterm",
			},
			Cwd: "/",
			Capabilities: &configs.Capabilities{
				Bounding:    configs.DefaultCapabilities,
				Effective:   configs.DefaultCapabilities,
				Inheritable: configs.DefaultCapabilities,
				Permitted:   configs.DefaultCapabilities,
			},
		},
		Root: &Root{
			Path:     config.RootFS,
			Readonly: false,
		},
		Hostname: config.Hostname,
		Mounts: []configs.Mount{
			{Destination: "/proc", Type: "proc", Source: "proc"},
			{Destination: "/dev", Type: "tmpfs", Source: "tmpfs", Options: []string{"nosuid", "strictatime", "mode=755", "size=65536k"}},
			{Destination: "/tmp", Type: "tmpfs", Source: "tmpfs", Options: []string{"nosuid", "nodev", "mode=1777", "size=67108864"}},
		},
		Linux: &Linux{
			Namespaces: buildNamespaces(config.Network),
			Resources:  &configs.Resources{},
		},
		Annotations: map[string]string{
			"mini-docker.image":          config.Image,
			"mini-docker.restart-policy": config.RestartPolicy,
			"mini-docker.network":        config.Network,
		},
	}

	if config.Memory != "" {
		memBytes, err := utils.ParseMemory(config.Memory) //调用 utils.ParseMemory() 将字符串转为字节数,后面cgroup要用到字节数单位的内存
		if err != nil {
			fmt.Fprintf(os.Stderr, "警告: 解析内存限制失败: %v，将不设置内存限制\n", err)
		} else {
			s.Linux.Resources.Memory = &configs.Memory{Limit: &memBytes}
		}
	}
	if config.CPUShares != "" {
		shares := int64(parseCPUShares(config.CPUShares))
		s.Linux.Resources.CPU = &configs.CPU{Shares: &shares}
	}

	for _, volSpec := range config.Volumes {
		mount, err := volume.ParseVolumeMount(volSpec)
		if err != nil {
			continue
		}
		hostPath, err := volume.ResolveMountPath(mount)
		if err != nil {
			continue
		}
		opts := []string{"rbind"}
		if mount.ReadOnly {
			opts = append(opts, "ro")
		}
		s.Mounts = append(s.Mounts, configs.Mount{
			Destination: mount.Destination,
			Type:        "bind",
			Source:      hostPath,
			Options:     opts,
		})
	}

	if config.OverlayMerged != "" {
		s.Annotations["mini-docker.overlay.merged"] = config.OverlayMerged
		s.Annotations["mini-docker.overlay.upper"] = config.OverlayUpper
		s.Annotations["mini-docker.overlay.work"] = config.OverlayWork
	}

	if config.PortMap != "" {
		s.Annotations["mini-docker.port-map"] = config.PortMap
	}

	return s
}

// parseCPUShares 的上下界 2 ~ 262144 来自 Linux 内核 CFS 调度器对 cpu.shares 的硬性约束（ MIN_SHARES=2 , MAX_SHARES=262144 ），
// 默认值 1024 来自内核的 NICE_0_LOAD 。在用户态做范围检查是为了避免内核静默裁剪导致用户预期不符。
func parseCPUShares(s string) uint64 {
	v, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return 1024
	}
	if v < 2 {
		return 2
	}
	if v > 262144 {
		return 262144
	}
	return v
}

// buildNamespaces 根据网络模式动态构建 namespace 列表（对齐 Docker: --network=host 不创建 network namespace）
// Docker 行为:
//   - --network=host: 容器共享宿主机网络栈，不创建 network namespace
//   - --network=none/bridge/<自定义>: 容器创建独立的 network namespace
func buildNamespaces(networkMode string) []configs.Namespace {
	namespaces := []configs.Namespace{
		{Type: "pid"},
		{Type: "ipc"},
		{Type: "uts"},
		{Type: "mount"},
	}

	// 对齐 Docker: --network=host 时不创建 network namespace，容器共享宿主机网络栈
	if networkMode != "host" {
		namespaces = append([]configs.Namespace{{Type: "network"}}, namespaces...)
	}

	return namespaces
}
