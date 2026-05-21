package spec

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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
	Args         []string      `json:"args"`
	Env          []string      `json:"env"`
	Cwd          string        `json:"cwd"`
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
	CpuShares     string
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
		memBytes := parseMemory(config.Memory)
		s.Linux.Resources.Memory = &Memory{Limit: &memBytes}
	}
	if config.CpuShares != "" {
		shares := parseCpuShares(config.CpuShares)
		s.Linux.Resources.CPU = &CPU{Shares: &shares}
	}

	for _, volSpec := range config.Volumes {
		parts := strings.SplitN(volSpec, ":", 3)
		if len(parts) < 2 {
			continue
		}
		hostPath := parts[0]
		containerPath := parts[1]
		opts := []string{"rbind"}
		if len(parts) >= 3 && parts[2] == "ro" {
			opts = append(opts, "ro")
		}
		s.Mounts = append(s.Mounts, Mount{
			Destination: containerPath,
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

func parseMemory(s string) int64 {
	s = strings.TrimSpace(s)
	multiplier := int64(1)
	switch {
	case strings.HasSuffix(s, "g"):
		multiplier = 1024 * 1024 * 1024
		s = strings.TrimSuffix(s, "g")
	case strings.HasSuffix(s, "m"):
		multiplier = 1024 * 1024
		s = strings.TrimSuffix(s, "m")
	case strings.HasSuffix(s, "k"):
		multiplier = 1024
		s = strings.TrimSuffix(s, "k")
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0
	}
	return v * multiplier
}

func parseCpuShares(s string) int64 {
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 1024
	}
	return v
}

// ExtractCloneFlags 从 OCI Spec 的 namespaces 提取 clone flags
func ExtractCloneFlags(namespaces []Namespace) uintptr {
	var flags uintptr
	nsMap := map[string]uintptr{
		"pid":     0x20000000, // CLONE_NEWPID
		"network": 0x40000000, // CLONE_NEWNET
		"ipc":     0x08000000, // CLONE_NEWIPC
		"uts":     0x04000000, // CLONE_NEWUTS
		"mount":   0x00020000, // CLONE_NEWNS
		"user":    0x10000000, // CLONE_NEWUSER
		"cgroup":  0x02000000, // CLONE_NEWCGROUP
	}
	for _, ns := range namespaces {
		if f, ok := nsMap[ns.Type]; ok {
			flags |= f
		}
	}
	return flags
}

// CapNameToValue 将 Capability 名称映射为系统值
var CapNameToValue = map[string]int{
	"CAP_CHOWN":            0,
	"CAP_DAC_OVERRIDE":     1,
	"CAP_DAC_READ_SEARCH":  2,
	"CAP_FOWNER":           3,
	"CAP_FSETID":           4,
	"CAP_KILL":             5,
	"CAP_SETGID":           6,
	"CAP_SETUID":           7,
	"CAP_SETPCAP":          8,
	"CAP_LINUX_IMMUTABLE":  9,
	"CAP_NET_BIND_SERVICE": 10,
	"CAP_NET_BROADCAST":    11,
	"CAP_NET_ADMIN":        12,
	"CAP_NET_RAW":          13,
	"CAP_IPC_LOCK":         14,
	"CAP_IPC_OWNER":        15,
	"CAP_SYS_MODULE":       16,
	"CAP_SYS_RAWIO":        17,
	"CAP_SYS_CHROOT":       18,
	"CAP_SYS_PTRACE":       19,
	"CAP_SYS_PACCT":        20,
	"CAP_SYS_ADMIN":        21,
	"CAP_SYS_BOOT":         22,
	"CAP_SYS_NICE":         23,
	"CAP_SYS_RESOURCE":     24,
	"CAP_SYS_TIME":         25,
	"CAP_SYS_TTY_CONFIG":   26,
	"CAP_MKNOD":            27,
	"CAP_LEASE":            28,
	"CAP_AUDIT_WRITE":      29,
	"CAP_AUDIT_CONTROL":    30,
	"CAP_SETFCAP":          31,
}
