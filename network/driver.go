package network

/*
=======================================================================
  多网络驱动 —— 对齐 Docker 的 CNM 网络模型
=======================================================================

  Docker 网络模式：
  ┌──────────────┬─────────────────────────────────────────────┐
  │ bridge       │ 默认模式，veth + bridge + NAT               │
  │ host         │ 共享宿主机网络，无隔离，性能最好             │
  │ none         │ 无网络，只有 lo 接口                        │
  │ container:id │ 共享另一个容器的网络栈                      │
  └──────────────┴─────────────────────────────────────────────┘

  本文件实现 host/none/container 三种网络驱动，
  bridge 模式已在 network_linux.go 中实现。

=======================================================================
*/

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// NetworkDriver 网络驱动接口
type NetworkDriver interface {
	Setup(pid int) error
	Teardown() error
	GetContainerIP() string
}

// BridgeDriver bridge 网络驱动（已有实现，这里封装为接口）
type BridgeDriver struct {
	NetworkName string
	PortMap     string
	VethHost    string
	VethCont    string
	ContainerIP string
}

func (b *BridgeDriver) Setup(pid int) error {
	mgr := &NetworkManager{
		NetworkName: b.NetworkName,
		PortMap:     b.PortMap,
	}
	if err := mgr.Connect(pid); err != nil {
		return err
	}
	b.VethHost = mgr.GetVethHost()
	b.VethCont = mgr.VethCont
	b.ContainerIP = mgr.GetContainerIP()
	return nil
}

func (b *BridgeDriver) Teardown() error {
	if b.VethHost != "" {
		cmd := exec.Command("ip", "link", "delete", b.VethHost)
		_ = cmd.Run()
	}
	return nil
}

func (b *BridgeDriver) GetContainerIP() string {
	return b.ContainerIP
}

// HostDriver host 网络驱动
// 不创建新的 Network Namespace，容器直接使用宿主机网络
// 性能最好但无隔离
type HostDriver struct{}

func (h *HostDriver) Setup(pid int) error {
	// host 模式不需要做任何操作
	// 容器进程创建时不设置 CLONE_NEWNET 标志即可
	fmt.Println("  网络模式: host（共享宿主机网络）")
	return nil
}

func (h *HostDriver) Teardown() error {
	return nil
}

func (h *HostDriver) GetContainerIP() string {
	return "host"
}

// NoneDriver none 网络驱动
// 创建新的 Network Namespace，但不配置任何网络接口
// 容器内只有 lo 接口
type NoneDriver struct{}

func (n *NoneDriver) Setup(pid int) error {
	// none 模式：新 Network Namespace 已通过 CLONE_NEWNET 创建
	// 只需要启用 lo 接口
	cmd := exec.Command("nsenter", "-t", fmt.Sprintf("%d", pid), "-n",
		"ip", "link", "set", "lo", "up")
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("启用 lo 失败: %w", err)
	}
	fmt.Println("  网络模式: none（无外部网络）")
	return nil
}

func (n *NoneDriver) Teardown() error {
	return nil
}

func (n *NoneDriver) GetContainerIP() string {
	return ""
}

// ContainerDriver container 网络驱动
// 共享另一个容器的 Network Namespace
// 用于容器间共享网络栈（如 sidecar 模式）
type ContainerDriver struct {
	TargetPID int // 目标容器的 PID
}

func (c *ContainerDriver) Setup(pid int) error {
	// 将新容器的网络命名空间设置为目标容器的网络命名空间
	// 实际上需要在新容器创建时不设置 CLONE_NEWNET，
	// 然后通过 setns 加入目标容器的网络命名空间

	// 获取目标容器的网络命名空间路径
	nsPath := fmt.Sprintf("/proc/%d/ns/net", c.TargetPID)
	if _, err := os.Stat(nsPath); os.IsNotExist(err) {
		return fmt.Errorf("目标容器进程 %d 不存在", c.TargetPID)
	}

	fmt.Printf("  网络模式: container:%d（共享目标容器网络）\n", c.TargetPID)
	return nil
}

func (c *ContainerDriver) Teardown() error {
	// container 模式下不需要清理网络（由目标容器管理）
	return nil
}

func (c *ContainerDriver) GetContainerIP() string {
	// 共享目标容器的 IP
	return "shared"
}

// CreateNetworkDriver 根据网络模式创建对应的驱动
// networkSpec 格式: "bridge", "host", "none", "container:<id>"
func CreateNetworkDriver(networkSpec string, portMap string) (NetworkDriver, error) {
	switch {
	case networkSpec == "" || networkSpec == "bridge":
		return &BridgeDriver{
			NetworkName: "mini-docker-bridge",
			PortMap:     portMap,
		}, nil
	case networkSpec == "host":
		return &HostDriver{}, nil
	case networkSpec == "none":
		return &NoneDriver{}, nil
	case strings.HasPrefix(networkSpec, "container:"):
		containerID := strings.TrimPrefix(networkSpec, "container:")
		// 需要查找目标容器的 PID
		pid, err := findContainerPID(containerID)
		if err != nil {
			return nil, fmt.Errorf("查找目标容器失败: %w", err)
		}
		return &ContainerDriver{TargetPID: pid}, nil
	default:
		// 可能是自定义 bridge 网络名称
		return &BridgeDriver{
			NetworkName: networkSpec,
			PortMap:     portMap,
		}, nil
	}
}

// findContainerPID 查找容器的 PID
func findContainerPID(containerID string) (int, error) {
	// 从容器元数据中读取 PID
	infoPath := fmt.Sprintf("/var/run/mini-docker/%s.json", containerID)
	if len(containerID) > 12 {
		infoPath = fmt.Sprintf("/var/run/mini-docker/%s.json", containerID[:12])
	}

	data, err := os.ReadFile(infoPath)
	if err != nil {
		return 0, fmt.Errorf("容器 %s 不存在", containerID)
	}

	// 简化解析：搜索 "pid" 字段
	var info struct {
		Pid int `json:"pid"`
	}
	if err := parseJSON(data, &info); err != nil {
		return 0, err
	}

	if info.Pid == 0 {
		return 0, fmt.Errorf("容器 %s 未在运行", containerID)
	}

	return info.Pid, nil
}

func parseJSON(data []byte, v interface{}) error {
	return json.Unmarshal(data, v)
}
