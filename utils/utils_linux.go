//go:build linux

package utils

import (
	"fmt"
	"syscall"

	"golang.org/x/sys/unix"
)

// DropCapability 从能力边界集中移除一个能力
func DropCapability(cap int) error {
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
