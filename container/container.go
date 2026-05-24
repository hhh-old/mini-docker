package container

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"mini-docker/constants"
	"mini-docker/network"
	"mini-docker/spec"
	"mini-docker/types"
	"mini-docker/utils"
)

const (
	containerStoreDir = constants.ContainerStoreDir
	containerDataDir  = constants.ContainerDataDir
	imageStoreDir     = constants.ImageStoreDir
)

type ContainerInfo struct {
	ID             string   `json:"id"`
	Name           string   `json:"name"`
	Pid            int      `json:"pid"`
	Image          string   `json:"image"`
	Cmd            []string `json:"cmd"`
	Status         string   `json:"status"` // created, running, paused, exited, stopped, dead, restarting
	CreatedAt      string   `json:"created_at"`
	RootFS         string   `json:"rootfs"`
	CgroupName     string   `json:"cgroup_name"`
	NetworkName    string   `json:"network_name"`
	VethHost       string   `json:"veth_host"`
	ContainerIP    string   `json:"container_ip"`
	PortMap        string   `json:"port_map"`
	OverlayMerged  string   `json:"overlay_merged"`
	OverlayUpper   string   `json:"overlay_upper"`
	OverlayWork    string   `json:"overlay_work"`
	RestartPolicy  string   `json:"restart_policy"` // no, always, on-failure
	Tty            bool     `json:"tty"`
	ExitCode       int      `json:"exit_code"`
	FinishedAt     string   `json:"finished_at"`
	Volumes        []string `json:"volumes"` // 记录容器的 volume 挂载
	HealthCmd      string   `json:"health_cmd"`
	HealthInterval string   `json:"health_interval"`
	HealthTimeout  string   `json:"health_timeout"`
	HealthRetries  int      `json:"health_retries"`
	MemoryLimit    string   `json:"memory_limit"`
	CPUShares      string   `json:"cpu_shares"`
}

type CgroupManager interface {
	Apply(pid int) error
	Destroy() error
	Freeze() error
	Thaw() error
}

type NetworkManager interface {
	Connect(pid int) error
	Disconnect() error
	GetVethHost() string
	GetContainerIP() string
}

func Exec(containerID string, cmd []string) error {
	containerInfo, err := loadContainerInfo(containerID)
	if err != nil {
		return fmt.Errorf("查找容器失败: %w", err)
	}

	if containerInfo.Status != constants.StatusRunning {
		return fmt.Errorf("容器 %s 未在运行", containerID)
	}

	// 使用 nsenter 命令进入容器的所有 namespace 执行命令
	// 原因：Go 运行时是多线程的，直接调用 setns() 加入 mount namespace
	// 会返回 EINVAL（setns 要求调用进程单线程）
	// Docker 的做法也是通过 nsenter 或 fork 子进程后再 setns
	//
	// nsenter 参数说明：
	//   -t <pid>   → 目标进程 PID
	//   -m         → 进入 mount namespace
	//   -u         → 进入 UTS namespace
	//   -i         → 进入 IPC namespace
	//   -n         → 进入 network namespace
	//   -p         → 进入 PID namespace
	//   --         → 后面是要执行的命令
	nsenterCmd := exec.Command("nsenter",
		"-t", fmt.Sprintf("%d", containerInfo.Pid),
		"-m", "-u", "-i", "-n", "-p",
		"--",
	)
	nsenterCmd.Args = append(nsenterCmd.Args, cmd...)

	nsenterCmd.Stdin = os.Stdin
	nsenterCmd.Stdout = os.Stdout
	nsenterCmd.Stderr = os.Stderr

	return nsenterCmd.Run()
}

func Stop(containerID string) error {
	containerInfo, err := loadContainerInfo(containerID)
	if err != nil {
		return fmt.Errorf("查找容器失败: %w", err)
	}

	if containerInfo.Status != constants.StatusRunning && containerInfo.Status != constants.StatusPaused {
		return fmt.Errorf("容器 %s 未在运行", containerID)
	}

	if containerInfo.Pid <= 0 {
		containerInfo.Status = constants.StatusExited
		containerInfo.FinishedAt = utils.NowFormatted()
		SaveContainerInfo(containerInfo)
		return fmt.Errorf("容器 %s PID 无效，状态已更新为 exited", containerID)
	}

	if err := utils.GracefulStopProcess(
		func(sig syscall.Signal) error { return sendSignal(containerInfo.Pid, int(sig)) },
		func() bool { return utils.CheckProcessAlive(containerInfo.Pid) == nil },
	); err != nil {
		return err
	}

	containerInfo.Status = constants.StatusExited
	containerInfo.ExitCode = constants.SIGTERMExitCode
	containerInfo.FinishedAt = utils.NowFormatted()
	containerInfo.Pid = 0
	cleanupContainerNetwork(containerInfo)
	cleanupCgroup(containerInfo.CgroupName)
	return SaveContainerInfo(containerInfo)
}

func Pause(containerID string, cg CgroupManager) error {
	containerInfo, err := loadContainerInfo(containerID)
	if err != nil {
		return fmt.Errorf("查找容器失败: %w", err)
	}

	if containerInfo.Status != constants.StatusRunning {
		return fmt.Errorf("容器 %s 未在运行", containerID)
	}

	if err := cg.Freeze(); err != nil {
		return fmt.Errorf("冻结容器失败: %w", err)
	}

	containerInfo.Status = constants.StatusPaused
	return SaveContainerInfo(containerInfo)
}

func Unpause(containerID string, cg CgroupManager) error {
	containerInfo, err := loadContainerInfo(containerID)
	if err != nil {
		return fmt.Errorf("查找容器失败: %w", err)
	}

	if containerInfo.Status != constants.StatusPaused {
		return fmt.Errorf("容器 %s 未处于暂停状态", containerID)
	}

	if err := cg.Thaw(); err != nil {
		return fmt.Errorf("恢复容器失败: %w", err)
	}

	containerInfo.Status = constants.StatusRunning
	return SaveContainerInfo(containerInfo)
}

func Remove(containerID string) error {
	containerInfo, err := loadContainerInfo(containerID)
	if err != nil {
		return fmt.Errorf("查找容器失败: %w", err)
	}

	if containerInfo.Status == constants.StatusRunning || containerInfo.Status == constants.StatusPaused || containerInfo.Status == constants.StatusRestarting {
		return fmt.Errorf("容器 %s 正在运行（状态: %s），请先停止容器", containerID, containerInfo.Status)
	}

	cleanupContainerNetwork(containerInfo)
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

		if c.Status == constants.StatusRunning {
			if err := utils.CheckProcessAlive(c.Pid); err != nil {
				c.Status = constants.StatusStopped
				_ = SaveContainerInfo(&c)
			}
		}

		containers = append(containers, &c)
	}

	return containers, nil
}

func HandleInit() {
	HandleOCIInit()
}

func IsInitProcess() bool {
	return len(os.Args) >= 2 && os.Args[1] == "init"
}

func HandleOCIInit() {
	bundlePath := getFlagValue("--bundle")
	fifoPath := getFlagValue("--fifo")

	if bundlePath == "" || fifoPath == "" {
		fmt.Fprintf(os.Stderr, "OCI init: 缺少 --bundle 或 --fifo 参数\n")
		os.Exit(1)
	}

	ociSpec, err := spec.LoadSpec(bundlePath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "OCI init: 加载 Spec 失败: %v\n", err)
		os.Exit(1)
	}

	overlay := extractOverlayFromAnnotations(ociSpec)

	// FIFO 在 bundle 目录中（宿主机文件系统），必须在 pivot_root 之前打开
	// pivot_root 后根目录切换，bundle 路径将不可访问
	// 使用 O_RDWR 打开避免阻塞（O_RDONLY 会阻塞直到有 writer，导致死锁）
	fifo, err := os.OpenFile(fifoPath, os.O_RDWR, 0)
	if err != nil {
		fmt.Fprintf(os.Stderr, "OCI init: 打开 FIFO 失败: %v\n", err)
		os.Exit(1)
	}

	if err := SetupRootFS(ociSpec.Root.Path, overlay); err != nil {
		fmt.Fprintf(os.Stderr, "OCI init: 设置 rootfs 失败: %v\n", err)
		os.Exit(1)
	}

	hostname := ociSpec.Hostname
	if hostname == "" {
		hostname = "mini-docker"
	}
	if err := setHostname(hostname); err != nil {
		fmt.Fprintf(os.Stderr, "OCI init: 设置主机名失败: %v\n", err)
		os.Exit(1)
	}

	applyOCICapabilities(ociSpec.Process.Capabilities)

	pipe := os.NewFile(3, "ready-pipe") //runtime create进程传递的pipeWrite管道就是fd3
	if pipe != nil {
		if _, err := pipe.Write([]byte("ready\n")); err != nil { //发送 ready 信号
			fmt.Fprintf(os.Stderr, "发送 ready 信号失败: %v\n", err)
		}
		pipe.Close()
	}

	buf := make([]byte, 16)
	fifo.Read(buf) //阻塞等待 FIFO 的 "start" 信号,等待 runtime start 命令写入 "start\n".
	fifo.Close()

	args := ociSpec.Process.Args
	if len(args) == 0 {
		fmt.Fprintf(os.Stderr, "OCI init: process.args 为空\n")
		os.Exit(1)
	}

	env := ociSpec.Process.Env
	if len(env) == 0 {
		env = os.Environ()
	}

	execPath := args[0]            //ociSpec.Process.Args的第一个参数是要执行的命令
	if !filepath.IsAbs(execPath) { //如果不是绝对路径，把相对路径的命令名解析为绝对路径
		resolved, err := lookPathInEnv(execPath, env) // 在 PATH 环境变量中查找
		if err != nil {
			fmt.Fprintf(os.Stderr, "OCI init: 找不到命令 %s: %v\n", execPath, err)
			os.Exit(1)
		}
		execPath = resolved
	}
	//进程替换（syscallExec）,调用底层的 execve 系统调用，替换进程执行的程序
	//execPath := args[0]      // 第一个参数：可执行文件的绝对路径
	//args := ociSpec.Process.Args  // 第二个参数：完整参数列表（包含 argv[0]）
	//env := ociSpec.Process.Env    // 第三个参数：环境变量
	//例如：syscall.Exec("/bin/sleep", ["sleep", "9999"], ["PATH=/bin:/usr/bin", ...])
	//将容器内PID为1的进程（mini-docker init，也就是创建这个隔离namespace的进程）的执行程序替换为execPath程序，也就是容器中要执行的命令
	if err := syscallExec(execPath, args, env); err != nil {
		fmt.Fprintf(os.Stderr, "OCI init: exec 失败: %v\n", err)
		os.Exit(1)
	}
}

func lookPathInEnv(file string, env []string) (string, error) {
	pathEnv := ""
	for _, e := range env {
		if strings.HasPrefix(e, "PATH=") {
			pathEnv = strings.TrimPrefix(e, "PATH=")
			break
		}
	}
	if pathEnv == "" {
		return "", fmt.Errorf("PATH 环境变量未设置")
	}
	for _, dir := range filepath.SplitList(pathEnv) {
		if dir == "" {
			dir = "."
		}
		fullPath := filepath.Join(dir, file)
		if err := isExecutable(fullPath); err == nil {
			return fullPath, nil
		}
	}
	return "", fmt.Errorf("在 PATH 中未找到 %s", file)
}

func isExecutable(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if info.IsDir() {
		return fmt.Errorf("是目录")
	}
	if info.Mode()&0111 == 0 {
		return fmt.Errorf("不可执行")
	}
	return nil
}

func extractOverlayFromAnnotations(s *spec.Spec) *types.OverlayDirs {
	if s == nil || s.Annotations == nil {
		return nil
	}
	merged := s.Annotations["mini-docker.overlay.merged"]
	upper := s.Annotations["mini-docker.overlay.upper"]
	work := s.Annotations["mini-docker.overlay.work"]
	if merged == "" {
		return nil
	}
	return &types.OverlayDirs{
		Merged: merged,
		Upper:  upper,
		Work:   work,
	}
}

func applyOCICapabilities(caps *spec.Capabilities) {
	if caps == nil {
		ApplyCapabilitiesFromEnv()
		return
	}
	var capDrop []string
	allCaps := []string{
		"CHOWN", "DAC_OVERRIDE", "DAC_READ_SEARCH", "FOWNER", "FSETID", "KILL",
		"SETGID", "SETUID", "SETPCAP", "LINUX_IMMUTABLE", "NET_BIND_SERVICE",
		"NET_BROADCAST", "NET_ADMIN", "NET_RAW", "IPC_LOCK", "IPC_OWNER",
		"SYS_MODULE", "SYS_RAWIO", "SYS_CHROOT", "SYS_PTRACE", "SYS_PACCT",
		"SYS_ADMIN", "SYS_BOOT", "SYS_NICE", "SYS_RESOURCE", "SYS_TIME",
		"SYS_TTY_CONFIG", "MKNOD", "LEASE", "AUDIT_WRITE", "AUDIT_CONTROL",
		"SETFCAP",
	}
	keepSet := make(map[string]bool)
	for _, c := range caps.Bounding {
		keepSet[strings.TrimPrefix(c, "CAP_")] = true
	}
	for _, c := range allCaps {
		if !keepSet[c] {
			capDrop = append(capDrop, c)
		}
	}
	if len(capDrop) > 0 {
		ApplyCapabilities(nil, capDrop)
	}
}

// 宿主机上
func CreateOverlayDirs(containerID string) (*types.OverlayDirs, error) {
	shortID := utils.FormatShortID(containerID)

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

	return &types.OverlayDirs{
		Merged: mergedDir,
		Upper:  upperDir,
		Work:   workDir,
	}, nil
}

func cleanupOverlay(info *ContainerInfo) {
	if info.OverlayMerged == "" {
		return
	}

	shortID := utils.FormatShortID(info.ID)
	containerDir := filepath.Join(containerDataDir, shortID)
	os.RemoveAll(containerDir)
}

func cleanupCgroup(cgroupName string) {
	if cgroupName == "" {
		return
	}
	subsystems := []string{"memory", "cpu", "freezer"}
	for _, subsys := range subsystems {
		cgroupPath := filepath.Join(constants.CgroupRootPath, subsys, cgroupName)
		if _, err := os.Stat(cgroupPath); err == nil {
			os.RemoveAll(cgroupPath)
		}
	}
}

func generateContainerID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

func getContainerInfoPath(containerID string) string {
	return filepath.Join(containerStoreDir, utils.FormatShortID(containerID)+".json")
}

func SaveContainerInfo(info *ContainerInfo) error {
	if err := os.MkdirAll(containerStoreDir, 0755); err != nil {
		return err
	}

	data, err := json.MarshalIndent(info, "", "  ")
	if err != nil {
		return err
	}

	return os.WriteFile(getContainerInfoPath(info.ID), data, 0644)
}

func RemoveContainerInfo(containerID string) error {
	return os.Remove(getContainerInfoPath(containerID))
}

func LoadContainerInfoByID(containerID string) (*ContainerInfo, error) {
	return loadContainerInfo(containerID)
}

func CleanupContainerNetwork(info *ContainerInfo) {
	cleanupContainerNetwork(info)
}

func CleanupOverlay(info *ContainerInfo) {
	cleanupOverlay(info)
}

func CleanupCgroup(cgroupName string) {
	cleanupCgroup(cgroupName)
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
	vethExisted := false
	if info.VethHost != "" {
		if _, err := os.Stat(fmt.Sprintf("/sys/class/net/%s", info.VethHost)); err == nil {
			vethExisted = true
		}
		cmd := exec.Command("ip", "link", "delete", info.VethHost)
		_ = cmd.Run()
	}

	if info.PortMap != "" && info.ContainerIP != "" {
		utils.CleanupPortMapping(info.PortMap, info.ContainerIP)
	}

	if info.NetworkName != "" && info.ContainerIP != "" {
		network.ReleaseIP(info.NetworkName, info.ContainerIP)
	}

	if vethExisted && info.NetworkName != "" {
		if netInfo, err := network.LoadNetworkInfo(info.NetworkName); err == nil {
			if len(netInfo.Allocated) == 0 {
				network.CleanupMasquerade(netInfo.Subnet, netInfo.Bridge)
			}
		}
	}
}

// ReadContainerLogs 读取容器日志（对齐 Docker 的 json-log 格式）
func ReadContainerLogs(containerID string) ([]string, error) {
	shimLogPath := filepath.Join(constants.ShimDir, containerID, "container.log")
	data, err := os.ReadFile(shimLogPath)
	if err != nil {
		entries, _ := os.ReadDir(constants.ShimDir)
		for _, e := range entries {
			if e.IsDir() && strings.HasPrefix(e.Name(), containerID) {
				shimLogPath = filepath.Join(constants.ShimDir, e.Name(), "container.log")
				data, err = os.ReadFile(shimLogPath)
				if err == nil {
					break
				}
			}
		}
	}
	if err != nil {
		return nil, fmt.Errorf("读取日志失败: %w", err)
	}

	var lines []string
	for _, line := range strings.Split(string(data), "\n") {
		if line == "" {
			continue
		}
		var entry map[string]string
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			lines = append(lines, line)
			continue
		}
		if logMsg, ok := entry["log"]; ok {
			lines = append(lines, logMsg)
		}
	}

	return lines, nil
}

func findFlagIndex(flag string) int {
	for i, arg := range os.Args {
		if arg == flag {
			return i
		}
	}
	return -1
}

func getFlagValue(flag string) string {
	idx := findFlagIndex(flag)
	if idx >= 0 && idx+1 < len(os.Args) {
		return os.Args[idx+1]
	}
	return ""
}
