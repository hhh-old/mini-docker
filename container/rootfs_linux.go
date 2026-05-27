//go:build linux

package container

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"mini-docker/constants"
	"mini-docker/types"

	"golang.org/x/sys/unix"
)

// 此时已经创建了namespace，整个函数逻辑在隔离的namespace中执行而不是在宿主机执行
func SetupRootFS(rootFSPath string, overlay *types.OverlayDirs) error {
	//Linux 的挂载事件默认是会在父子空间传播的。这一句告诉内核：“把当前根目录及其所有子挂载点设为私有”。这样一来，容器内部执行的任何 mount 或 umount 操作，都不会影响到宿主机
	//切断mount namespace和宿主机的联系

	//当你用 CLONE_NEWNS 创建出一个新的 Mount Namespace 时，Linux 内核并不会给你一个空荡荡的世界。
	//相反，内核非常体贴：它会把宿主机当前的整棵“挂载树（文件系统视图）”，完完整整地拷贝一份，塞进这个新的 Namespace 里。
	//所以，在容器进程刚启动的头零点几毫秒里，它所处的世界是这样的：
	//它眼里的根目录 /，依然是宿主机的根目录！
	//它能看到 /etc，能看到 /var，当然也能顺着路径找到你在宿主机上准备好的 /var/lib/mycontainer/overlay.Merged。
	//此时，容器和宿主机看到的目录结构是一模一样的。
	//那么，“隔离”体现在哪里？
	//既然刚进去的时候看到的东西一模一样，那 CLONE_NEWNS 的意义是什么？
	//意义在于：虽然初始视图一模一样，但从这一刻起，它们是两张独立的“图纸”了。
	//没有 Namespace 时：大家共用一张挂载点目录树图纸。你在某个目录下执行 mount，图纸被修改了，所有人都看到这个目录变了。
	//有了 Namespace 后：容器手里拿的是挂载点目录树复印件。当加上 MS_PRIVATE 声明后，容器在自己的复印件上画画（执行 mount overlay），宿主机手里的原稿绝对不会发生任何变化。
	if err := unix.Mount("", "/", "", unix.MS_PRIVATE|unix.MS_REC, ""); err != nil {
		return fmt.Errorf("重新挂载 / 为 private 失败: %w", err)
	}

	var targetPath string
	if overlay != nil {
		options := fmt.Sprintf("lowerdir=%s,upperdir=%s,workdir=%s",
			rootFSPath, overlay.Upper, overlay.Work)
		//挂载 OverlayFS
		//unix.Mount("overlay", overlay.Merged, "overlay", 0, options)
		//结合之前讲过的参数，翻译成大白话就是：
		//source ("overlay"): 随便填个名字，内核要求必须有，但不重要。
		//target (overlay.Merged): 挂载点，这是在宿主机硬盘上的一个真实存在的空目录。
		//fstype ("overlay"): 关键！告诉内核，我要召唤 OverlayFS（联合文件系统） 的魔法。
		//flags (0): 没有特殊的基础标志。
		//options: （极其重要）包含了 lowerdir=...,upperdir=...,workdir=...。
		//执行效果（OverlayFS 的魔法）：
		//内核会把 options 里指定的 lowerdir（只读的镜像层）和 upperdir（可读写的容器层）像三明治一样“压”在一起，然后投影（挂载）到 overlay.Merged 这个空目录上。
		//执行完这句后，原本空空如也的 overlay.Merged 目录里，就会奇迹般地出现一个完整的 Linux 系统文件树（有 bin, etc, var 等等）。
		//而且由于宿主机已经和namespace隔离，宿主机上看其overlay.Merged路径文件夹还是空
		if err := unix.Mount("overlay", overlay.Merged, "overlay", 0, options); err != nil {
			fmt.Printf("  警告: OverlayFS 挂载失败，回退到 bind mount: %v\n", err)
			if err := unix.Mount(rootFSPath, rootFSPath, "bind", unix.MS_BIND|unix.MS_REC, ""); err != nil {
				return fmt.Errorf("绑定挂载 rootfs 失败: %w", err)
			}
			targetPath = rootFSPath
		} else {
			targetPath = overlay.Merged
		}
	} else {
		if err := unix.Mount(rootFSPath, rootFSPath, "bind", unix.MS_BIND|unix.MS_REC, ""); err != nil {
			return fmt.Errorf("绑定挂载 rootfs 失败: %w", err)
		}
		targetPath = rootFSPath
	}

	pivotDir := filepath.Join(targetPath, ".pivot_root")
	if err := os.MkdirAll(pivotDir, 0755); err != nil {
		return fmt.Errorf("创建 pivot_root 目录失败: %w", err)
	}

	if err := mountVolumesIntoRootfs(targetPath); err != nil {
		fmt.Printf("  警告: 挂载卷失败: %v\n", err)
	}

	if err := pivotRoot(targetPath, pivotDir); err != nil {
		if overlay != nil && overlay.Merged != "" {
			unix.Unmount(overlay.Merged, unix.MNT_DETACH)
		}
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
	return syscall.Exec(argv0, argv, envv)
}

func pivotRoot(newRoot string, putOld string) error {
	//newroot (新根)：你希望未来作为整个系统 /（根目录）的那个目录路径。他必须是一个挂载点而不只是一个普通的文件夹，我们的overlay.Merged就是一个挂载点
	//putold (旧根安放地)：你当前的namespace的根目录（也就是宿主机的 /的复印版本）被替换掉后，不能直接凭空消失，内核需要你提供一个目录，用来临时存放原来的宿主机根目录。通常是 newroot 下面的一个叫 .pivot_root 的临时空文件夹。
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
