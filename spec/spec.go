package spec

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

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
	Mounts      []Mount           `json:"mounts"`
	Linux       *Linux            `json:"linux,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

type Process struct {
	Terminal     bool          `json:"terminal"`
	User         User          `json:"user"`
	Args         []string      `json:"args"` //要执行的命令及其参数，index为0的是命令
	Env          []string      `json:"env"`
	Cwd          string        `json:"cwd"` //Cwd 是 Current Working Directory （当前工作目录）的缩写
	Capabilities *Capabilities `json:"capabilities,omitempty"`
}

type User struct {
	UID uint32 `json:"uid"`
	GID uint32 `json:"gid"`
}

type Capabilities struct {
	Bounding    []string `json:"bounding"`
	Effective   []string `json:"effective"`
	Inheritable []string `json:"inheritable"`
	Permitted   []string `json:"permitted"`
	Ambient     []string `json:"ambient"`
}

type Root struct {
	Path     string `json:"path"`
	Readonly bool   `json:"readonly"`
}

type Mount struct {
	Destination string   `json:"destination"`
	Type        string   `json:"type"`
	Source      string   `json:"source"`
	Options     []string `json:"options"`
}

type Linux struct {
	Namespaces    []Namespace `json:"namespaces"`
	Resources     *Resources  `json:"resources,omitempty"`
	MaskedPaths   []string    `json:"maskedPaths,omitempty"`
	ReadonlyPaths []string    `json:"readonlyPaths,omitempty"`
}

type Namespace struct {
	Type string `json:"type"`
	Path string `json:"path,omitempty"`
}

type Resources struct {
	Memory *Memory `json:"memory,omitempty"`
	CPU    *CPU    `json:"cpu,omitempty"`
	Pids   *Pids   `json:"pids,omitempty"`
}

type Memory struct {
	Limit *int64 `json:"limit,omitempty"`
}

type CPU struct {
	Shares *int64  `json:"shares,omitempty"`
	Quota  *int64  `json:"quota,omitempty"`
	Period *int64  `json:"period,omitempty"`
	Cpus   *string `json:"cpus,omitempty"`
}

type Pids struct {
	Limit *int64 `json:"limit,omitempty"`
}

type ContainerStatus string

const (
	StatusCreating ContainerStatus = "creating"
	StatusCreated  ContainerStatus = "created"
	StatusRunning  ContainerStatus = "running"
	StatusStopped  ContainerStatus = "stopped"
	StatusPaused   ContainerStatus = "paused"
)

type State struct {
	OCIVersion  string            `json:"ociVersion"`
	ID          string            `json:"id"`
	Status      ContainerStatus   `json:"status"`
	Pid         int               `json:"pid"`
	Bundle      string            `json:"bundle"`
	Annotations map[string]string `json:"annotations,omitempty"`
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

func SaveState(stateDir string, state State) error {
	if err := os.MkdirAll(stateDir, 0755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(stateDir, "state.json"), data, 0644)
}

func LoadState(stateDir string) (*State, error) {
	data, err := os.ReadFile(filepath.Join(stateDir, "state.json"))
	if err != nil {
		return nil, fmt.Errorf("读取 state.json 失败: %w", err)
	}
	var s State
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("解析 state.json 失败: %w", err)
	}
	return &s, nil
}

// RunConfig 与现有 container.RunConfig 对齐，用于生成 OCI Spec
type RunConfig struct {
	Tty           bool
	Memory        string
	CPUShares     string
	Image         string
	ImageRootFS   string
	Cmd           []string
	Volumes       []string
	Hostname      string
	Network       string
	RestartPolicy string
}

func DefaultSpec(config *RunConfig) *Spec {
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
			Capabilities: &Capabilities{
				Bounding:    defaultCapNames(),
				Effective:   defaultCapNames(),
				Inheritable: defaultCapNames(),
				Permitted:   defaultCapNames(),
			},
		},
		Root: &Root{
			Path:     config.ImageRootFS,
			Readonly: false,
		},
		Hostname: config.Hostname,
		Mounts: []Mount{
			{Destination: "/proc", Type: "proc", Source: "proc"},
			{Destination: "/dev", Type: "tmpfs", Source: "tmpfs", Options: []string{"nosuid", "strictatime", "mode=755", "size=65536k"}},
			{Destination: "/tmp", Type: "tmpfs", Source: "tmpfs", Options: []string{"nosuid", "nodev", "mode=1777", "size=67108864"}},
		},
		Linux: &Linux{
			Namespaces: []Namespace{
				{Type: "pid"},
				{Type: "network"},
				{Type: "ipc"},
				{Type: "uts"},
				{Type: "mount"},
			},
			Resources: &Resources{},
		},
		Annotations: map[string]string{
			"mini-docker.image":          config.Image,
			"mini-docker.restart-policy": config.RestartPolicy,
			"mini-docker.network":        config.Network,
		},
	}

	if config.Memory != "" {
		memBytes, err := utils.ParseMemory(config.Memory)
		if err != nil {
			fmt.Fprintf(os.Stderr, "警告: 解析内存限制失败: %v，将不设置内存限制\n", err)
		} else {
			s.Linux.Resources.Memory = &Memory{Limit: &memBytes}
		}
	}
	if config.CPUShares != "" {
		shares := int64(parseCPUShares(config.CPUShares))
		s.Linux.Resources.CPU = &CPU{Shares: &shares}
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
		s.Mounts = append(s.Mounts, Mount{
			Destination: mount.Destination,
			Type:        "bind",
			Source:      hostPath,
			Options:     opts,
		})
	}

	return s
}

func defaultCapNames() []string {
	return []string{
		"CAP_CHOWN",
		"CAP_DAC_OVERRIDE",
		"CAP_FSETID",
		"CAP_FOWNER",
		"CAP_MKNOD",
		"CAP_NET_RAW",
		"CAP_SETGID",
		"CAP_SETUID",
		"CAP_SETFCAP",
		"CAP_SETPCAP",
		"CAP_NET_BIND_SERVICE",
		"CAP_SYS_CHROOT",
		"CAP_KILL",
		"CAP_AUDIT_WRITE",
	}
}

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

// ExtractCloneFlags 从 OCI Spec 的 namespaces 提取 clone flags
func ExtractCloneFlags(namespaces []Namespace) uintptr {
	var flags uintptr
	nsMap := getNamespaceFlags()
	for _, ns := range namespaces {
		if f, ok := nsMap[ns.Type]; ok {
			flags |= f
		}
	}
	return flags
}
