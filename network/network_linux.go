//go:build linux

package network

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"mini-docker/constants"
	"mini-docker/utils"
)

const (
	networkStorePath = constants.NetworkStoreDir
	defaultSubnet    = constants.DefaultSubnet
	defaultGateway   = constants.DefaultGateway
)

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

var ipamMutex sync.Mutex

func (n *NetworkManager) Create(name string) error {
	if err := os.MkdirAll(networkStorePath, 0755); err != nil {
		return err
	}

	bridgeName := "mini-" + name
	subnet := defaultSubnet
	gateway := defaultGateway

	cmd := exec.Command("ip", "link", "add", bridgeName, "type", "bridge")
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("创建网桥失败: %w", err)
	}

	cmd = exec.Command("ip", "addr", "add", gateway+"/16", "dev", bridgeName)
	if err := cmd.Run(); err != nil {
		cleanupBridge(bridgeName)
		return fmt.Errorf("设置网桥 IP 失败: %w", err)
	}

	cmd = exec.Command("ip", "link", "set", bridgeName, "up")
	if err := cmd.Run(); err != nil {
		cleanupBridge(bridgeName)
		return fmt.Errorf("启用网桥失败: %w", err)
	}

	if err := enableIPForward(); err != nil {
		fmt.Printf("  警告: 开启 IP 转发失败: %v\n", err)
	}

	info := &NetworkInfo{
		Name:      name,
		Subnet:    subnet,
		Gateway:   gateway,
		Bridge:    bridgeName,
		Allocated: []string{},
	}

	return saveNetworkInfo(info)
}

func (n *NetworkManager) List() ([]*NetworkInfo, error) {
	if err := os.MkdirAll(networkStorePath, 0755); err != nil {
		return nil, err
	}

	entries, err := os.ReadDir(networkStorePath)
	if err != nil {
		return nil, err
	}

	var networks []*NetworkInfo
	for _, entry := range entries {
		if filepath.Ext(entry.Name()) != ".json" {
			continue
		}

		data, err := os.ReadFile(filepath.Join(networkStorePath, entry.Name()))
		if err != nil {
			continue
		}

		var netInfo NetworkInfo
		if err := json.Unmarshal(data, &netInfo); err != nil {
			continue
		}

		networks = append(networks, &netInfo)
	}

	return networks, nil
}

func (n *NetworkManager) Delete(name string) error {
	info, err := LoadNetworkInfo(name)
	if err != nil {
		return err
	}

	cmd := exec.Command("ip", "link", "delete", info.Bridge)
	_ = cmd.Run()

	infoPath := filepath.Join(networkStorePath, name+".json")
	return os.Remove(infoPath)
}

func (n *NetworkManager) Connect(pid int) error {
	if n.NetworkName == "" {
		return nil
	}

	info, err := LoadNetworkInfo(n.NetworkName)
	if err != nil {
		return fmt.Errorf("网络 %s 不存在", n.NetworkName)
	}

	containerIP, err := allocateIP(info)
	if err != nil {
		return fmt.Errorf("分配 IP 失败: %w", err)
	}
	n.ContainerIP = containerIP

	vethHost := fmt.Sprintf("veth-%d-h", pid)
	vethContainer := fmt.Sprintf("veth-%d-c", pid)
	n.VethHost = vethHost
	n.VethCont = vethContainer

	cmd := exec.Command("ip", "link", "add", vethHost, "type", "veth", "peer", "name", vethContainer)
	if err := cmd.Run(); err != nil {
		releaseIP(n.NetworkName, containerIP)
		return fmt.Errorf("创建 veth pair 失败: %w", err)
	}

	cmd = exec.Command("ip", "link", "set", vethContainer, "netns", fmt.Sprintf("%d", pid))
	if err := cmd.Run(); err != nil {
		cleanupVeth(vethHost)
		releaseIP(n.NetworkName, containerIP)
		return fmt.Errorf("将 veth 移入容器 namespace 失败: %w", err)
	}

	cmd = exec.Command("nsenter", "-t", fmt.Sprintf("%d", pid), "-n",
		"ip", "addr", "add", containerIP+"/16", "dev", vethContainer)
	if err := cmd.Run(); err != nil {
		cleanupVeth(vethHost)
		releaseIP(n.NetworkName, containerIP)
		return fmt.Errorf("设置容器 IP 失败: %w", err)
	}

	cmd = exec.Command("nsenter", "-t", fmt.Sprintf("%d", pid), "-n",
		"ip", "link", "set", vethContainer, "up")
	if err := cmd.Run(); err != nil {
		cleanupVeth(vethHost)
		releaseIP(n.NetworkName, containerIP)
		return fmt.Errorf("启用容器 veth 失败: %w", err)
	}

	cmd = exec.Command("nsenter", "-t", fmt.Sprintf("%d", pid), "-n",
		"ip", "link", "set", "lo", "up")
	if err := cmd.Run(); err != nil {
		cleanupVeth(vethHost)
		releaseIP(n.NetworkName, containerIP)
		return fmt.Errorf("启用容器 lo 失败: %w", err)
	}

	cmd = exec.Command("ip", "link", "set", vethHost, "master", info.Bridge)
	if err := cmd.Run(); err != nil {
		cleanupVeth(vethHost)
		releaseIP(n.NetworkName, containerIP)
		return fmt.Errorf("将 veth 连接到网桥失败: %w", err)
	}

	cmd = exec.Command("ip", "link", "set", vethHost, "up")
	if err := cmd.Run(); err != nil {
		cleanupVeth(vethHost)
		releaseIP(n.NetworkName, containerIP)
		return fmt.Errorf("启用宿主机 veth 失败: %w", err)
	}

	cmd = exec.Command("nsenter", "-t", fmt.Sprintf("%d", pid), "-n",
		"ip", "route", "add", "default", "via", info.Gateway)
	if err := cmd.Run(); err != nil {
		cleanupVeth(vethHost)
		releaseIP(n.NetworkName, containerIP)
		return fmt.Errorf("设置容器默认路由失败: %w", err)
	}

	cmd = exec.Command("iptables", "-t", "nat", "-A", "POSTROUTING",
		"-s", info.Subnet, "!", "-o", info.Bridge, "-j", "MASQUERADE")
	_ = cmd.Run()

	if n.PortMap != "" {
		if err := setupPortMapping(n.PortMap, containerIP, info.Bridge); err != nil {
			fmt.Printf("  警告: 端口映射失败: %v\n", err)
		} else {
			fmt.Printf("  端口映射: %s\n", n.PortMap)
		}
	}

	fmt.Printf("容器已连接到网络 %s (IP: %s)\n", n.NetworkName, containerIP)
	return nil
}

func (n *NetworkManager) Disconnect() error {
	if n.VethHost == "" {
		return nil
	}

	cmd := exec.Command("ip", "link", "delete", n.VethHost)
	_ = cmd.Run()

	if n.PortMap != "" && n.ContainerIP != "" {
		utils.CleanupPortMapping(n.PortMap, n.ContainerIP)
	}

	if n.NetworkName != "" && n.ContainerIP != "" {
		releaseIP(n.NetworkName, n.ContainerIP)
		info, err := LoadNetworkInfo(n.NetworkName)
		if err == nil && len(info.Allocated) == 0 {
			CleanupMasquerade(info.Subnet, info.Bridge)
		}
	}

	return nil
}

func (n *NetworkManager) GetVethHost() string {
	return n.VethHost
}

func (n *NetworkManager) GetContainerIP() string {
	return n.ContainerIP
}

func setupPortMapping(portMap string, containerIP string, bridgeName string) error {
	parts := strings.Split(portMap, ":")
	if len(parts) != 2 {
		return fmt.Errorf("端口映射格式错误，应为 hostPort:containerPort，如 8080:80")
	}

	hostPort := parts[0]
	containerPort := parts[1]

	if _, err := strconv.Atoi(hostPort); err != nil {
		return fmt.Errorf("宿主端口无效: %s", hostPort)
	}
	if _, err := strconv.Atoi(containerPort); err != nil {
		return fmt.Errorf("容器端口无效: %s", containerPort)
	}

	cmd := exec.Command("iptables", "-t", "nat", "-A", "PREROUTING",
		"-p", "tcp", "--dport", hostPort,
		"-j", "DNAT", "--to-destination", containerIP+":"+containerPort)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("添加 DNAT 规则失败: %w", err)
	}

	cmd = exec.Command("iptables", "-t", "nat", "-A", "OUTPUT",
		"-p", "tcp", "--dport", hostPort,
		"-j", "DNAT", "--to-destination", containerIP+":"+containerPort)
	_ = cmd.Run()

	cmd = exec.Command("iptables", "-t", "nat", "-A", "POSTROUTING",
		"-p", "tcp", "-d", containerIP, "--dport", containerPort,
		"-j", "MASQUERADE")
	_ = cmd.Run()

	return nil
}

func allocateIP(info *NetworkInfo) (string, error) {
	ipamMutex.Lock()
	defer ipamMutex.Unlock()

	_, ipNet, err := net.ParseCIDR(info.Subnet)
	if err != nil {
		return "", fmt.Errorf("解析子网失败: %w", err)
	}

	allocated := make(map[string]bool)
	allocated[info.Gateway] = true
	for _, ip := range info.Allocated {
		allocated[ip] = true
	}

	ip := make(net.IP, len(ipNet.IP))
	copy(ip, ipNet.IP)
	for i := 0; i < 65534; i++ {
		incIP(ip)
		if !ipNet.Contains(ip) {
			continue
		}
		ipStr := ip.String()
		if !allocated[ipStr] {
			info.Allocated = append(info.Allocated, ipStr)
			if err := saveNetworkInfo(info); err != nil {
				info.Allocated = info.Allocated[:len(info.Allocated)-1]
				return "", fmt.Errorf("保存网络信息失败: %w", err)
			}
			return ipStr, nil
		}
	}

	return "", fmt.Errorf("网络 %s 的 IP 地址已耗尽", info.Name)
}

func releaseIP(networkName string, ip string) {
	ipamMutex.Lock()
	defer ipamMutex.Unlock()

	info, err := LoadNetworkInfo(networkName)
	if err != nil {
		return
	}

	for i, allocated := range info.Allocated {
		if allocated == ip {
			info.Allocated = append(info.Allocated[:i], info.Allocated[i+1:]...)
			_ = saveNetworkInfo(info)
			return
		}
	}
}

// ReleaseIP 释放 IP 地址（公开版本）
func ReleaseIP(networkName string, ip string) {
	releaseIP(networkName, ip)
}

func incIP(ip net.IP) {
	for j := len(ip) - 1; j >= 0; j-- {
		ip[j]++
		if ip[j] > 0 {
			break
		}
	}
}

func cleanupBridge(bridgeName string) {
	cmd := exec.Command("ip", "link", "delete", bridgeName, "type", "bridge")
	_ = cmd.Run()
}

func cleanupVeth(vethHost string) {
	cmd := exec.Command("ip", "link", "delete", vethHost)
	_ = cmd.Run()
}

func CleanupMasquerade(subnet string, bridgeName string) {
	cmd := exec.Command("iptables", "-t", "nat", "-D", "POSTROUTING",
		"-s", subnet, "!", "-o", bridgeName, "-j", "MASQUERADE")
	_ = cmd.Run()
}

func enableIPForward() error {
	return os.WriteFile("/proc/sys/net/ipv4/ip_forward", []byte("1"), 0644)
}

func saveNetworkInfo(info *NetworkInfo) error {
	if err := os.MkdirAll(networkStorePath, 0755); err != nil {
		return err
	}

	data, err := json.MarshalIndent(info, "", "  ")
	if err != nil {
		return err
	}

	return os.WriteFile(filepath.Join(networkStorePath, info.Name+".json"), data, 0644)
}

func LoadNetworkInfo(name string) (*NetworkInfo, error) {
	infoPath := filepath.Join(networkStorePath, name+".json")
	data, err := os.ReadFile(infoPath)
	if err != nil {
		return nil, fmt.Errorf("网络 %s 不存在", name)
	}

	var netInfo NetworkInfo
	if err := json.Unmarshal(data, &netInfo); err != nil {
		return nil, err
	}

	return &netInfo, nil
}
