//go:build !linux

package network

import "fmt"

// Manager 网络管理器接口（对齐 Docker: libnetwork 的 Endpoint 抽象）
// 定义在 network 包中，避免 container 包与 network 包之间的循环依赖
type Manager interface {
	Connect(pid int) error
	Disconnect() error
	GetVethHost() string
	GetContainerIP() string
}

type NetworkInfo struct {
	Name      string   `json:"name"`
	Subnet    string   `json:"subnet"`
	Gateway   string   `json:"gateway"`
	Bridge    string   `json:"bridge"`
	Allocated []string `json:"allocated"`
}

type NetworkManager struct {
	NetworkName string
	PortMap     string
	VethHost    string
	VethCont    string
	ContainerIP string
}

func NewManagerFromInfo(networkName, portMap, containerIP, vethHost string) *NetworkManager {
	return &NetworkManager{
		NetworkName: networkName,
		PortMap:     portMap,
		ContainerIP: containerIP,
		VethHost:    vethHost,
	}
}

func (n *NetworkManager) Create(name string) error {
	return fmt.Errorf("网络管理仅在 Linux 上可用，请在 WSL2 或 Linux 环境中运行")
}

func (n *NetworkManager) List() ([]*NetworkInfo, error) {
	return nil, fmt.Errorf("网络管理仅在 Linux 上可用")
}

func (n *NetworkManager) Delete(name string) error {
	return fmt.Errorf("网络管理仅在 Linux 上可用")
}

func (n *NetworkManager) Connect(pid int) error {
	return fmt.Errorf("网络管理仅在 Linux 上可用")
}

func (n *NetworkManager) Disconnect() error {
	return fmt.Errorf("网络管理仅在 Linux 上可用")
}

func (n *NetworkManager) GetVethHost() string {
	return ""
}

func (n *NetworkManager) GetContainerIP() string {
	return ""
}

func LoadNetworkInfo(name string) (*NetworkInfo, error) {
	return nil, fmt.Errorf("网络管理仅在 Linux 上可用")
}

func CleanupMasquerade(subnet string, bridgeName string) {
}

const (
	DefaultNetworkName = "bridge"
	DefaultBridgeName  = "mini-bridge"
)

func EnsureDefaultNetwork() error {
	return fmt.Errorf("网络管理仅在 Linux 上可用")
}

func IsDefaultNetwork(name string) bool {
	return name == DefaultNetworkName
}

func EnableLoopback(pid int) {
}

func CleanupAllIptables() {
}

func CreateNetwork(name string) error {
	return fmt.Errorf("网络管理仅在 Linux 上可用，请在 WSL2 或 Linux 环境中运行")
}

func ListNetworks() ([]*NetworkInfo, error) {
	return nil, fmt.Errorf("网络管理仅在 Linux 上可用")
}

func DeleteNetwork(name string) error {
	return fmt.Errorf("网络管理仅在 Linux 上可用")
}
