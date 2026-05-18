package container

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const (
	containerStoreDir = "/var/run/mini-docker"
	containerDataDir  = "/var/lib/mini-docker/containers"
	imageStoreDir     = "/var/lib/mini-docker/images"
)

type ContainerInfo struct {
	ID            string   `json:"id"`
	Name          string   `json:"name"`
	Pid           int      `json:"pid"`
	Image         string   `json:"image"`
	Cmd           []string `json:"cmd"`
	Status        string   `json:"status"`
	CreatedAt     string   `json:"created_at"`
	RootFS        string   `json:"rootfs"`
	CgroupName    string   `json:"cgroup_name"`
	NetworkName   string   `json:"network_name"`
	VethHost      string   `json:"veth_host"`
	ContainerIP   string   `json:"container_ip"`
	PortMap       string   `json:"port_map"`
	OverlayMerged string   `json:"overlay_merged"`
	OverlayUpper  string   `json:"overlay_upper"`
	OverlayWork   string   `json:"overlay_work"`
}

type RunConfig struct {
	Tty       bool
	Detach    bool
	Memory    string
	CpuShares string
	Network   string
	Name      string
	PortMap   string
	Image     string
	Cmd       []string //用于存储要在容器内执行的命令及其参数,对于 ./mini-docker run -it myos /bin/sh ，它就是 ["/bin/sh"] ，最终会让容器启动一个交互式 shell。
}

type CgroupManager interface {
	Apply(pid int) error
	Destroy() error
	Freeze() error
	Unfreeze() error
}

type NetworkManager interface {
	Connect(pid int) error
	Disconnect() error
	GetVethHost() string
	GetContainerIP() string
}

func Run(config RunConfig, cg CgroupManager, netManager NetworkManager) error {
	rootFSPath := filepath.Join(imageStoreDir, config.Image, "rootfs")
	if _, err := os.Stat(rootFSPath); os.IsNotExist(err) {
		return fmt.Errorf("镜像 %s 不存在，请先使用 mini-docker pull 拉取", config.Image)
	}

	containerID := generateContainerID()
	if config.Name == "" {
		config.Name = containerID[:12]
	}

	overlay, err := createOverlayDirs(containerID)
	if err != nil {
		return fmt.Errorf("创建 OverlayFS 目录失败: %w", err)
	}

	containerInfo := &ContainerInfo{
		ID:            containerID,
		Name:          config.Name,
		Image:         config.Image,
		Cmd:           config.Cmd,
		Status:        "running",
		CreatedAt:     time.Now().Format("2006-01-02 15:04:05"),
		RootFS:        rootFSPath,
		OverlayMerged: overlay.Merged,
		OverlayUpper:  overlay.Upper,
		OverlayWork:   overlay.Work,
	}
	// /proc/self/exe —— 它指向当前可执行文件自身（即 mini-docker ）。这意味着子进程启动的还是 mini-docker 自己，但第一个参数变成了 "init" 。
	cmd := exec.Command("/proc/self/exe", append([]string{"init"}, rootFSPath)...)
	cmd.Args = append(cmd.Args, config.Cmd...)

	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	cmd.Env = append(os.Environ(),
		"MINI_DOCKER_OVERLAY_MERGED="+overlay.Merged,
		"MINI_DOCKER_OVERLAY_UPPER="+overlay.Upper,
		"MINI_DOCKER_OVERLAY_WORK="+overlay.Work,
	)

	nsFlags := NewNamespaceFlags()
	//通过 Cloneflags 在 fork 时创建新的命名空间
	setCloneFlags(cmd, nsFlags, config.Tty)

	if err := cmd.Start(); err != nil { // 非阻塞，子进程开始运行
		cleanupOverlay(containerInfo)
		return fmt.Errorf("启动容器进程失败: %w", err)
	}

	containerInfo.Pid = cmd.Process.Pid
	containerInfo.CgroupName = fmt.Sprintf("mini-docker-%s", containerID[:12])

	if cg != nil {
		if err := cg.Apply(cmd.Process.Pid); err != nil {
			fmt.Printf("警告: 设置 cgroup 失败: %v\n", err)
		}
	}

	if netManager != nil && config.Network != "" {
		if err := netManager.Connect(cmd.Process.Pid); err != nil {
			fmt.Printf("警告: 设置网络失败: %v\n", err)
		}
		containerInfo.NetworkName = config.Network
		containerInfo.PortMap = config.PortMap
		containerInfo.VethHost = netManager.GetVethHost()
		containerInfo.ContainerIP = netManager.GetContainerIP()
	}

	if err := saveContainerInfo(containerInfo); err != nil {
		return fmt.Errorf("保存容器信息失败: %w", err)
	}

	if !config.Detach {
		if err := cmd.Wait(); err != nil { //调用 cmd.Wait() 阻塞等待容器进程退出
			fmt.Printf("容器进程退出: %v\n", err)
		}
		cleanupContainer(containerInfo, cg, netManager)
	} else {
		go func() {
			_ = cmd.Wait()
			cleanupContainer(containerInfo, cg, netManager)
		}()
		fmt.Printf("容器 %s 已在后台启动 (PID: %d)\n", containerID[:12], cmd.Process.Pid)
	}

	return nil
}

// rootFSPath = var/lib/mini-docker/images/myos/rootfs/
func InitProcess(rootFSPath string, cmd []string) error {
	var overlay *OverlayDirs
	// upperdir = /var/lib/mini-docker/containers/<id>/overlay/upper/  │
	// workdir  = /var/lib/mini-docker/containers/<id>/overlay/work/   │
	// merged   = /var/lib/mini-docker/containers/<id>/overlay/merged/
	merged := os.Getenv("MINI_DOCKER_OVERLAY_MERGED")
	upper := os.Getenv("MINI_DOCKER_OVERLAY_UPPER")
	work := os.Getenv("MINI_DOCKER_OVERLAY_WORK")
	if merged != "" {
		overlay = &OverlayDirs{
			Merged: merged,
			Upper:  upper,
			Work:   work,
		}
	}

	if err := SetupRootFS(rootFSPath, overlay); err != nil {
		return fmt.Errorf("设置 rootfs 失败: %w", err)
	}

	if err := setHostname("mini-docker"); err != nil {
		return fmt.Errorf("设置主机名失败: %w", err)
	}
	//  用用户命令替换当前进程
	//./mini-docker run -it myos /bin/sh
	//│
	//├── 1. 父进程 fork 子进程 (cmd.Start)
	//│   └── 子进程 PID=100 (mini-docker init)
	//│
	//├── 2. 子进程执行 InitProcess
	//│   ├── SetupRootFS (设置文件系统)
	//│   ├── setHostname (设置主机名)
	//│   └── syscallExec("/bin/sh", ...)
	//│       │
	//│       └── 内核执行 execve("/bin/sh", ...)
	//│           │
	//│           └── 进程 PID=100 被替换为 /bin/sh
	//│               ├── 代码：/bin/sh 的代码
	//│               ├── 数据：/bin/sh 的数据
	//│               └── 开始执行 /bin/sh
	//│
	//└── 3. 用户看到 shell 提示符
	//    $
	if err := syscallExec(cmd[0], cmd[0:], os.Environ()); err != nil {
		return fmt.Errorf("执行命令失败: %w", err)
	}

	return nil
}

func Exec(containerID string, cmd []string) error {
	containerInfo, err := loadContainerInfo(containerID)
	if err != nil {
		return fmt.Errorf("查找容器失败: %w", err)
	}

	if containerInfo.Status != "running" {
		return fmt.Errorf("容器 %s 未在运行", containerID)
	}

	nsTypes := []string{"ipc", "uts", "net", "pid", "mnt"}
	for _, nsType := range nsTypes {
		nsPath := GetNamespacePath(containerInfo.Pid, nsType)
		if err := SetNamespace(nsPath); err != nil {
			return fmt.Errorf("加入 namespace %s 失败: %w", nsType, err)
		}
	}

	execCmd := exec.Command(cmd[0], cmd[1:]...)
	execCmd.Stdin = os.Stdin
	execCmd.Stdout = os.Stdout
	execCmd.Stderr = os.Stderr

	return execCmd.Run()
}

func Stop(containerID string) error {
	containerInfo, err := loadContainerInfo(containerID)
	if err != nil {
		return fmt.Errorf("查找容器失败: %w", err)
	}

	if containerInfo.Status != "running" && containerInfo.Status != "paused" {
		return fmt.Errorf("容器 %s 未在运行", containerID)
	}

	if err := sendSignal(containerInfo.Pid, 15); err != nil {
		return fmt.Errorf("发送 SIGTERM 失败: %w", err)
	}

	time.Sleep(2 * time.Second)

	if err := checkProcessAlive(containerInfo.Pid); err == nil {
		if err := sendSignal(containerInfo.Pid, 9); err != nil {
			return fmt.Errorf("发送 SIGKILL 失败: %w", err)
		}
	}

	containerInfo.Status = "stopped"
	cleanupContainerNetwork(containerInfo)
	cleanupCgroup(containerInfo.CgroupName)
	return saveContainerInfo(containerInfo)
}

func Pause(containerID string, cg CgroupManager) error {
	containerInfo, err := loadContainerInfo(containerID)
	if err != nil {
		return fmt.Errorf("查找容器失败: %w", err)
	}

	if containerInfo.Status != "running" {
		return fmt.Errorf("容器 %s 未在运行", containerID)
	}

	if err := cg.Freeze(); err != nil {
		return fmt.Errorf("冻结容器失败: %w", err)
	}

	containerInfo.Status = "paused"
	return saveContainerInfo(containerInfo)
}

func Unpause(containerID string, cg CgroupManager) error {
	containerInfo, err := loadContainerInfo(containerID)
	if err != nil {
		return fmt.Errorf("查找容器失败: %w", err)
	}

	if containerInfo.Status != "paused" {
		return fmt.Errorf("容器 %s 未处于暂停状态", containerID)
	}

	if err := cg.Unfreeze(); err != nil {
		return fmt.Errorf("恢复容器失败: %w", err)
	}

	containerInfo.Status = "running"
	return saveContainerInfo(containerInfo)
}

func Remove(containerID string) error {
	containerInfo, err := loadContainerInfo(containerID)
	if err != nil {
		return fmt.Errorf("查找容器失败: %w", err)
	}

	if containerInfo.Status == "running" || containerInfo.Status == "paused" {
		return fmt.Errorf("容器 %s 正在运行，请先停止容器", containerID)
	}

	cleanupOverlay(containerInfo)
	cleanupCgroup(containerInfo.CgroupName)

	infoPath := getContainerInfoPath(containerID)
	return os.Remove(infoPath)
}

func ListContainers() ([]*ContainerInfo, error) {
	if err := os.MkdirAll(containerStoreDir, 0755); err != nil {
		return nil, err
	}

	entries, err := os.ReadDir(containerStoreDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var containers []*ContainerInfo
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}

		infoPath := filepath.Join(containerStoreDir, entry.Name())
		data, err := os.ReadFile(infoPath)
		if err != nil {
			continue
		}

		var c ContainerInfo
		if err := json.Unmarshal(data, &c); err != nil {
			continue
		}

		if c.Status == "running" {
			if err := checkProcessAlive(c.Pid); err != nil {
				c.Status = "stopped"
				_ = saveContainerInfo(&c)
			}
		}

		containers = append(containers, &c)
	}

	return containers, nil
}

func HandleInit() {
	args := os.Args[2:]
	if len(args) < 2 {
		fmt.Fprintf(os.Stderr, "init: 参数不足\n")
		os.Exit(1)
	}

	rootFSPath := args[0]
	userCmd := args[1:]

	if err := InitProcess(rootFSPath, userCmd); err != nil {
		fmt.Fprintf(os.Stderr, "容器初始化失败: %v\n", err)
		os.Exit(1)
	}
}

func IsInitProcess() bool {
	return len(os.Args) >= 2 && os.Args[1] == "init"
}

func GetContainerCgroupName(containerID string) (string, error) {
	info, err := loadContainerInfo(containerID)
	if err != nil {
		return "", err
	}
	return info.CgroupName, nil
}

func createOverlayDirs(containerID string) (*OverlayDirs, error) {
	shortID := containerID
	if len(shortID) > 12 {
		shortID = shortID[:12]
	}

	baseDir := filepath.Join(containerDataDir, shortID, "overlay")
	mergedDir := filepath.Join(baseDir, "merged")
	upperDir := filepath.Join(baseDir, "upper")
	workDir := filepath.Join(baseDir, "work")

	os.RemoveAll(baseDir)

	for _, dir := range []string{mergedDir, upperDir, workDir} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return nil, fmt.Errorf("创建目录 %s 失败: %w", dir, err)
		}
	}

	return &OverlayDirs{
		Merged: mergedDir,
		Upper:  upperDir,
		Work:   workDir,
	}, nil
}

// 执行清理（销毁 cgroup、卸载 OverlayFS、更新状态）
func cleanupContainer(info *ContainerInfo, cg CgroupManager, netManager NetworkManager) {
	info.Status = "stopped"
	_ = saveContainerInfo(info)

	if netManager != nil {
		_ = netManager.Disconnect()
	}
	if cg != nil {
		_ = cg.Destroy()
	}

	cleanupOverlay(info)
}

func cleanupOverlay(info *ContainerInfo) {
	if info.OverlayMerged == "" {
		return
	}

	shortID := info.ID
	if len(shortID) > 12 {
		shortID = shortID[:12]
	}
	containerDir := filepath.Join(containerDataDir, shortID)
	os.RemoveAll(containerDir)
}

func cleanupCgroup(cgroupName string) {
	if cgroupName == "" {
		return
	}
	subsystems := []string{"memory", "cpu", "freezer"}
	for _, subsys := range subsystems {
		cgroupPath := filepath.Join("/sys/fs/cgroup", subsys, cgroupName)
		if _, err := os.Stat(cgroupPath); err == nil {
			os.RemoveAll(cgroupPath)
		}
	}
}

func generateContainerID() string {
	return fmt.Sprintf("%d", time.Now().UnixNano())
}

func getContainerInfoPath(containerID string) string {
	if len(containerID) > 12 {
		containerID = containerID[:12]
	}
	return filepath.Join(containerStoreDir, containerID+".json")
}

func saveContainerInfo(info *ContainerInfo) error {
	if err := os.MkdirAll(containerStoreDir, 0755); err != nil {
		return err
	}

	data, err := json.MarshalIndent(info, "", "  ")
	if err != nil {
		return err
	}

	return os.WriteFile(getContainerInfoPath(info.ID), data, 0644)
}

func loadContainerInfo(containerID string) (*ContainerInfo, error) {
	if err := os.MkdirAll(containerStoreDir, 0755); err != nil {
		return nil, err
	}

	entries, err := os.ReadDir(containerStoreDir)
	if err != nil {
		return nil, fmt.Errorf("容器 %s 不存在", containerID)
	}

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}

		fullPath := filepath.Join(containerStoreDir, entry.Name())
		data, err := os.ReadFile(fullPath)
		if err != nil {
			continue
		}

		var c ContainerInfo
		if err := json.Unmarshal(data, &c); err != nil {
			continue
		}

		if strings.HasPrefix(c.ID, containerID) || c.Name == containerID {
			return &c, nil
		}
	}

	return nil, fmt.Errorf("容器 %s 不存在", containerID)
}

func cleanupContainerNetwork(info *ContainerInfo) {
	if info.VethHost != "" {
		cmd := exec.Command("ip", "link", "delete", info.VethHost)
		_ = cmd.Run()
	}

	if info.PortMap != "" && info.ContainerIP != "" {
		cleanupPortMapping(info.PortMap, info.ContainerIP)
	}

	if info.NetworkName != "" && info.ContainerIP != "" {
		releaseContainerIP(info.NetworkName, info.ContainerIP)
	}
}

func cleanupPortMapping(portMap string, containerIP string) {
	parts := strings.Split(portMap, ":")
	if len(parts) != 2 {
		return
	}
	hostPort := parts[0]
	containerPort := parts[1]

	cmd := exec.Command("iptables", "-t", "nat", "-D", "PREROUTING",
		"-p", "tcp", "--dport", hostPort,
		"-j", "DNAT", "--to-destination", containerIP+":"+containerPort)
	_ = cmd.Run()

	cmd = exec.Command("iptables", "-t", "nat", "-D", "OUTPUT",
		"-p", "tcp", "--dport", hostPort,
		"-j", "DNAT", "--to-destination", containerIP+":"+containerPort)
	_ = cmd.Run()

	cmd = exec.Command("iptables", "-t", "nat", "-D", "POSTROUTING",
		"-p", "tcp", "-d", containerIP, "--dport", containerPort,
		"-j", "MASQUERADE")
	_ = cmd.Run()
}

func releaseContainerIP(networkName string, ip string) {
	networkStorePath := "/var/lib/mini-docker/networks"
	infoPath := filepath.Join(networkStorePath, networkName+".json")
	data, err := os.ReadFile(infoPath)
	if err != nil {
		return
	}

	var netInfo struct {
		Allocated []string `json:"allocated"`
	}
	if err := json.Unmarshal(data, &netInfo); err != nil {
		return
	}

	for i, allocated := range netInfo.Allocated {
		if allocated == ip {
			netInfo.Allocated = append(netInfo.Allocated[:i], netInfo.Allocated[i+1:]...)
			break
		}
	}

	updated, err := json.MarshalIndent(netInfo, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(infoPath, updated, 0644)
}
