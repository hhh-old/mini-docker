//go:build linux

package cgroup

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	cgroupRoot       = "/sys/fs/cgroup"
	miniDockerCgroup = "mini-docker"
)

type CgroupManager struct {
	Pid         int
	MemoryLimit string
	CpuShares   string
	CgroupName  string
}

func (c *CgroupManager) Apply(pid int) error {
	c.Pid = pid
	if c.CgroupName == "" {
		c.CgroupName = fmt.Sprintf("%s-%d", miniDockerCgroup, pid)
	}

	if err := c.setMemoryLimit(); err != nil {
		return fmt.Errorf("设置内存限制失败: %w", err)
	}

	if err := c.setCpuShares(); err != nil {
		return fmt.Errorf("设置 CPU 限制失败: %w", err)
	}

	if err := c.addProcess(pid); err != nil {
		return fmt.Errorf("将进程加入 cgroup 失败: %w", err)
	}

	return nil
}

func (c *CgroupManager) setMemoryLimit() error {
	if c.MemoryLimit == "" {
		return nil
	}

	memoryBytes, err := parseMemory(c.MemoryLimit)
	if err != nil {
		return err
	}

	memoryCgroupPath := filepath.Join(cgroupRoot, "memory", c.CgroupName)
	if err := os.MkdirAll(memoryCgroupPath, 0755); err != nil {
		return fmt.Errorf("创建 memory cgroup 目录失败: %w", err)
	}

	limitFile := filepath.Join(memoryCgroupPath, "memory.limit_in_bytes")
	if err := os.WriteFile(limitFile, []byte(strconv.FormatInt(memoryBytes, 10)), 0644); err != nil {
		return fmt.Errorf("写入内存限制失败: %w", err)
	}

	return nil
}

func (c *CgroupManager) setCpuShares() error {
	if c.CpuShares == "" {
		return nil
	}

	cpuCgroupPath := filepath.Join(cgroupRoot, "cpu", c.CgroupName)
	if err := os.MkdirAll(cpuCgroupPath, 0755); err != nil {
		return fmt.Errorf("创建 cpu cgroup 目录失败: %w", err)
	}

	sharesFile := filepath.Join(cpuCgroupPath, "cpu.shares")
	if err := os.WriteFile(sharesFile, []byte(c.CpuShares), 0644); err != nil {
		return fmt.Errorf("写入 CPU 份额失败: %w", err)
	}

	return nil
}

func (c *CgroupManager) addProcess(pid int) error {
	subsystems := []string{"memory", "cpu"}

	for _, subsys := range subsystems {
		cgroupPath := filepath.Join(cgroupRoot, subsys, c.CgroupName)
		procsFile := filepath.Join(cgroupPath, "cgroup.procs")

		if _, err := os.Stat(cgroupPath); os.IsNotExist(err) {
			continue
		}

		if err := os.WriteFile(procsFile, []byte(strconv.Itoa(pid)), 0644); err != nil {
			return fmt.Errorf("写入 cgroup.procs 失败 (%s): %w", subsys, err)
		}
	}

	return nil
}

func (c *CgroupManager) Destroy() error {
	subsystems := []string{"memory", "cpu"}

	for _, subsys := range subsystems {
		cgroupPath := filepath.Join(cgroupRoot, subsys, c.CgroupName)
		if _, err := os.Stat(cgroupPath); err == nil {
			if err := os.RemoveAll(cgroupPath); err != nil {
				return fmt.Errorf("删除 cgroup 目录失败 (%s): %w", subsys, err)
			}
		}
	}

	return nil
}

func (c *CgroupManager) Freeze() error {
	freezerCgroupPath := filepath.Join(cgroupRoot, "freezer", c.CgroupName)
	if err := os.MkdirAll(freezerCgroupPath, 0755); err != nil {
		return fmt.Errorf("创建 freezer cgroup 目录失败: %w", err)
	}

	if err := c.addProcessToSubsystem(c.Pid, "freezer"); err != nil {
		return err
	}

	stateFile := filepath.Join(freezerCgroupPath, "freezer.state")
	return os.WriteFile(stateFile, []byte("FROZEN"), 0644)
}

func (c *CgroupManager) Unfreeze() error {
	freezerCgroupPath := filepath.Join(cgroupRoot, "freezer", c.CgroupName)
	stateFile := filepath.Join(freezerCgroupPath, "freezer.state")
	return os.WriteFile(stateFile, []byte("THAWED"), 0644)
}

func (c *CgroupManager) addProcessToSubsystem(pid int, subsys string) error {
	cgroupPath := filepath.Join(cgroupRoot, subsys, c.CgroupName)
	procsFile := filepath.Join(cgroupPath, "cgroup.procs")
	return os.WriteFile(procsFile, []byte(strconv.Itoa(pid)), 0644)
}

func parseMemory(memoryStr string) (int64, error) {
	memoryStr = strings.TrimSpace(memoryStr)
	multiplier := int64(1)

	switch {
	case strings.HasSuffix(memoryStr, "g"):
		multiplier = 1024 * 1024 * 1024
		memoryStr = strings.TrimSuffix(memoryStr, "g")
	case strings.HasSuffix(memoryStr, "m"):
		multiplier = 1024 * 1024
		memoryStr = strings.TrimSuffix(memoryStr, "m")
	case strings.HasSuffix(memoryStr, "k"):
		multiplier = 1024
		memoryStr = strings.TrimSuffix(memoryStr, "k")
	}

	value, err := strconv.ParseInt(memoryStr, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("无效的内存限制格式: %s", memoryStr)
	}

	return value * multiplier, nil
}
