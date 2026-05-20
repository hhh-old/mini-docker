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
	Cpus        string // --cpus 参数（如 1.5 表示 1.5 核）
	PidsLimit   string // --pids-limit 参数
	BlkioWeight string // --blkio-weight 参数
	isCgroupV2  bool   // 是否使用 cgroup v2
}

func (c *CgroupManager) Apply(pid int) error {
	c.Pid = pid
	if c.CgroupName == "" {
		c.CgroupName = fmt.Sprintf("%s-%d", miniDockerCgroup, pid)
	}

	// 自动检测 cgroup 版本
	c.isCgroupV2 = isCgroupV2()

	if c.isCgroupV2 {
		return c.applyV2(pid)
	}
	return c.applyV1(pid)
}

// applyV1 cgroup v1 应用资源限制
func (c *CgroupManager) applyV1(pid int) error {
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

// applyV2 cgroup v2 应用资源限制
func (c *CgroupManager) applyV2(pid int) error {
	cgroupPath := filepath.Join(cgroupRoot, c.CgroupName)
	if err := os.MkdirAll(cgroupPath, 0755); err != nil {
		return fmt.Errorf("创建 cgroup 目录失败: %w", err)
	}

	// 设置内存限制
	if c.MemoryLimit != "" {
		memoryBytes, err := parseMemory(c.MemoryLimit)
		if err != nil {
			return err
		}
		maxFile := filepath.Join(cgroupPath, "memory.max")
		if err := os.WriteFile(maxFile, []byte(strconv.FormatInt(memoryBytes, 10)), 0644); err != nil {
			return fmt.Errorf("写入 memory.max 失败: %w", err)
		}
	}

	// 设置 CPU 限制
	if c.Cpus != "" {
		if err := c.setCpusV2(cgroupPath); err != nil {
			fmt.Printf("  警告: 设置 CPU 限制失败: %v\n", err)
		}
	} else if c.CpuShares != "" {
		// cgroup v2 中 cpu.shares 被替换为 cpu.weight
		weightFile := filepath.Join(cgroupPath, "cpu.weight")
		if err := os.WriteFile(weightFile, []byte(c.CpuShares), 0644); err != nil {
			fmt.Printf("  警告: 写入 cpu.weight 失败: %v\n", err)
		}
	}

	// 设置 PIDs 限制
	if c.PidsLimit != "" {
		pidsFile := filepath.Join(cgroupPath, "pids.max")
		if err := os.WriteFile(pidsFile, []byte(c.PidsLimit), 0644); err != nil {
			fmt.Printf("  警告: 写入 pids.max 失败: %v\n", err)
		}
	}

	// 设置 IO 权重
	if c.BlkioWeight != "" {
		// cgroup v2: io.bfq.weight
		weightFile := filepath.Join(cgroupPath, "io.bfq.weight")
		if err := os.WriteFile(weightFile, []byte(c.BlkioWeight), 0644); err != nil {
			fmt.Printf("  警告: 写入 io.bfq.weight 失败: %v\n", err)
		}
	}

	// 将进程加入 cgroup
	procsFile := filepath.Join(cgroupPath, "cgroup.procs")
	if err := os.WriteFile(procsFile, []byte(strconv.Itoa(pid)), 0644); err != nil {
		return fmt.Errorf("写入 cgroup.procs 失败: %w", err)
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
	if c.isCgroupV2 {
		return c.destroyV2()
	}
	return c.destroyV1()
}

func (c *CgroupManager) destroyV1() error {
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

func (c *CgroupManager) destroyV2() error {
	cgroupPath := filepath.Join(cgroupRoot, c.CgroupName)
	if _, err := os.Stat(cgroupPath); err == nil {
		if err := os.RemoveAll(cgroupPath); err != nil {
			return fmt.Errorf("删除 cgroup 目录失败: %w", err)
		}
	}
	return nil
}

func (c *CgroupManager) Freeze() error {
	if c.isCgroupV2 {
		return c.freezeV2()
	}
	return c.freezeV1()
}

func (c *CgroupManager) freezeV1() error {
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

func (c *CgroupManager) freezeV2() error {
	cgroupPath := filepath.Join(cgroupRoot, c.CgroupName)
	if err := os.MkdirAll(cgroupPath, 0755); err != nil {
		return fmt.Errorf("创建 cgroup 目录失败: %w", err)
	}

	// cgroup v2: cgroup.freeze
	freezeFile := filepath.Join(cgroupPath, "cgroup.freeze")
	return os.WriteFile(freezeFile, []byte("1"), 0644)
}

func (c *CgroupManager) Unfreeze() error {
	if c.isCgroupV2 {
		return c.unfreezeV2()
	}
	return c.unfreezeV1()
}

func (c *CgroupManager) unfreezeV1() error {
	freezerCgroupPath := filepath.Join(cgroupRoot, "freezer", c.CgroupName)
	stateFile := filepath.Join(freezerCgroupPath, "freezer.state")
	return os.WriteFile(stateFile, []byte("THAWED"), 0644)
}

func (c *CgroupManager) unfreezeV2() error {
	cgroupPath := filepath.Join(cgroupRoot, c.CgroupName)
	freezeFile := filepath.Join(cgroupPath, "cgroup.freeze")
	return os.WriteFile(freezeFile, []byte("0"), 0644)
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

// isCgroupV2 检测系统是否使用 cgroup v2
// cgroup v2: /sys/fs/cgroup/cgroup.controllers 文件存在
// cgroup v1: /sys/fs/cgroup/memory/ 目录存在
func isCgroupV2() bool {
	// 如果存在 cgroup.controllers 文件，说明是 cgroup v2 统一层级
	if _, err := os.Stat(filepath.Join(cgroupRoot, "cgroup.controllers")); err == nil {
		return true
	}
	return false
}

// setCpusV2 设置 CPU 核数限制（cgroup v2）
// --cpus 1.5 → cpu.max = "150000 100000", cpu.max = "max 100000"
func (c *CgroupManager) setCpusV2(cgroupPath string) error {
	cpus, err := strconv.ParseFloat(c.Cpus, 64)
	if err != nil {
		return fmt.Errorf("无效的 CPU 核数格式: %s", c.Cpus)
	}

	// cpu.max = quota period
	// quota = cpus * period (period 默认 100000 微秒)
	period := int64(100000)
	quota := int64(cpus * float64(period))

	maxFile := filepath.Join(cgroupPath, "cpu.max")
	content := fmt.Sprintf("%d %d", quota, period)
	if err := os.WriteFile(maxFile, []byte(content), 0644); err != nil {
		return fmt.Errorf("写入 cpu.max 失败: %w", err)
	}

	return nil
}
