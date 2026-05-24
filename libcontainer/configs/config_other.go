//go:build !linux

package configs

// Config 容器完整配置（对标 OCI runtime-spec + libcontainer 扩展）
type Config struct {
	BundlePath      string        `json:"bundle_path,omitempty"`
	Rootfs          string        `json:"rootfs"`
	ReadonlyRootfs  bool          `json:"readonly_rootfs,omitempty"`
	Hostname        string        `json:"hostname,omitempty"`
	Args            []string      `json:"args,omitempty"`
	Env             []string      `json:"env,omitempty"`
	Cwd             string        `json:"cwd,omitempty"`
	User            string        `json:"user,omitempty"`
	Namespaces      Namespaces    `json:"namespaces"`
	Capabilities    *Capabilities `json:"capabilities,omitempty"`
	Networks        []*Network    `json:"networks,omitempty"`
	Routes          []*Route      `json:"routes,omitempty"`
	Cgroups         *Resources    `json:"cgroups,omitempty"`
	Mounts          []*Mount      `json:"mounts,omitempty"`
	MaskedPaths     []string      `json:"masked_paths,omitempty"`
	ReadonlyPaths   []string      `json:"readonly_paths,omitempty"`
	Seccomp         *Seccomp      `json:"seccomp,omitempty"`
	NoNewPrivileges bool          `json:"no_new_privileges,omitempty"`
}

type Namespaces []Namespace

type Namespace struct {
	Type string `json:"type"`
	Path string `json:"path,omitempty"`
}

func NamespaceTypeToCloneFlag(nsType string) uintptr { return 0 }
func (n Namespaces) CloneFlags() uintptr             { return 0 }
func (n Namespaces) Contains(nsType string) bool     { return false }
func (n Namespaces) Get(nsType string) *Namespace    { return nil }

type Capabilities struct {
	Bounding    []string `json:"bounding"`
	Effective   []string `json:"effective"`
	Inheritable []string `json:"inheritable"`
	Permitted   []string `json:"permitted"`
	Ambient     []string `json:"ambient"`
}

type Mount struct {
	Destination string   `json:"destination"`
	Type        string   `json:"type"`
	Source      string   `json:"source"`
	Options     []string `json:"options,omitempty"`
	Flags       uintptr  `json:"flags,omitempty"`
}

type Network struct {
	Type       string `json:"type"`
	Name       string `json:"name"`
	Bridge     string `json:"bridge,omitempty"`
	IPAddress  string `json:"ip_address,omitempty"`
	Gateway    string `json:"gateway,omitempty"`
	MacAddress string `json:"mac_address,omitempty"`
	VethHost   string `json:"veth_host,omitempty"`
	VethPeer   string `json:"veth_peer,omitempty"`
}

type Route struct {
	Destination string `json:"destination"`
	Gateway     string `json:"gateway,omitempty"`
	Interface   string `json:"interface,omitempty"`
}
