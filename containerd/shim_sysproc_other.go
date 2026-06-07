//go:build !linux

package containerd

import "syscall"

func newShimSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{}
}
