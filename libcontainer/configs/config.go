//go:build linux

// Package configs 定义容器的配置结构，对标 runc/libcontainer/configs
package configs

import "syscall"

// Config 容器完整配置（对标 OCI runtime-spec + libcontainer 扩展）
type Config struct {
	// OCIVersion OCI 运行时规范版本（来自 config.json 的 ociVersion）
	OCIVersion string `json:"oci_version,omitempty"`

	// BundlePath OCI bundle 路径（包含 config.json 和 rootfs）
	BundlePath string `json:"bundle_path,omitempty"`

	// Rootfs 容器根文件系统路径
	Rootfs string `json:"rootfs"`

	// ReadonlyRootfs 是否只读挂载 rootfs
	ReadonlyRootfs bool `json:"readonly_rootfs,omitempty"`

	// Hostname 容器主机名
	Hostname string `json:"hostname,omitempty"`

	// Args 容器内要执行的命令（第一个为可执行文件）
	Args []string `json:"args,omitempty"`

	// Env 容器环境变量（格式: KEY=VALUE）
	Env []string `json:"env,omitempty"`

	// Cwd 容器工作目录
	Cwd string `json:"cwd,omitempty"`

	// User 容器用户（UID:GID 格式）
	User string `json:"user,omitempty"`

	// Namespaces 容器要创建的 Namespace 列表
	Namespaces Namespaces `json:"namespaces"`

	// Capabilities 容器的 Capability 配置
	Capabilities *Capabilities `json:"capabilities,omitempty"`

	// Networks 容器网络配置
	Networks []*Network `json:"networks,omitempty"`

	// Routes 容器路由配置
	Routes []*Route `json:"routes,omitempty"`

	// Cgroups 容器 cgroup 资源限制配置
	Cgroups *Resources `json:"cgroups,omitempty"`

	// Mounts 额外挂载点列表（Volume 等）
	Mounts []*Mount `json:"mounts,omitempty"`

	// MaskedPaths 容器内需要屏蔽的路径（安全特性）
	MaskedPaths []string `json:"masked_paths,omitempty"`

	// ReadonlyPaths 容器内需要只读的路径（安全特性）
	ReadonlyPaths []string `json:"readonly_paths,omitempty"`

	// Seccomp Seccomp 配置（系统调用过滤）
	Seccomp *Seccomp `json:"seccomp,omitempty"`

	// NoNewPrivileges 是否设置 PR_SET_NO_NEW_PRIVS
	NoNewPrivileges bool `json:"no_new_privileges,omitempty"`

	// Annotations 容器注解（来自 OCI Spec）
	Annotations map[string]string `json:"annotations,omitempty"`
}

// Namespaces Namespace 配置集合
type Namespaces []Namespace

// Namespace 单个 Namespace 配置
type Namespace struct {
	Type string `json:"type"`           // pid, network, mount, ipc, uts, user, cgroup
	Path string `json:"path,omitempty"` // 加入已有 Namespace 时的路径
}

// NamespaceTypeToCloneFlag 将 Namespace 类型映射为 clone flag
func NamespaceTypeToCloneFlag(nsType string) uintptr {
	switch nsType {
	case "pid":
		return syscall.CLONE_NEWPID
	case "network":
		return syscall.CLONE_NEWNET
	case "mount":
		return syscall.CLONE_NEWNS
	case "ipc":
		return syscall.CLONE_NEWIPC
	case "uts":
		return syscall.CLONE_NEWUTS
	case "user":
		return syscall.CLONE_NEWUSER
	case "cgroup":
		return 0x02000000 // syscall.CLONE_NEWCGROUP
	default:
		return 0
	}
}

// CloneFlags 将所有 Namespace 配置转换为 clone flags
func (n Namespaces) CloneFlags() uintptr {
	var flags uintptr
	for _, ns := range n {
		flags |= NamespaceTypeToCloneFlag(ns.Type)
	}
	return flags
}

// Capabilities 容器 Capability 配置
type Capabilities struct {
	Bounding    []string `json:"bounding"`
	Effective   []string `json:"effective"`
	Inheritable []string `json:"inheritable"`
	Permitted   []string `json:"permitted"`
	Ambient     []string `json:"ambient"`
}

// Mount 挂载点配置
type Mount struct {
	// Destination 容器内挂载目标路径
	Destination string `json:"destination"`

	// Type 文件系统类型（bind, tmpfs, proc, devpts 等）
	Type string `json:"type"`

	// Source 源路径（宿主机路径或设备名）
	Source string `json:"source"`

	// Options 挂载选项
	Options []string `json:"options,omitempty"`

	// Flags 挂载标志（MS_BIND, MS_RDONLY 等）
	Flags uintptr `json:"flags,omitempty"`
}

// Network 网络配置
type Network struct {
	// Type 网络类型（veth, loopback 等）
	Type string `json:"type"`

	// Name 网络接口名
	Name string `json:"name"`

	// Bridge 连接的网桥名
	Bridge string `json:"bridge,omitempty"`

	// IPAddress 容器 IP 地址
	IPAddress string `json:"ip_address,omitempty"`

	// Gateway 网关地址
	Gateway string `json:"gateway,omitempty"`

	// MacAddress MAC 地址
	MacAddress string `json:"mac_address,omitempty"`

	// VethHost 宿主机端 veth 名称
	VethHost string `json:"veth_host,omitempty"`

	// VethPeer 容器端 veth 名称
	VethPeer string `json:"veth_peer,omitempty"`
}

// Route 路由配置
type Route struct {
	Destination string `json:"destination"`
	Gateway     string `json:"gateway,omitempty"`
	Interface   string `json:"interface,omitempty"`
}
