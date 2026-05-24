//go:build linux

package cgroups

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"mini-docker/libcontainer/configs"

	"golang.org/x/sys/unix"
)

// managerV1 cgroup v1 管理器实现
type managerV1 struct {
	config *configs.Resources
	paths  map[string]string
}

func newManagerV1(config *configs.Resources) (Manager, error) {
	return &managerV1{
		config: config,
		paths:  make(map[string]string),
	}, nil
}

func (m *managerV1) Apply(pid int) error {
	name := m.config.CgroupName
	pidStr := strconv.Itoa(pid)

	// Memory cgroup
	if m.config.Memory != nil {
		memPath := getCgroupPath("memory", name)
		if err := mkdirAll(memPath); err != nil {
			return fmt.Errorf("创建 memory cgroup 失败: %w", err)
		}
		if m.config.Memory.Limit != nil {
			if err := WriteFile(memPath, "memory.limit_in_bytes", formatMemory(*m.config.Memory.Limit)); err != nil {
				return fmt.Errorf("设置内存限制失败: %w", err)
			}
		}
		if err := WriteFile(memPath, "cgroup.procs", pidStr); err != nil {
			return fmt.Errorf("添加进程到 memory cgroup 失败: %w", err)
		}
		m.paths["memory"] = memPath
	}

	// CPU cgroup
	if m.config.CPU != nil {
		cpuPath := getCgroupPath("cpu", name)
		if err := mkdirAll(cpuPath); err != nil {
			return fmt.Errorf("创建 cpu cgroup 失败: %w", err)
		}
		if m.config.CPU.Shares != nil {
			if err := WriteFile(cpuPath, "cpu.shares", strconv.FormatInt(*m.config.CPU.Shares, 10)); err != nil {
				return fmt.Errorf("设置 CPU shares 失败: %w", err)
			}
		}
		if m.config.CPU.Cpus != nil {
			cfsPeriod := int64(100000)
			if m.config.CPU.Period != nil {
				cfsPeriod = *m.config.CPU.Period
			}
			cpus := *m.config.CPU.Cpus
			cfsQuota := calculateCfsQuota(cpus, cfsPeriod)
			if cfsQuota > 0 {
				WriteFile(cpuPath, "cfs_period_us", strconv.FormatInt(cfsPeriod, 10))
				WriteFile(cpuPath, "cfs_quota_us", strconv.FormatInt(cfsQuota, 10))
			}
		}
		if err := WriteFile(cpuPath, "cgroup.procs", pidStr); err != nil {
			return fmt.Errorf("添加进程到 cpu cgroup 失败: %w", err)
		}
		m.paths["cpu"] = cpuPath
	}

	// PIDs cgroup
	if m.config.Pids != nil && m.config.Pids.Limit != nil {
		pidsPath := getCgroupPath("pids", name)
		if err := mkdirAll(pidsPath); err != nil {
			return fmt.Errorf("创建 pids cgroup 失败: %w", err)
		}
		if err := WriteFile(pidsPath, "pids.max", strconv.FormatInt(*m.config.Pids.Limit, 10)); err != nil {
			return fmt.Errorf("设置 PID 限制失败: %w", err)
		}
		if err := WriteFile(pidsPath, "cgroup.procs", pidStr); err != nil {
			return fmt.Errorf("添加进程到 pids cgroup 失败: %w", err)
		}
		m.paths["pids"] = pidsPath
	}

	// Freezer cgroup
	freezerPath := getCgroupPath("freezer", name)
	if err := mkdirAll(freezerPath); err != nil {
		return fmt.Errorf("创建 freezer cgroup 失败: %w", err)
	}
	if err := WriteFile(freezerPath, "cgroup.procs", pidStr); err != nil {
		return fmt.Errorf("添加进程到 freezer cgroup 失败: %w", err)
	}
	m.paths["freezer"] = freezerPath

	return nil
}

func (m *managerV1) Destroy() error {
	for _, path := range m.paths {
		unix.Rmdir(path)
	}
	return nil
}

func (m *managerV1) GetPaths() map[string]string {
	return m.paths
}

func (m *managerV1) GetStats() (*Stats, error) {
	stats := &Stats{}

	if memPath, ok := m.paths["memory"]; ok {
		stats.MemoryStats = &MemoryStats{}
		if v, err := readUint64(filepath.Join(memPath, "memory.usage_in_bytes")); err == nil {
			stats.MemoryStats.Usage = v
		}
		if v, err := readUint64(filepath.Join(memPath, "memory.max_usage_in_bytes")); err == nil {
			stats.MemoryStats.MaxUsage = v
		}
		if v, err := readUint64(filepath.Join(memPath, "memory.limit_in_bytes")); err == nil {
			stats.MemoryStats.Limit = v
		}
	}

	return stats, nil
}

func (m *managerV1) Freeze() error {
	name := m.config.CgroupName
	freezerPath := getCgroupPath("freezer", name)
	if err := mkdirAll(freezerPath); err != nil {
		return err
	}
	return WriteFile(freezerPath, "freezer.state", "FROZEN")
}

func (m *managerV1) Thaw() error {
	name := m.config.CgroupName
	freezerPath := getCgroupPath("freezer", name)
	return WriteFile(freezerPath, "freezer.state", "THAWED")
}

func (m *managerV1) Set(container *configs.Resources) error {
	m.config = container
	return nil
}

// calculateCfsQuota 计算 CFS quota（根据 CPU 核数）
func calculateCfsQuota(cpus string, period int64) int64 {
	cores := parseCpus(cpus)
	if cores <= 0 {
		return 0
	}
	return cores * period
}

// parseCpus 解析 CPU 列表（如 "0-3,7" -> 5）
func parseCpus(cpus string) int64 {
	count := int64(0)
	parts := strings.Split(cpus, ",")
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if strings.Contains(part, "-") {
			rangeParts := strings.SplitN(part, "-", 2)
			if len(rangeParts) == 2 {
				start, err1 := strconv.ParseInt(rangeParts[0], 10, 64)
				end, err2 := strconv.ParseInt(rangeParts[1], 10, 64)
				if err1 == nil && err2 == nil && end >= start {
					count += end - start + 1
				}
			}
		} else {
			if _, err := strconv.ParseInt(part, 10, 64); err == nil {
				count++
			}
		}
	}
	return count
}

// writeFile 写入文件
func writeFile(path, data string) error {
	return os.WriteFile(path, []byte(data), 0644)
}

// readFile 读取文件
func readFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

// mkdirAll_ 创建目录
func mkdirAll_(path string) error {
	return os.MkdirAll(path, 0755)
}

// readUint64 读取 uint64 值
func readUint64(path string) (uint64, error) {
	data, err := readFile(path)
	if err != nil {
		return 0, err
	}
	return strconv.ParseUint(data, 10, 64)
}
