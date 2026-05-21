//go:build linux

package libcontainer

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"

	"mini-docker/libcontainer/configs"

	"golang.org/x/sys/unix"
)

// InitProcess 容器 init 进程逻辑（对标 libcontainer/init_linux.go）
// 这个函数在容器的 init 进程中执行（由 runtime create fork 出来）
func InitProcess(bundlePath string, fifoPath string) error {
	// 锁定 OS 线程，确保 namespace 操作在正确的线程上
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	// 1. 在 pivot_root 之前打开 FIFO（因为 pivot_root 会改变根目录，FIFO 路径将不可访问）
	fifo, err := os.OpenFile(fifoPath, os.O_RDONLY, 0)
	if err != nil {
		return fmt.Errorf("打开 FIFO 失败 (必须在 pivot_root 前): %w", err)
	}

	// 2. 加载容器配置
	configPath := filepath.Join(bundlePath, "config.json")
	data, err := os.ReadFile(configPath)
	if err != nil {
		fifo.Close()
		return fmt.Errorf("读取容器配置失败: %w", err)
	}

	var config configs.Config
	if err := json.Unmarshal(data, &config); err != nil {
		fifo.Close()
		return fmt.Errorf("解析容器配置失败: %w", err)
	}

	// 3. 设置 rootfs（OverlayFS + pivot_root + mount proc/tmp/volumes）
	if err := setupRootfs(&config); err != nil {
		fifo.Close()
		return fmt.Errorf("设置 rootfs 失败: %w", err)
	}

	// 4. 设置主机名
	if config.Hostname != "" {
		if err := unix.Sethostname([]byte(config.Hostname)); err != nil {
			fifo.Close()
			return fmt.Errorf("设置主机名失败: %w", err)
		}
	}

	// 5. 应用 Capability 限制
	if config.Capabilities != nil {
		if err := applyCapabilities(config.Capabilities); err != nil {
			fifo.Close()
			return fmt.Errorf("应用 Capability 失败: %w", err)
		}
	}

	// 6. 屏蔽危险路径（安全特性）
	for _, path := range config.MaskedPaths {
		maskPath(path)
	}
	for _, path := range config.ReadonlyPaths {
		readonlyPath(path)
	}

	// 7. 发送 ready 信号给父进程（runtime create）
	pipe := os.NewFile(InitPipeFd, "ready-pipe")
	if pipe != nil {
		pipe.Write([]byte("ready\n"))
		pipe.Close()
	}

	// 8. 等待 start 信号（使用之前打开的 FIFO fd，此时 pivot_root 已完成）
	buf := make([]byte, 16)
	_, err = fifo.Read(buf)
	fifo.Close()
	if err != nil {
		return fmt.Errorf("等待 start 信号失败: %w", err)
	}

	// 9. 执行用户命令
	if len(config.Args) > 0 {
		if config.Cwd != "" {
			if err := os.Chdir(config.Cwd); err != nil {
				return fmt.Errorf("切换工作目录失败: %w", err)
			}
		}
		if len(config.Env) > 0 {
			os.Clearenv()
			for _, env := range config.Env {
				parts := strings.SplitN(env, "=", 2)
				if len(parts) == 2 {
					os.Setenv(parts[0], parts[1])
				}
			}
		}
		if err := syscall.Exec(config.Args[0], config.Args, os.Environ()); err != nil {
			return fmt.Errorf("exec 用户命令失败: %w", err)
		}
	}

	return nil
}

// setupRootfs 设置容器根文件系统（对标 libcontainer/rootfs_linux.go）
func setupRootfs(config *configs.Config) error {
	// 1. 将当前根目录设为私有（切断与宿主机的 mount 传播）
	if err := unix.Mount("", "/", "", unix.MS_PRIVATE|unix.MS_REC, ""); err != nil {
		return fmt.Errorf("重挂载 / 为 private 失败: %w", err)
	}

	// 2. 检测是否有 overlay 配置（从注解中获取）
	overlayMerged := ""
	overlayUpper := ""
	overlayWork := ""

	// 尝试从环境变量读取 overlay 配置（兼容旧模式）
	if overlayMerged == "" {
		overlayMerged = os.Getenv("MINI_DOCKER_OVERLAY_MERGED")
		overlayUpper = os.Getenv("MINI_DOCKER_OVERLAY_UPPER")
		overlayWork = os.Getenv("MINI_DOCKER_OVERLAY_WORK")
	}

	// 3. 挂载 rootfs
	var targetPath string
	if overlayMerged != "" {
		options := fmt.Sprintf("lowerdir=%s,upperdir=%s,workdir=%s",
			config.Rootfs, overlayUpper, overlayWork)
		if err := unix.Mount("overlay", overlayMerged, "overlay", 0, options); err != nil {
			// 回退到 bind mount
			if err := unix.Mount(config.Rootfs, config.Rootfs, "bind", unix.MS_BIND|unix.MS_REC, ""); err != nil {
				return fmt.Errorf("绑定挂载 rootfs 失败: %w", err)
			}
			targetPath = config.Rootfs
		} else {
			targetPath = overlayMerged
		}
	} else {
		if err := unix.Mount(config.Rootfs, config.Rootfs, "bind", unix.MS_BIND|unix.MS_REC, ""); err != nil {
			return fmt.Errorf("绑定挂载 rootfs 失败: %w", err)
		}
		targetPath = config.Rootfs
	}

	// 4. pivot_root（切换根目录）
	pivotDir := filepath.Join(targetPath, ".pivot_root")
	if err := os.MkdirAll(pivotDir, 0755); err != nil {
		return fmt.Errorf("创建 pivot_root 目录失败: %w", err)
	}
	if err := pivotRoot(targetPath, pivotDir); err != nil {
		return fmt.Errorf("pivot_root 失败: %w", err)
	}

	// 5. 挂载 /proc
	if err := mountProc(); err != nil {
		return fmt.Errorf("挂载 /proc 失败: %w", err)
	}

	// 6. 挂载 /tmp
	if err := mountTmp(); err != nil {
		return fmt.Errorf("挂载 /tmp 失败: %w", err)
	}

	// 7. 挂载额外的挂载点（Volume 等）
	for _, m := range config.Mounts {
		if err := mountExtra(m); err != nil {
			fmt.Printf("警告: 挂载 %s 失败: %v\n", m.Destination, err)
		}
	}

	// 8. 挂载 Volume（从环境变量读取，兼容旧模式）
	mountVolumesFromEnv()

	return nil
}

// pivotRoot 切换根目录
func pivotRoot(newRoot string, putOld string) error {
	if err := unix.PivotRoot(newRoot, putOld); err != nil {
		return fmt.Errorf("pivot_root 系统调用失败: %w", err)
	}

	if err := unix.Chdir("/"); err != nil {
		return fmt.Errorf("chdir / 失败: %w", err)
	}

	putOldDir := filepath.Join("/", filepath.Base(putOld))
	if err := unix.Unmount(putOldDir, unix.MNT_DETACH); err != nil {
		return fmt.Errorf("卸载旧根失败: %w", err)
	}

	return os.RemoveAll(putOldDir)
}

// mountProc 挂载 /proc
func mountProc() error {
	if err := os.MkdirAll("/proc", 0755); err != nil {
		return err
	}
	return unix.Mount("proc", "/proc", "proc", 0, "")
}

// mountTmp 挂载 /tmp
func mountTmp() error {
	if err := os.MkdirAll("/tmp", 0755); err != nil {
		return err
	}
	return unix.Mount("tmpfs", "/tmp", "tmpfs", unix.MS_NOSUID|unix.MS_NODEV, "size=64m")
}

// mountExtra 挂载额外的挂载点
func mountExtra(m *configs.Mount) error {
	if err := os.MkdirAll(m.Destination, 0755); err != nil {
		return err
	}

	flags := m.Flags
	if flags == 0 {
		flags = unix.MS_BIND | unix.MS_REC
	}

	return unix.Mount(m.Source, m.Destination, m.Type, flags, "")
}

// mountVolumesFromEnv 从环境变量挂载 Volume
func mountVolumesFromEnv() {
	volumesStr := os.Getenv("MINI_DOCKER_VOLUMES")
	if volumesStr == "" {
		return
	}

	volumeSpecs := strings.Split(volumesStr, ",")
	for _, spec := range volumeSpecs {
		parts := strings.Split(spec, ":")
		if len(parts) < 2 {
			continue
		}

		hostPath := parts[0]
		containerPath := parts[1]
		readOnly := len(parts) >= 3 && parts[2] == "ro"

		if err := os.MkdirAll(containerPath, 0755); err != nil {
			continue
		}

		flags := unix.MS_BIND | unix.MS_REC
		if readOnly {
			flags |= unix.MS_RDONLY
		}

		if err := unix.Mount(hostPath, containerPath, "bind", uintptr(flags), ""); err != nil {
			continue
		}

		if readOnly {
			unix.Mount(hostPath, containerPath, "bind", uintptr(flags|unix.MS_REMOUNT), "")
		}
	}
}

// applyCapabilities 应用 Capability 限制
func applyCapabilities(caps *configs.Capabilities) error {
	if caps == nil {
		return nil
	}

	// 计算需要 drop 的 capability
	keepSet := make(map[int]bool)
	for _, name := range caps.Bounding {
		name = strings.TrimPrefix(name, "CAP_")
		if val, ok := configs.CapNameToValue[name]; ok {
			keepSet[val] = true
		}
	}

	// Drop 不在 keepSet 中的 capability
	for _, capName := range configs.AllKnownCapabilities {
		capName = strings.TrimPrefix(capName, "CAP_")
		val, ok := configs.CapNameToValue[capName]
		if !ok {
			continue
		}
		if !keepSet[val] {
			dropCapability(val)
		}
	}

	return nil
}

// dropCapability 从能力边界集中移除一个能力
func dropCapability(cap int) error {
	_, _, errno := syscall.Syscall6(
		unix.SYS_PRCTL,
		unix.PR_CAPBSET_DROP,
		uintptr(cap),
		0, 0, 0, 0,
	)
	if errno != 0 {
		return fmt.Errorf("prctl(PR_CAPBSET_DROP, %d) 失败: %v", cap, errno)
	}
	return nil
}

// maskPath 屏蔽路径（挂载空文件覆盖）
func maskPath(path string) {
	if err := unix.Mount("/dev/null", path, "", unix.MS_BIND, ""); err != nil {
		// 忽略错误，路径可能不存在
	}
}

// readonlyPath 设置路径为只读
func readonlyPath(path string) {
	if err := unix.Mount(path, path, "bind", unix.MS_BIND|unix.MS_REMOUNT|unix.MS_RDONLY, ""); err != nil {
		// 忽略错误，路径可能不存在
	}
}

// IsInitProcess 检测是否是 init 进程
func IsInitProcess() bool {
	return len(os.Args) >= 2 && os.Args[1] == "init"
}

// HandleInit 处理 init 进程
func HandleInit() error {
	// 解析参数
	bundlePath := ""
	fifoPath := ""

	for i := 2; i < len(os.Args)-1; i++ {
		switch os.Args[i] {
		case "--bundle":
			bundlePath = os.Args[i+1]
		case "--fifo":
			fifoPath = os.Args[i+1]
		}
	}

	if bundlePath == "" || fifoPath == "" {
		return fmt.Errorf("init: 缺少 --bundle 或 --fifo 参数")
	}

	return InitProcess(bundlePath, fifoPath)
}
