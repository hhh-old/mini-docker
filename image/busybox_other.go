//go:build !linux

package image

import "fmt"

func setupBusybox(rootFSPath string) error {
	fmt.Printf("  提示: busybox 自动安装仅在 Linux 上可用\n")
	fmt.Printf("  请在 WSL2/Linux 环境中运行，或手动复制 busybox 到 %s/bin/\n", rootFSPath)
	return nil
}
