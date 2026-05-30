package containerinit

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"mini-docker/libcontainer"
	"mini-docker/libcontainer/configs"
	"mini-docker/spec"
	"mini-docker/types"
	"mini-docker/utils"
)

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

	// CreateContainer Hooks: 在容器命名空间内、pivot_root 之前执行
	// OCI 规范: "after the container has been created but before pivot_root or any equivalent operation has been called"
	if ociSpec.Hooks != nil && len(ociSpec.Hooks.CreateContainer) > 0 {
		hookState := &libcontainer.HookState{
			OCIVersion:  ociSpec.OCIVersion,
			ID:          filepath.Base(bundlePath),
			Status:      "creating",
			Pid:         os.Getpid(),
			Bundle:      bundlePath,
			Annotations: ociSpec.Annotations,
		}
		if err := libcontainer.RunHooks(ociSpec.Hooks.CreateContainer, hookState); err != nil {
			fmt.Fprintf(os.Stderr, "CreateContainer hook 失败: %v\n", err)
			os.Exit(1)
		}
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
		if _, err := pipe.Write([]byte("ready\n")); err != nil { //发送 ready 信号给 runtime create 进程
			fmt.Fprintf(os.Stderr, "发送 ready 信号失败: %v\n", err)
		}
		pipe.Close()
	}
	//runtime start 唯一做的事情就是向 FIFO 写入一个字节，唤醒阻塞在 io.ReadFull(fifo) 上的 init 进程。
	//init 进程被唤醒后才会执行 syscallExec 替换为用户的 cmd 命令。
	//在 runtime start 之前，init 进程虽然已经存在（PID 已分配、namespace 已隔离、rootfs 已设置），但它被"冻结"在 FIFO 读取上，不会执行任何用户代码。
	//这正是 create/start 分离的核心价值——在"冻结"期间，Daemon 可以安全地配置网络、设置安全策略等
	_, _ = io.ReadFull(fifo, make([]byte, 1)) // 阻塞！等 FIFO 里有数据,只有runtime start进程执行后,向".start-fifo"管道中写了数据,容器init进程才会向下执行容器中的cmd命令(容器第一条命令)
	fifo.Close()

	// StartContainer Hooks: 在容器命名空间内执行，用户进程 execve 之前
	if ociSpec.Hooks != nil && len(ociSpec.Hooks.StartContainer) > 0 {
		hookState := &libcontainer.HookState{
			OCIVersion:  ociSpec.OCIVersion,
			ID:          filepath.Base(bundlePath),
			Status:      "created",
			Pid:         os.Getpid(),
			Bundle:      bundlePath,
			Annotations: ociSpec.Annotations,
		}
		if err := libcontainer.RunHooks(ociSpec.Hooks.StartContainer, hookState); err != nil {
			fmt.Fprintf(os.Stderr, "StartContainer hook 失败: %v\n", err)
			os.Exit(1)
		}
	}
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

func applyOCICapabilities(caps *configs.Capabilities) {
	if caps == nil {
		ApplyCapabilitiesFromEnv()
		return
	}

	keepSet := make(map[int]bool)
	for _, c := range caps.Bounding {
		if capVal, err := configs.ResolveCapName(c); err == nil {
			keepSet[capVal] = true
		}
	}

	for _, capName := range configs.AllKnownCapabilities {
		capVal, err := configs.ResolveCapName(capName)
		if err != nil {
			continue
		}
		if !keepSet[capVal] {
			if err := utils.DropCapability(capVal); err != nil {
				fmt.Printf("  提示: drop CAP_%s 失败（可能不受支持）: %v\n",
					configs.CapValueToName[capVal], err)
			}
		}
	}
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
