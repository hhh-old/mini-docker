//go:build linux

package cgroups

import (
	"fmt"
	"log"
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
	m := &managerV1{
		config: config,
		paths:  make(map[string]string),
	}
	name := config.CgroupName
	if name != "" {
		for _, subsys := range []string{"memory", "cpu", "pids", "freezer"} {
			cgPath := getCgroupPath(subsys, name)
			if _, err := os.Stat(cgPath); err == nil {
				m.paths[subsys] = cgPath
			}
		}
	}
	return m, nil
}

// Apply 在 cgroup v1 中为每个子系统创建独立目录，写入资源限制，并将进程加入各子系统
//
// cgroup v1 与 v2 的核心区别：v1 的每个子系统（memory/cpu/pids/freezer）各自挂载为独立的目录树，
// 每个子系统需要单独创建目录、单独写入 cgroup.procs。
// 而 v2 是统一层级，所有控制在同一目录下。
func (m *managerV1) Apply(pid int) error {
	name := m.config.CgroupName
	pidStr := strconv.Itoa(pid)

	// ─────────────────────────────────────────────────────────────
	// Memory cgroup（路径：/sys/fs/cgroup/memory/mini-docker-xxx/）
	// ─────────────────────────────────────────────────────────────
	if m.config.Memory != nil {
		memPath := getCgroupPath("memory", name)
		if err := mkdirAll(memPath); err != nil {
			return fmt.Errorf("创建 memory cgroup 失败: %w", err)
		}

		// memory.limit_in_bytes — 内存硬限制（对应 v2 的 memory.max，对应 Docker 的 -m）
		//
		// 含义：cgroup 内所有进程最多能使用的物理内存（字节）
		// 行为：当内存使用量达到此值时，内核触发 OOM Killer，直接杀掉占用内存最多的进程
		//
		// 使用示例：
		//   mini-docker run -m 100m busybox /bin/sh → memory.limit_in_bytes = "104857600"
		//   mini-docker run -m 1g busybox /bin/sh   → memory.limit_in_bytes = "1073741824"
		//   docker run -m 512m busybox              → memory.limit_in_bytes = "536870912"
		if m.config.Memory.Limit != nil {
			if err := WriteFile(memPath, "memory.limit_in_bytes", formatMemory(*m.config.Memory.Limit)); err != nil {
				return fmt.Errorf("设置内存限制失败: %w", err)
			}
			memswLimit := *m.config.Memory.Limit * 2
			if m.config.Memory.Swap != nil {
				memswLimit = *m.config.Memory.Limit + *m.config.Memory.Swap
			}
			if err := WriteFile(memPath, "memory.memsw.limit_in_bytes", formatMemory(memswLimit)); err != nil {
				log.Printf("提示: 设置 swap 限制失败（可能未启用 swapaccount）: %v\n", err)
			}
		}
		if err := WriteFile(memPath, "cgroup.procs", pidStr); err != nil {
			return fmt.Errorf("添加进程到 memory cgroup 失败: %w", err)
		}
		m.paths["memory"] = memPath
	}

	// ─────────────────────────────────────────────────────────────
	// CPU cgroup（路径：/sys/fs/cgroup/cpu/mini-docker-xxx/）
	// ─────────────────────────────────────────────────────────────
	if m.config.CPU != nil {
		cpuPath := getCgroupPath("cpu", name)
		if err := mkdirAll(cpuPath); err != nil {
			return fmt.Errorf("创建 cpu cgroup 失败: %w", err)
		}

		// cpu.shares — CPU 相对权重（对应 v2 的 cpu.weight，对应 Docker 的 -c）
		//
		// 含义：该 cgroup 在 CPU 竞争时的相对权重
		// 范围：2 ~ 262144，默认 1024
		// 行为：不是绝对限制，只在 CPU 资源紧张时按比例分配
		//   - CPU 空闲时：shares=2 的容器也能用满全部 CPU
		//   - CPU 竞争时：A=1024, B=512 → A 分到 2/3, B 分到 1/3
		//
		// 使用示例：
		//   mini-docker run -c 512 busybox /bin/sh  → cpu.shares = "512"
		//   mini-docker run -c 1024 busybox /bin/sh → cpu.shares = "1024" (默认值)
		//   docker run -c 2048 busybox              → cpu.shares = "2048"
		if m.config.CPU.Shares != nil {
			if err := WriteFile(cpuPath, "cpu.shares", strconv.FormatInt(*m.config.CPU.Shares, 10)); err != nil {
				return fmt.Errorf("设置 CPU shares 失败: %w", err)
			}
		}

		// cpu.cfs_quota_us + cpu.cfs_period_us — CPU 绝对时间限制（对应 v2 的 cpu.max，对应 Docker 的 --cpus）
		//
		// v1 中 quota 和 period 是两个独立文件，v2 合并为一个文件 "quota period"
		//
		// cfs_period_us：CPU 调度周期（微秒），默认 100000（100ms）
		// cfs_quota_us：周期内允许使用的 CPU 时间（微秒）
		//
		// 示例：
		//   quota=200000, period=100000 → 每 100ms 最多用 200ms CPU → 2 核 CPU
		//   quota=50000,  period=100000 → 每 100ms 最多用 50ms CPU → 0.5 核 CPU
		//   quota=-1,     period=100000 → 不限制
		//
		//   时间线（1 核 CPU，quota=50000, period=100000）：
		//   0ms    50ms              100ms   150ms             200ms
		//   |██████|░░░░░░░░░░░░░░░░|██████|░░░░░░░░░░░░░░░░|
		//    运行   被限流(throttled)  运行    被限流
		//
		// 使用示例：
		//   docker run --cpus=2 busybox                 → cfs_quota_us = "200000", cfs_period_us = "100000"
		//   docker run --cpu-period=100000 --cpu-quota=50000 busybox → cfs_quota_us = "50000", cfs_period_us = "100000"
		if m.config.CPU.Cpus != nil {
			cfsPeriod := int64(100000)
			if m.config.CPU.Period != nil {
				cfsPeriod = *m.config.CPU.Period
			}
			cpus := *m.config.CPU.Cpus
			cfsQuota := calculateCfsQuota(cpus, cfsPeriod)
			if cfsQuota > 0 {
				if err := WriteFile(cpuPath, "cpu.cfs_period_us", strconv.FormatInt(cfsPeriod, 10)); err != nil {
					return fmt.Errorf("设置 cpu.cfs_period_us 失败: %w", err)
				}
				if err := WriteFile(cpuPath, "cpu.cfs_quota_us", strconv.FormatInt(cfsQuota, 10)); err != nil {
					return fmt.Errorf("设置 cpu.cfs_quota_us 失败: %w", err)
				}
			}
		}
		if err := WriteFile(cpuPath, "cgroup.procs", pidStr); err != nil {
			return fmt.Errorf("添加进程到 cpu cgroup 失败: %w", err)
		}
		m.paths["cpu"] = cpuPath
	}

	// ─────────────────────────────────────────────────────────────
	// PIDs cgroup（路径：/sys/fs/cgroup/pids/mini-docker-xxx/）
	// ─────────────────────────────────────────────────────────────
	if m.config.Pids != nil && m.config.Pids.Limit != nil {
		pidsPath := getCgroupPath("pids", name)
		if err := mkdirAll(pidsPath); err != nil {
			return fmt.Errorf("创建 pids cgroup 失败: %w", err)
		}

		// pids.max — 最大进程数（对应 v2 的 pids.max，对应 Docker 的 --pids-limit）
		//
		// 含义：该 cgroup 内允许同时存在的最大进程/线程数
		// 行为：当进程数达到上限后，fork()/clone() 系统调用返回 -EAGAIN，新进程无法创建
		// 主要防御：Fork 炸弹（:(){ :|:& };:），防止恶意或异常进程耗尽宿主机 PID 资源
		//
		// 使用示例：
		//   docker run --pids-limit 100 busybox  → pids.max = "100"
		//   docker run --pids-limit 1000 busybox → pids.max = "1000"
		if err := WriteFile(pidsPath, "pids.max", strconv.FormatInt(*m.config.Pids.Limit, 10)); err != nil {
			return fmt.Errorf("设置 PID 限制失败: %w", err)
		}
		if err := WriteFile(pidsPath, "cgroup.procs", pidStr); err != nil {
			return fmt.Errorf("添加进程到 pids cgroup 失败: %w", err)
		}
		m.paths["pids"] = pidsPath
	}

	// ─────────────────────────────────────────────────────────────
	// Freezer cgroup（路径：/sys/fs/cgroup/freezer/mini-docker-xxx/）
	//
	// v1 中 freezer 是独立子系统，v2 中内置为 cgroup.freeze 文件
	// 无论是否需要立即冻结，都必须创建 freezer cgroup 并将进程加入，
	// 以便后续 pause/unpause 操作可以使用
	// ─────────────────────────────────────────────────────────────
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
	var firstErr error
	for _, path := range m.paths {
		if err := unix.Rmdir(path); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("删除 cgroup %s 失败: %w", path, err)
		}
	}
	return firstErr
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

// Freeze 冻结容器内所有进程（对应 v2 的 cgroup.freeze=1，对应 Docker 的 pause）
//
// cgroup freezer 的机制：内核暂停调度该 cgroup 中的所有进程，它们不会消耗任何 CPU 时间
// 这比 SIGSTOP 更优雅，因为 freezer 是 cgroup 级别操作，不需要逐个发送信号
func (m *managerV1) Freeze() error {
	freezerPath, ok := m.paths["freezer"]
	if !ok {
		return fmt.Errorf("freezer cgroup 未初始化")
	}
	return WriteFile(freezerPath, "freezer.state", "FROZEN")
}

func (m *managerV1) Thaw() error {
	freezerPath, ok := m.paths["freezer"]
	if !ok {
		return fmt.Errorf("freezer cgroup 未初始化")
	}
	return WriteFile(freezerPath, "freezer.state", "THAWED")
}

func (m *managerV1) Set(container *configs.Resources) error {
	m.config = container

	if m.config.Memory != nil {
		if memPath, ok := m.paths["memory"]; ok {
			if m.config.Memory.Limit != nil {
				if err := WriteFile(memPath, "memory.limit_in_bytes", formatMemory(*m.config.Memory.Limit)); err != nil {
					return fmt.Errorf("设置内存限制失败: %w", err)
				}
			}
		}
	}

	if m.config.CPU != nil {
		if cpuPath, ok := m.paths["cpu"]; ok {
			if m.config.CPU.Shares != nil {
				if err := WriteFile(cpuPath, "cpu.shares", strconv.FormatInt(*m.config.CPU.Shares, 10)); err != nil {
					return fmt.Errorf("设置 CPU shares 失败: %w", err)
				}
			}
			if m.config.CPU.Quota != nil {
				if err := WriteFile(cpuPath, "cpu.cfs_quota_us", strconv.FormatInt(*m.config.CPU.Quota, 10)); err != nil {
					return fmt.Errorf("设置 cpu.cfs_quota_us 失败: %w", err)
				}
			}
			if m.config.CPU.Period != nil {
				if err := WriteFile(cpuPath, "cpu.cfs_period_us", strconv.FormatInt(*m.config.CPU.Period, 10)); err != nil {
					return fmt.Errorf("设置 cpu.cfs_period_us 失败: %w", err)
				}
			}
		}
	}

	if m.config.Pids != nil && m.config.Pids.Limit != nil {
		if pidsPath, ok := m.paths["pids"]; ok {
			if err := WriteFile(pidsPath, "pids.max", strconv.FormatInt(*m.config.Pids.Limit, 10)); err != nil {
				return fmt.Errorf("设置 PID 限制失败: %w", err)
			}
		}
	}

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
