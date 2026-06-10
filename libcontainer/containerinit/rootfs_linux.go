//go:build linux

package containerinit

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"mini-docker/constants"

	"golang.org/x/sys/unix"
)

// SetupRootFS 设置容器根文件系统
// 对齐 containerd/runc: overlay mount 已在宿主机上完成，
// Root.Path 指向已挂载的 overlay merged 目录（真正的文件系统），
// 此处只需 bind mount + pivot_root 切换根目录
func SetupRootFS(rootFSPath string) error {
	// 切断 mount namespace 和宿主机的联系
	// 容器内部的 mount/umount 不会影响宿主机
	if err := unix.Mount("", "/", "", unix.MS_PRIVATE|unix.MS_REC, ""); err != nil {
		return fmt.Errorf("重新挂载 / 为 private 失败: %w", err)
	}

	// bind mount rootfs 使其成为挂载点（pivot_root 要求 newRoot 必须是挂载点）
	if err := unix.Mount(rootFSPath, rootFSPath, "bind", unix.MS_BIND|unix.MS_REC, ""); err != nil {
		return fmt.Errorf("绑定挂载 rootfs 失败: %w", err)
	}

	pivotDir := filepath.Join(rootFSPath, ".pivot_root")
	if err := os.MkdirAll(pivotDir, 0755); err != nil {
		return fmt.Errorf("创建 pivot_root 目录失败: %w", err)
	}

	if err := mountVolumesIntoRootfs(rootFSPath); err != nil {
		fmt.Printf("  警告: 挂载卷失败: %v\n", err)
	}

	if err := pivotRoot(rootFSPath, pivotDir); err != nil {
		return fmt.Errorf("pivot_root 失败: %w", err)
	}

	if err := mountProc(); err != nil {
		return fmt.Errorf("挂载 proc 失败: %w", err)
	}

	if err := mountTmp(); err != nil {
		return fmt.Errorf("挂载 tmpfs 失败: %w", err)
	}

	return nil
}

func setHostname(name string) error {
	return unix.Sethostname([]byte(name))
}

func syscallExec(argv0 string, argv []string, envv []string) error {
	return unix.Exec(argv0, argv, envv)
}

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

	if err := os.RemoveAll(putOldDir); err != nil {
		return fmt.Errorf("删除旧根目录失败: %w", err)
	}

	return nil
}

func mountProc() error {
	if err := os.MkdirAll("/proc", 0755); err != nil {
		return err
	}
	return unix.Mount("proc", "/proc", "proc", 0, "")
}

func mountTmp() error {
	if err := os.MkdirAll("/tmp", 0755); err != nil {
		return err
	}
	return unix.Mount("tmpfs", "/tmp", "tmpfs", unix.MS_NOSUID|unix.MS_NODEV, constants.DefaultTmpfsSize)
}

// mountVolumes 挂载 Volume（bind mount）
// 对齐 Docker 的数据卷机制：
// - bind mount：mount --bind /host/path /container/path
// - named volume：与 bind mount 相同，源路径指向 /var/lib/mini-docker/volumes/<name>/_data
//
// Docker 的 Volume 本质就是 bind mount，容器退出后 Volume 数据不丢失。
// 而容器的 OverlayFS upper 层会被删除，所以非 Volume 的修改会丢失。
func mountVolumesIntoRootfs(rootfsPath string) error {
	volumesStr := os.Getenv("MINI_DOCKER_VOLUMES")
	if volumesStr == "" {
		return nil
	}

	volumeSpecs := strings.Split(volumesStr, ",")
	for _, spec := range volumeSpecs {
		parts := strings.Split(spec, ":")
		if len(parts) < 2 {
			fmt.Printf("  警告: 无效的卷挂载规格: %s\n", spec)
			continue
		}

		hostPath := parts[0]
		containerPath := parts[1]
		readOnly := len(parts) >= 3 && parts[2] == "ro"

		destInRootfs := filepath.Join(rootfsPath, containerPath)
		if err := os.MkdirAll(destInRootfs, 0755); err != nil {
			fmt.Printf("  警告: 创建容器挂载点 %s 失败: %v\n", containerPath, err)
			continue
		}

		flags := unix.MS_BIND | unix.MS_REC
		if readOnly {
			flags |= unix.MS_RDONLY
		}

		if err := unix.Mount(hostPath, destInRootfs, "bind", uintptr(flags), ""); err != nil {
			fmt.Printf("  警告: bind mount %s -> %s 失败: %v\n", hostPath, containerPath, err)
			continue
		}

		if readOnly {
			if err := unix.Mount(hostPath, destInRootfs, "bind", uintptr(flags|unix.MS_REMOUNT), ""); err != nil {
				fmt.Printf("  警告: 设置只读挂载 %s 失败: %v\n", containerPath, err)
			}
		}

		fmt.Printf("  卷已挂载: %s -> %s\n", hostPath, containerPath)
	}

	return nil
}
