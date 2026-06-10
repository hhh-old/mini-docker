//go:build !linux

package containerstore

// CleanupOverlay 卸载容器的 OverlayFS 挂载（非 Linux 平台无操作）
func CleanupOverlay(info *ContainerInfo) {
}
