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

var iptablesPath string

// iptablesCmd 返回可用的 iptables 命令路径
// 优先使用 iptables-legacy（在 WSL2 等 iptables-nft 环境下更可靠）
// iptables-nft 在某些环境下（如 WSL2）创建自定义链时存在兼容性问题：
//   - iptables -N 创建链返回成功但实际未在 nftables 中创建
//   - iptables -L 查看自定义链报 "chain is incompatible, use 'nft' tool"
//
// iptables-legacy 直接操作内核 netfilter，无此问题
func iptablesCmd() string {
	if iptablesPath != "" {
		return iptablesPath
	}
	if p, err := exec.LookPath("iptables-legacy"); err == nil {
		iptablesPath = p
	} else {
		iptablesPath = "iptables"
	}
	return iptablesPath
}

const (
	networkStorePath   = constants.NetworkStoreDir
	DefaultNetworkName = "bridge"
	DefaultBridgeName  = "mini-bridge"

	// iptables 链名前缀（对齐 Docker: Docker 使用 DOCKER/DOCKER-ISOLATION-STAGE-1/2 链名）
	// mini-docker 使用 MD 前缀的链名，确保与真实 Docker 的 iptables 链完全不冲突
	// 注意: iptables 链名最长 28 字符，因此使用 MD 而非 MINI-DOCKER 作为链名前缀
	chainPrefix              = "MD"
	natChainName             = "MD"
	isolationStage1ChainName = "MD-ISOLATION-1"
	isolationStage2ChainName = "MD-ISOLATION-2"
)

// Manager 网络管理器接口（对齐 Docker: libnetwork 的 Endpoint 抽象）
// 定义在 network 包中，避免 container 包与 network 包之间的循环依赖
type Manager interface {
	Connect(pid int) error
	Disconnect() error
	GetVethHost() string
	GetContainerIP() string
}

// 一个网络的元数据,代表一个网段的网络资源
type NetworkInfo struct {
	Name      string   `json:"name"`      // 如 "mynet"
	Subnet    string   `json:"subnet"`    // 如 "172.33.0.0/16"
	Gateway   string   `json:"gateway"`   // 如 "172.33.0.1"
	Bridge    string   `json:"bridge"`    // 如 "mini-mynet"
	Allocated []string `json:"allocated"` // 已分配 IP 列表
}

// 容器的网络连接控制器，用来记录单个容器和网络之间的绑定状态
type NetworkManager struct {
	NetworkName string //NetworkInfo对应的名称Name
	PortMap     string // 如 "8080:80"
	VethHost    string // 宿主机端的 veth 设备名
	VethCont    string // 容器端的 veth 设备名
	ContainerIP string // 容器分配到的 IP
}

var ipamMutex sync.Mutex

// CreateNetwork 创建网络
// 当用户执行类似于 mini-docker network create mynet 时会调用此函数
func CreateNetwork(name string) error {
	if err := os.MkdirAll(networkStorePath, 0755); err != nil {
		return err
	}

	bridgeName := "mini-" + name

	subnet, gateway, err := allocateSubnet() //分配子网
	if err != nil {
		return fmt.Errorf("分配子网失败: %w", err)
	}
	//创建虚拟网桥：
	//类似于在宿主机上虚拟出了一个“交换机”
	//ip link add mini-<name> type bridge     # 创建网桥设备
	//ip addr add <网关IP>/16 dev mini-<name> # 将网关 IP 绑定到网桥上
	//ip link set mini-<name> up              # 启用网桥
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

	// 对齐 Docker: 创建网络时设置 iptables 规则
	// Docker 使用专用链（DOCKER/DOCKER-ISOLATION-STAGE-1/2）管理规则，避免与系统规则混杂
	// mini-docker 使用 MINI-DOCKER 前缀的链名，确保与真实 Docker 的链不冲突
	//初始化 mini-docker 专用的 iptables 链和规则
	if err := ensureIptablesChains(); err != nil {
		cleanupBridge(bridgeName)
		return fmt.Errorf("初始化 iptables 链失败: %w", err)
	}
	setupNetworkIptablesRules(bridgeName, subnet)

	info := &NetworkInfo{
		Name:      name,
		Subnet:    subnet,
		Gateway:   gateway,
		Bridge:    bridgeName,
		Allocated: []string{gateway}, // 网关 IP 预分配，避免后续分配冲突
	}

	return saveNetworkInfo(info)
}

// ListNetworks 列出所有网络
func ListNetworks() ([]*NetworkInfo, error) {
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

// DeleteNetwork 删除网络
func DeleteNetwork(name string) error {
	if IsDefaultNetwork(name) {
		return fmt.Errorf("不能删除默认网络 %s（对齐 Docker: bridge 网络受保护）", name)
	}

	info, err := LoadNetworkInfo(name)
	if err != nil {
		return err
	}

	// 检查网络是否还有容器在使用
	if len(info.Allocated) > 0 {
		return fmt.Errorf("网络 %s 还有 %d 个容器在使用，请先删除相关容器", name, len(info.Allocated))
	}

	// 对齐 Docker: 删除网络时清理该网络的所有 iptables 规则
	cleanupNetworkIptablesRules(info.Bridge, info.Subnet)

	cmd := exec.Command("ip", "link", "delete", info.Bridge)
	_ = cmd.Run()

	infoPath := filepath.Join(networkStorePath, name+".json")
	return os.Remove(infoPath)
}

// NewManagerFromInfo 从持久化的容器元数据重建 NetworkManager（对齐 Docker: Daemon 恢复场景）
// 用于容器恢复、rm、start 等只有 ContainerInfo 而没有运行时 NetworkManager 实例的场景
// 重建的 NetworkManager 可正常调用 Disconnect()，但 Connect() 不应被调用
func NewManagerFromInfo(networkName, portMap, containerIP, vethHost string) *NetworkManager {
	return &NetworkManager{
		NetworkName: networkName,
		PortMap:     portMap,
		ContainerIP: containerIP,
		VethHost:    vethHost,
	}
}

// 容器连接网络
func (n *NetworkManager) Connect(pid int) error {
	if n.NetworkName == "" {
		return nil
	}

	info, err := LoadNetworkInfo(n.NetworkName)
	if err != nil {
		return fmt.Errorf("网络 %s 不存在", n.NetworkName)
	}

	containerIP, err := allocateIP(info) //从当前网络的可用 IP 池中分配一个未使用的 IP
	if err != nil {
		return fmt.Errorf("分配 IP 失败: %w", err)
	}
	n.ContainerIP = containerIP

	vethHost := fmt.Sprintf("vh-%x", pid)
	vethContainer := fmt.Sprintf("vc-%x", pid)
	n.VethHost = vethHost
	n.VethCont = vethContainer
	//创建 Veth Pair: ip link add vh-<pid> type veth peer name vc-<pid>
	cmd := exec.Command("ip", "link", "add", vethHost, "type", "veth", "peer", "name", vethContainer)
	if err := cmd.Run(); err != nil {
		releaseIP(n.NetworkName, containerIP)
		return fmt.Errorf("创建 veth pair 失败: %w", err)
	}
	//转移网卡至容器命名空间 ip link set vc-<pid> netns <pid>
	cmd = exec.Command("ip", "link", "set", vethContainer, "netns", fmt.Sprintf("%d", pid))
	if err := cmd.Run(); err != nil {
		cleanupVeth(vethHost)
		releaseIP(n.NetworkName, containerIP)
		return fmt.Errorf("将 veth 移入容器 namespace 失败: %w", err)
	}

	// 对齐 Docker: 在容器 netns 内将 veth 重命名为 eth0
	// Docker 在容器内总是将网络接口命名为 eth0，这样容器内程序查找 eth0 接口时能正常工作
	// 重命名必须在设置 IP 之前完成，后续操作使用 eth0 作为设备名
	//通过 nsenter -t <pid> -n <cmd> 命令，在容器的网络命名空间内执行：
	//重命名：将 vc-<pid> 重命名为标准的 eth0（对齐 Docker 行为）。
	//分配 IP：给 eth0 绑定刚刚分配的 ContainerIP/16。
	//启动网卡：启用 eth0 以及本地回环网卡 lo。
	const containerIface = "eth0"
	cmd = exec.Command("nsenter", "-t", fmt.Sprintf("%d", pid), "-n",
		"ip", "link", "set", vethContainer, "name", containerIface)
	if err := cmd.Run(); err != nil {
		cleanupVeth(vethHost)
		releaseIP(n.NetworkName, containerIP)
		return fmt.Errorf("重命名容器 veth 为 eth0 失败: %w", err)
	}

	cmd = exec.Command("nsenter", "-t", fmt.Sprintf("%d", pid), "-n",
		"ip", "addr", "add", containerIP+"/16", "dev", containerIface)
	if err := cmd.Run(); err != nil {
		cleanupVeth(vethHost)
		releaseIP(n.NetworkName, containerIP)
		return fmt.Errorf("设置容器 IP 失败: %w", err)
	}

	cmd = exec.Command("nsenter", "-t", fmt.Sprintf("%d", pid), "-n",
		"ip", "link", "set", containerIface, "up")
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
	//将留在宿主机的 vh-<pid> 绑定到虚拟网桥上（ip link set vh-<pid> master mini-<bridge>）
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
	//设置默认路由：
	//在容器内添加默认网关路由：ip route add default via <gateway_ip>。这样容器内凡是找不到本地路由的数据包，都会丢给网桥上的网关 IP
	cmd = exec.Command("nsenter", "-t", fmt.Sprintf("%d", pid), "-n",
		"ip", "route", "add", "default", "via", info.Gateway)
	if err := cmd.Run(); err != nil {
		cleanupVeth(vethHost)
		releaseIP(n.NetworkName, containerIP)
		return fmt.Errorf("设置容器默认路由失败: %w", err)
	}

	//端口映射：如果指定了端口映射（如 -p 8080:80），则调用 setupPortMapping 配置 DNAT 规则
	if n.PortMap != "" {
		if err := setupPortMapping(n.PortMap, containerIP, info.Bridge); err != nil {
			cleanupVeth(vethHost)
			releaseIP(n.NetworkName, containerIP)
			return fmt.Errorf("端口映射设置失败: %w", err)
		}
		fmt.Printf("  端口映射: %s\n", n.PortMap)
	}

	fmt.Printf("容器已连接到网络 %s (IP: %s)\n", n.NetworkName, containerIP)
	return nil
}

// 在容器停止或退出时，调用 Disconnect()，它会删除宿主机上的 vh-<pid>（Linux 机制中，删除 Veth Pair 的一端，另一端在容器内也会自动被销毁），并释放 IP 地址、清理端口映射
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
	}

	// MASQUERADE 规则是网络级别的，在 Delete() 中统一清理，不再在容器断开时清理（对齐 Docker）

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

	// 对齐 Docker: 端口映射规则放入 MD 专用链，而非直接添加到系统链
	// Docker 将 DNAT 规则放入 DOCKER 链，清理时只需清空该链
	// mini-docker 使用 MD 链名，与 Docker 的 DOCKER 链不冲突
	if err := ensureIptablesChains(); err != nil {
		return fmt.Errorf("初始化 iptables 链失败: %w", err)
	}

	ipt := iptablesCmd()
	cmd := exec.Command(ipt, "-t", "nat", "-A", natChainName,
		"-p", "tcp", "--dport", hostPort,
		"-j", "DNAT", "--to-destination", containerIP+":"+containerPort)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("添加 DNAT 规则失败: %w", err)
	}

	cmd = exec.Command(ipt, "-t", "nat", "-A", "OUTPUT",
		"-p", "tcp", "--dport", hostPort,
		"-j", natChainName)
	_ = cmd.Run()

	cmd = exec.Command(ipt, "-t", "nat", "-A", "POSTROUTING",
		"-p", "tcp", "-d", containerIP, "--dport", containerPort,
		"-j", "MASQUERADE")
	_ = cmd.Run()

	return nil
}

func allocateIP(info *NetworkInfo) (string, error) {
	ipamMutex.Lock()
	defer ipamMutex.Unlock()

	_, ipNet, err := net.ParseCIDR(info.Subnet) //ipNet 包含了网络地址和掩码，用于后续判断 IP 是否属于该子网
	//ipNet 是 *net.IPNet 类型，它包含两个字段：
	//
	//IPNet.IP表示该子网的网络地址（例如 172.33.0.0）。类型为net.IP 类型net.IP 本质上是 []byte（字节切片）,对于 IPv4，它长度为 4 字节；对于 IPv6，长度为 16 字节,为什么是byte,因为一个byte正好是一个字节可以表示0-255
	//
	//IPNet.Mask：表示子网掩码。

	if err != nil {
		return "", fmt.Errorf("解析子网失败: %w", err)
	}

	// 计算广播地址
	//对于 IPv4，广播地址是将网络地址的主机位全部置为 1。
	//步骤：
	//复制网络地址（如 172.33.0.0）作为起点。
	//遍历每个字节，对掩码取反（^ipNet.Mask[i]），然后按位或到广播地址副本上。
	//最终 broadcast 就是广播地址（如 172.33.255.255）
	broadcast := make(net.IP, len(ipNet.IP))
	copy(broadcast, ipNet.IP)  //copy(dst, src) 是 Go 的内置函数，用于将 src 切片中的元素复制到 dst 切片中。copy 后 broadcast 就变成了网络地址（例如 172.33.0.0）
	for i := range broadcast { //先掩码取反,取反后的字节序列进行或运算，效果是将网络地址中属于主机的位全部设置为 1，从而得到广播地址
		broadcast[i] |= ^ipNet.Mask[i]
	}

	allocated := make(map[string]bool)
	allocated[broadcast.String()] = true // 排除广播地址
	for _, ip := range info.Allocated {
		allocated[ip] = true
	}

	ip := make(net.IP, len(ipNet.IP))
	copy(ip, ipNet.IP)
	//循环使用 65534 次是因为：
	//子网掩码是 /16（如 172.33.0.0/16），该子网总共有 2^(32-16) = 65536 个 IP 地址。
	//其中网络地址（如 172.33.0.0）和广播地址（如 172.33.255.255）不能分配给普通设备，因此可分配的 IP 数量最多为 65536 - 2 = 65534。
	for i := 0; i < 65534; i++ {
		incIP(ip)
		if !ipNet.Contains(ip) { //判断给定的 IP 地址是否属于该子网
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

func incIP(ip net.IP) {
	for j := len(ip) - 1; j >= 0; j-- {
		ip[j]++ //加 1 后如果 > 0，说明没有发生溢出（即原来不是 255）。一旦没有溢出，就直接 break 结束循环；如果发生了溢出（原来是 255，加 1 后变为 0），则继续往高位进位（循环继续执行前一个字节）。
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
	ipt := iptablesCmd()
	cmd := exec.Command(ipt, "-t", "nat", "-D", "POSTROUTING",
		"-s", subnet, "!", "-o", bridgeName, "-j", "MASQUERADE")
	_ = cmd.Run()
}

// 开启 IP 转发：修改系统的 /proc/sys/net/ipv4/ip_forward 为 1，允许宿主机内核在不同网卡之间转发数据包，这是容器能够访问外网的前提
func enableIPForward() error {
	return os.WriteFile("/proc/sys/net/ipv4/ip_forward", []byte("1"), 0644)
}

// ensureIptablesChains 确保 mini-docker 所需的 iptables 专用链存在（对齐 Docker: dockerd 创建 DOCKER/DOCKER-ISOLATION 链）
// Docker 使用 DOCKER 链管理 DNAT 规则，使用 DOCKER-ISOLATION-STAGE-1/2 链实现跨 bridge 隔离
// mini-docker 使用 MD 前缀的链名，确保与真实 Docker 的链完全不冲突
// 注意: iptables 链名最长 28 字符，所以使用 MD 而非 MINI-DOCKER 作为前缀
//
// iptables-nft 兼容性:
//
//	iptables-nft 模式下，iptables -N 创建自定义链是可行的（只要名称不超过 28 字符），
//	之前的 MINI-DOCKER-ISOLATION-STAGE-1 (33字符) 超过限制导致创建失败。
//	改用 MD-ISOLATION-1/2 (14字符) 后，iptables -N 在 nft 模式下也能正常工作。
//	因此统一使用 iptables 命令，无需区分 nft/legacy 模式。
//
// 链结构（对齐 Docker）:
//
//	filter 表:
//	  FORWARD → MD-ISOLATION-1
//	               └─ 如果包从一个 mini-* bridge 进来 → 跳到 MD-ISOLATION-2
//	               └─ 否则 RETURN
//	            MD-ISOLATION-2
//	               └─ 如果包要发往另一个 mini-* bridge → DROP
//	               └─ 否则 RETURN
//
//	nat 表:
//	  PREROUTING → MD (DNAT 端口映射规则)
func ensureIptablesChains() error {
	ipt := iptablesCmd()

	if err := exec.Command(ipt, "-N", isolationStage1ChainName).Run(); err != nil {
		// 链已存在，忽略错误
	}
	if err := exec.Command(ipt, "-N", isolationStage2ChainName).Run(); err != nil {
		// 链已存在，忽略错误
	}

	if err := exec.Command(ipt, "-C", "FORWARD", "-j", isolationStage1ChainName).Run(); err != nil {
		if err := exec.Command(ipt, "-I", "FORWARD", "1", "-j", isolationStage1ChainName).Run(); err != nil {
			return fmt.Errorf("添加 FORWARD 规则失败: %w", err)
		}
	}

	if err := exec.Command(ipt, "-t", "nat", "-N", natChainName).Run(); err != nil {
		// 链已存在，忽略错误
	}

	if err := exec.Command(ipt, "-t", "nat", "-C", "PREROUTING", "-j", natChainName).Run(); err != nil {
		if err := exec.Command(ipt, "-t", "nat", "-A", "PREROUTING", "-j", natChainName).Run(); err != nil {
			return fmt.Errorf("添加 PREROUTING 规则失败: %w", err)
		}
	}

	return nil
}

// setupNetworkIptablesRules 为单个网络添加 iptables 规则（对齐 Docker: 创建网络时设置 FORWARD/MASQUERADE/ISOLATION 规则）
func setupNetworkIptablesRules(bridgeName string, subnet string) {
	ipt := iptablesCmd()

	// 对齐 Docker: 设置 bridge-nf-call-iptables=0，避免 bridge 二层流量经过 iptables FORWARD 链
	// 默认 Linux 内核 bridge-nf-call-iptables=1 时，bridge 上的流量也会经过 iptables FORWARD 链
	// 这会导致同一 bridge 内的容器通信被隔离规则 DROP（因为隔离规则在 FORWARD 链中）
	// Docker 在创建 bridge 网络时也会设置此参数为 0
	//将其设为 0，可以保证同一网桥内的容器直接进行二层交换，不走 iptables，从而保障内部通信顺畅。这与 Docker 的官方默认行为一致。
	os.WriteFile("/proc/sys/net/bridge/bridge-nf-call-iptables", []byte("0"), 0644)

	// 1. FORWARD 链: 允许从 bridge 进出的流量被转发
	//在 filter 表的 FORWARD（转发）链尾部追加两条规则：
	//只要是从这个网桥设备流入（-i）的数据包，全部放行（ACCEPT）。
	//只要是流向（-o）这个网桥设备的数据包，全部放行（ACCEPT）。
	exec.Command(ipt, "-A", "FORWARD", "-i", bridgeName, "-j", "ACCEPT").Run()
	exec.Command(ipt, "-A", "FORWARD", "-o", bridgeName, "-j", "ACCEPT").Run()

	// 2. MASQUERADE (SNAT): 使该网络所有容器都能访问外网，每个网络只需一条
	//MASQUERADE（动态 SNAT）的作用是：当容器的包要发往外网时，宿主机会把数据包的源 IP 改写为宿主机自己的公网/外网网卡 IP。当外网响应返回时，宿主机再把目的 IP 还原并转交给容器。这样，容器就拥有了访问互联网的能力。
	exec.Command(ipt, "-t", "nat", "-A", "POSTROUTING",
		"-s", subnet, "!", "-o", bridgeName, "-j", "MASQUERADE").Run()

	// 3. 跨 bridge 网络隔离（对齐 Docker: DOCKER-ISOLATION-STAGE-1/2）
	// Docker 默认行为: 不同 bridge 网络的容器不能互相通信
	// STAGE-1: 从该 bridge 进来的包 → 跳到 STAGE-2 检查
	exec.Command(ipt, "-I", isolationStage1ChainName, "1",
		"-i", bridgeName, "-j", isolationStage2ChainName).Run()
	// STAGE-2: 如果要发往该 bridge → DROP（阻止其他 bridge 的流量进入该 bridge）
	exec.Command(ipt, "-I", isolationStage2ChainName, "1",
		"-o", bridgeName, "-j", "DROP").Run()
}

// cleanupNetworkIptablesRules 清理单个网络的所有 iptables 规则（对齐 Docker: 删除网络时清理规则）
func cleanupNetworkIptablesRules(bridgeName string, subnet string) {
	ipt := iptablesCmd()
	// 1. 清理 FORWARD 规则
	exec.Command(ipt, "-D", "FORWARD", "-i", bridgeName, "-j", "ACCEPT").Run()
	exec.Command(ipt, "-D", "FORWARD", "-o", bridgeName, "-j", "ACCEPT").Run()

	// 2. 清理 MASQUERADE 规则
	exec.Command(ipt, "-t", "nat", "-D", "POSTROUTING",
		"-s", subnet, "!", "-o", bridgeName, "-j", "MASQUERADE").Run()

	// 3. 清理隔离规则
	exec.Command(ipt, "-D", isolationStage1ChainName,
		"-i", bridgeName, "-j", isolationStage2ChainName).Run()
	exec.Command(ipt, "-D", isolationStage2ChainName,
		"-o", bridgeName, "-j", "DROP").Run()
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

// 扫描磁盘上已有网络的网段，从 172.33.0.0/16 到 172.99.0.0/16 之间，自动挑选一个未被占用的网段作为该网络的子网，网关默认设为 .1。
func allocateSubnet() (subnet string, gateway string, err error) {
	if err := os.MkdirAll(networkStorePath, 0755); err != nil {
		return "", "", err
	}

	usedSeconds := make(map[byte]bool)
	entries, err := os.ReadDir(networkStorePath)
	if err == nil {
		for _, entry := range entries {
			if filepath.Ext(entry.Name()) != ".json" {
				continue
			}
			data, readErr := os.ReadFile(filepath.Join(networkStorePath, entry.Name()))
			if readErr != nil {
				continue
			}
			var netInfo NetworkInfo
			if json.Unmarshal(data, &netInfo) == nil && netInfo.Subnet != "" {
				parts := strings.SplitN(netInfo.Subnet, ".", 3) //将 netInfo.Subnet 字符串按点号 . 分割，最多拆分成 3 部分，结果存入 parts 切片
				if len(parts) >= 2 {
					if sec, e := strconv.Atoi(parts[1]); e == nil {
						usedSeconds[byte(sec)] = true
					}
				}
			}
		}
	}

	for sec := 33; sec <= 99; sec++ {
		if !usedSeconds[byte(sec)] {
			return fmt.Sprintf("172.%d.0.0/16", sec), fmt.Sprintf("172.%d.0.1", sec), nil
		}
	}

	return "", "", fmt.Errorf("可用子网已耗尽（已使用 172.33-99.0.0/16）")
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

// EnsureDefaultNetwork 确保默认网络存在（对齐 Docker: dockerd 启动时自动创建 bridge 网络）
// Docker 在启动时自动创建名为 "bridge" 的默认网络（对应 docker0 网桥），
// 容器在不指定 --network 时自动连接到该网络。
// mini-docker 对齐此行为：Daemon 启动时调用此函数，确保默认网络就绪。
func EnsureDefaultNetwork() error {
	if _, err := LoadNetworkInfo(DefaultNetworkName); err == nil {
		return nil
	}

	// 如果配置文件不存在但网桥设备仍存在（例如手动删除配置文件后），先清理残留网桥
	bridgeName := "mini-" + DefaultNetworkName
	cleanupBridge(bridgeName)

	if err := CreateNetwork(DefaultNetworkName); err != nil {
		return fmt.Errorf("创建默认网络失败: %w", err)
	}

	fmt.Printf("默认网络 %s 已自动创建\n", DefaultNetworkName)
	return nil
}

// IsDefaultNetwork 判断网络是否为默认网络
func IsDefaultNetwork(name string) bool {
	return name == DefaultNetworkName
}

// EnableLoopback 在容器的 network namespace 中启用 loopback 接口（对齐 Docker: --network=none 行为）
// Docker 在 --network=none 时创建独立 netns 但只启用 lo 接口，不创建 veth pair
// lo 接口对于容器内本地 socket 通信（如数据库本地连接）是必需的
func EnableLoopback(pid int) {
	cmd := exec.Command("nsenter", "-t", fmt.Sprintf("%d", pid), "-n",
		"ip", "link", "set", "lo", "up")
	_ = cmd.Run()
}

// CleanupAllIptables 清理 mini-docker 创建的所有 iptables 链和规则
// 在 Daemon 停止时调用，确保不留残留规则
func CleanupAllIptables() {
	ipt := iptablesCmd()
	exec.Command(ipt, "-F", isolationStage2ChainName).Run()
	exec.Command(ipt, "-F", isolationStage1ChainName).Run()
	exec.Command(ipt, "-t", "nat", "-F", natChainName).Run()

	exec.Command(ipt, "-D", "FORWARD", "-j", isolationStage1ChainName).Run()
	exec.Command(ipt, "-t", "nat", "-D", "PREROUTING", "-j", natChainName).Run()

	exec.Command(ipt, "-X", isolationStage2ChainName).Run()
	exec.Command(ipt, "-X", isolationStage1ChainName).Run()
	exec.Command(ipt, "-t", "nat", "-X", natChainName).Run()
}
