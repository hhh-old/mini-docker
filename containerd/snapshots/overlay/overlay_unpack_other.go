//go:build !linux

package overlay

import "fmt"

// extractLayerBlob 解压 tar.gz 层到目标目录（非 Linux 桩实现）
func extractLayerBlob(blobPath, destDir string) (string, error) {
	return "", fmt.Errorf("OCI 层解压仅在 Linux 上可用")
}

// mkdev 构造设备号（非 Linux 桩实现）
func mkdev(major, minor int64) uint32 {
	return 0
}
