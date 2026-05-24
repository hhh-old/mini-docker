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

	"mini-docker/container"
	"mini-docker/libcontainer/configs"
	"mini-docker/types"
	"mini-docker/utils"

	"golang.org/x/sys/unix"
)

// InitProcess 容器 init 进程逻辑（对标 libcontainer/init_linux.go）
// 这个函数在容器的 init 进程中执行（由 runtime create fork 出来）
func InitProcess(bundlePath string, fifoPath string) error {
	// 锁定 OS 线程，确保 namespace 操作在正确的线程上
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	// 1. 在 pivot_root 之前打开 FIFO（因为 pivot_root 会改变根目录，FIFO 路径将不可访问）
	// 使用 O_RDWR 打开避免阻塞（O_RDONLY 会阻塞直到有 writer，导致死锁）
	fifo, err := os.OpenFile(fifoPath, os.O_RDWR, 0)
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
		if _, err := pipe.Write([]byte("ready\n")); err != nil {
			pipe.Close()
			return fmt.Errorf("发送 ready 信号失败: %w", err)
		}
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
	overlayMerged := os.Getenv("MINI_DOCKER_OVERLAY_MERGED")
	overlayUpper := os.Getenv("MINI_DOCKER_OVERLAY_UPPER")
	overlayWork := os.Getenv("MINI_DOCKER_OVERLAY_WORK")

	var overlay *types.OverlayDirs
	if overlayMerged != "" {
		overlay = &types.OverlayDirs{
			Merged: overlayMerged,
			Upper:  overlayUpper,
			Work:   overlayWork,
		}
	}

	if err := container.SetupRootFS(config.Rootfs, overlay); err != nil {
		return fmt.Errorf("设置 rootfs 失败: %w", err)
	}

	for _, m := range config.Mounts {
		if err := mountExtra(m); err != nil {
			fmt.Printf("警告: 挂载 %s 失败: %v\n", m.Destination, err)
		}
	}

	return nil
}

func mountExtra(m *configs.Mount) error {
	if err := os.MkdirAll(m.Destination, 0755); err != nil {
		return err
	}

	flags := m.Flags
	data := ""

	if m.Type != "" && m.Type != "bind" {
		data = strings.Join(m.Options, ",")
	} else {
		if flags == 0 {
			flags = unix.MS_BIND | unix.MS_REC
		}
	}

	return unix.Mount(m.Source, m.Destination, m.Type, uintptr(flags), data)
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
			if err := utils.DropCapability(val); err != nil {
				return fmt.Errorf("丢弃 capability %s 失败: %w", capName, err)
			}
		}
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
