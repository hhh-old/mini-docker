//go:build !linux

package utils

import "fmt"

func DropCapability(cap int) error {
	return fmt.Errorf("capability 操作仅在 Linux 上可用")
}
