//go:build !linux

package cgroup

import "fmt"

type CgroupManager struct {
	Pid         int
	MemoryLimit string
	CpuShares   string
	CgroupName  string
}

func (c *CgroupManager) Apply(pid int) error {
	return fmt.Errorf("cgroup 仅在 Linux 上可用，请在 WSL2 或 Linux 环境中运行")
}

func (c *CgroupManager) Destroy() error {
	return fmt.Errorf("cgroup 仅在 Linux 上可用")
}

func (c *CgroupManager) Freeze() error {
	return fmt.Errorf("cgroup 仅在 Linux 上可用")
}

func (c *CgroupManager) Unfreeze() error {
	return fmt.Errorf("cgroup 仅在 Linux 上可用")
}
