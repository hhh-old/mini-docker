//go:build linux

package containerstore

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// CleanupOverlay 卸载容器的 OverlayFS 挂载
// 对齐 containerd: overlay mount 在宿主机上完成，容器退出后需要 umount
// 目录删除由 Snapshotter.Remove 统一处理，此处只负责 umount
func CleanupOverlay(overlayMerged string) error {
	if overlayMerged == "" {
		return nil
	}

	// 先尝试正常卸载
	if err := unix.Unmount(overlayMerged, 0); err != nil {
		// 挂载点忙时降级为懒卸载（MNT_DETACH：立即从命名空间移除，
		// 等引用消失后自动释放，对齐 Docker/runc 的清理策略）
		if err := unix.Unmount(overlayMerged, unix.MNT_DETACH); err != nil {
			return fmt.Errorf("卸载 overlay %s 失败: %w", overlayMerged, err)
		}
	}
	return nil
}
