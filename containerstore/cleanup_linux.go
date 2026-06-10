//go:build linux

package containerstore

import (
	"golang.org/x/sys/unix"
)

// CleanupOverlay 卸载容器的 OverlayFS 挂载
// 对齐 containerd: overlay mount 在宿主机上完成，容器退出后需要 umount
// 目录删除由 Snapshotter.Remove 统一处理，此处只负责 umount
func CleanupOverlay(info *ContainerInfo) {
	if info.OverlayMerged == "" {
		return
	}

	unix.Unmount(info.OverlayMerged, 0)
}
