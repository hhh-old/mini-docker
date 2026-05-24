//go:build !linux

package network

import "fmt"

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

func ReleaseIP(networkName string, ip string) {
}

func LoadNetworkInfo(name string) (*NetworkInfo, error) {
	return nil, fmt.Errorf("网络管理仅在 Linux 上可用")
}

func CleanupMasquerade(subnet string, bridgeName string) {
}
