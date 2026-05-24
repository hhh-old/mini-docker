//go:build !linux

package utils

import "fmt"

// DropCapability 从能力边界集中移除一个能力（非 Linux 平台不支持）
func DropCapability(cap int) error {
	return fmt.Errorf("capability 操作仅在 Linux 上可用")
}
