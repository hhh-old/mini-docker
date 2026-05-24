//go:build linux

package cgroups

import (
	"fmt"
	"path/filepath"
	"strconv"

	"mini-docker/libcontainer/configs"

	"golang.org/x/sys/unix"
)

// managerV2 cgroup v2 管理器实现
type managerV2 struct {
	config *configs.Resources
	path   string
}

func newManagerV2(config *configs.Resources) (Manager, error) {
	return &managerV2{
		config: config,
	}, nil
}

func (m *managerV2) Apply(pid int) error {
	name := m.config.CgroupName
	cgPath := filepath.Join(cgroupRoot, name)

	if err := mkdirAll(cgPath); err != nil {
		return fmt.Errorf("创建 cgroup 失败: %w", err)
	}

	pidStr := strconv.Itoa(pid)
	if err := WriteFile(cgPath, "cgroup.procs", pidStr); err != nil {
		return fmt.Errorf("添加进程到 cgroup 失败: %w", err)
	}

	if err := m.applyResources(cgPath); err != nil {
		return fmt.Errorf("应用资源限制失败: %w", err)
	}

	m.path = cgPath
	return nil
}

func (m *managerV2) applyResources(cgPath string) error {
	res := m.config
	if res == nil {
		return nil
	}

	// 内存限制
	if res.Memory != nil {
		if res.Memory.Limit != nil {
			if err := WriteFile(cgPath, "memory.max", formatMemory(*res.Memory.Limit)); err != nil {
				return fmt.Errorf("设置 memory.max 失败: %w", err)
			}
		}
		if res.Memory.Reservation != nil {
			WriteFile(cgPath, "memory.high", formatMemory(*res.Memory.Reservation))
		}
		if res.Memory.Swap != nil {
			WriteFile(cgPath, "memory.swap.max", formatMemory(*res.Memory.Swap))
		}
	}

	// CPU 限制
	if res.CPU != nil {
		if res.CPU.Shares != nil {
			weight := cpusharesToWeight(*res.CPU.Shares)
			WriteFile(cgPath, "cpu.weight", strconv.FormatInt(weight, 10))
		}
		if res.CPU.Cpus != nil {
			cfsPeriod := int64(100000)
			if res.CPU.Period != nil {
				cfsPeriod = *res.CPU.Period
			}
			cores := parseCpus(*res.CPU.Cpus)
			if cores > 0 {
				cfsQuota := cores * cfsPeriod
				maxVal := fmt.Sprintf("%d %d", cfsQuota, cfsPeriod)
				WriteFile(cgPath, "cpu.max", maxVal)
			}
		} else if res.CPU.Quota != nil && res.CPU.Period != nil {
			maxVal := fmt.Sprintf("%d %d", *res.CPU.Quota, *res.CPU.Period)
			WriteFile(cgPath, "cpu.max", maxVal)
		} else if res.CPU.Quota != nil {
			maxVal := fmt.Sprintf("%d %d", *res.CPU.Quota, 100000)
			WriteFile(cgPath, "cpu.max", maxVal)
		}
	}

	// PID 限制
	if res.Pids != nil && res.Pids.Limit != nil {
		if err := WriteFile(cgPath, "pids.max", strconv.FormatInt(*res.Pids.Limit, 10)); err != nil {
			return fmt.Errorf("设置 pids.max 失败: %w", err)
		}
	}

	return nil
}

func (m *managerV2) Destroy() error {
	if m.path == "" {
		return nil
	}
	return unix.Rmdir(m.path)
}

func (m *managerV2) GetPaths() map[string]string {
	return map[string]string{
		"": m.path,
	}
}

func (m *managerV2) GetStats() (*Stats, error) {
	stats := &Stats{}
	if m.path == "" {
		return stats, nil
	}

	stats.MemoryStats = &MemoryStats{}
	if v, err := readUint64(filepath.Join(m.path, "memory.current")); err == nil {
		stats.MemoryStats.Usage = v
	}
	if v, err := readUint64(filepath.Join(m.path, "memory.max")); err == nil {
		stats.MemoryStats.Limit = v
	}

	stats.PidsStats = &PidsStats{}
	if v, err := readUint64(filepath.Join(m.path, "pids.current")); err == nil {
		stats.PidsStats.Current = v
	}

	return stats, nil
}

func (m *managerV2) Freeze() error {
	if m.path == "" {
		return fmt.Errorf("cgroup 路径未初始化")
	}
	return WriteFile(m.path, "cgroup.freeze", "1")
}

func (m *managerV2) Thaw() error {
	if m.path == "" {
		return fmt.Errorf("cgroup 路径未初始化")
	}
	return WriteFile(m.path, "cgroup.freeze", "0")
}

func (m *managerV2) Set(container *configs.Resources) error {
	m.config = container
	if m.path != "" {
		return m.applyResources(m.path)
	}
	return nil
}

// cpusharesToWeight 将 cpu shares 转换为 cgroup v2 的 cpu weight
// cgroup v1 shares 范围: 2-262144，默认 1024
// cgroup v2 weight 范围: 1-10000，默认 100
func cpusharesToWeight(shares int64) int64 {
	if shares <= 0 {
		return 100
	}
	weight := shares * 100 / 1024
	if weight < 1 {
		weight = 1
	}
	if weight > 10000 {
		weight = 10000
	}
	return weight
}
