//go:build linux

package cgroups

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"mini-docker/constants"
	"mini-docker/libcontainer/configs"

	"golang.org/x/sys/unix"
)

// managerV2 cgroup v2 管理器实现
type managerV2 struct {
	config *configs.Resources // 期望限制的资源配置（如 CPU、内存大小、PID 上限等）
	path   string             // 当前容器在 cgroup v2 文件系统中的绝对路径:/sys/fs/cgroup/mini-docker-abc123/
}

func newManagerV2(config *configs.Resources) (Manager, error) {
	m := &managerV2{
		config: config,
	}
	name := config.CgroupName
	if name != "" {
		cgPath := filepath.Join(constants.CgroupRootPath, name)
		if _, err := os.Stat(cgPath); err == nil {
			m.path = cgPath
		}
	}
	return m, nil
}

func (m *managerV2) Apply(pid int) error {
	name := m.config.CgroupName //CgroupName = constants.CgroupPrefix + containerID,例如"mini-docker-" + "abc123"
	// cgroup是对进程使用的资源进行限制,而不是对namespace资源进行限制
	// /sys/fs/cgroup这个 cgroupfs 的“虚拟文件系统”创建子目录代表创建一个新的控制组
	//内核检测到创建目录的系统调用，会自动在 /sys/fs/cgroup/my_container/ 下生成一堆虚拟文件，如 cgroup.procs、memory.max、cpu.max 等
	//不需要也不应该手动去创建例如 memory.max 这样的文件,只需要往里面写内容
	cgPath := filepath.Join(constants.CgroupRootPath, name) // 例如: /sys/fs/cgroup/mini-docker-abc123/

	if err := mkdirAll(cgPath); err != nil {
		return fmt.Errorf("创建 cgroup 失败: %w", err)
	}

	pidStr := strconv.Itoa(pid)
	//将 PID 写入 cgroup.procs文件中,表示进程组添加要限制的目标对象
	if err := WriteFile(cgPath, "cgroup.procs", pidStr); err != nil {
		return fmt.Errorf("添加进程到 cgroup 失败: %w", err)
	}

	if err := m.applyResources(cgPath); err != nil {
		return fmt.Errorf("应用资源限制失败: %w", err)
	}

	m.path = cgPath
	return nil
}

// applyResources 将资源配置写入 cgroup v2 的控制文件
//
// cgroup v2 统一层级下所有资源控制在同一目录，通过写入不同的虚拟文件来设置各类限制。
// 每个虚拟文件由内核自动创建，应用程序只需写入值即可生效。
func (m *managerV2) applyResources(cgPath string) error {
	res := m.config
	if res == nil {
		return nil
	}

	// ─────────────────────────────────────────────────────────────
	// 内存限制
	// ─────────────────────────────────────────────────────────────
	if res.Memory != nil {
		// memory.max — 内存硬限制（对应 v1 的 memory.limit_in_bytes，对应 Docker 的 -m）
		//
		// 含义：cgroup 内所有进程最多能使用的物理内存（字节）
		// 行为：当内存使用量达到此值时，内核触发 OOM Killer，直接杀掉占用内存最多的进程
		// 类比：硬墙，撞上去就死
		//
		//   内存使用量 ──→ │← memory.max →│
		//                  │   允许使用    │  OOM Kill! 💀
		//
		// 使用示例：
		//   mini-docker run -m 100m busybox /bin/sh   → memory.max = "104857600" (100MB)
		//   mini-docker run -m 1g busybox /bin/sh     → memory.max = "1073741824" (1GB)
		//   docker run -m 512m busybox                → memory.max = "536870912" (512MB)
		if res.Memory.Limit != nil {
			if err := WriteFile(cgPath, "memory.max", formatMemory(*res.Memory.Limit)); err != nil {
				return fmt.Errorf("设置 memory.max 失败: %w", err)
			}
		}

		// memory.high — 内存软限制（对应 Docker 的 --memory-reservation）
		//
		// 含义：内存使用的软上限（字节），超过后不会杀进程，但会受到惩罚
		// 行为：当内存使用超过 memory.high 但未超过 memory.max 时：
		//   - 内核大幅限流该 cgroup 的内存分配请求（让进程变慢）
		//   - 内核更积极地回收该 cgroup 的缓存页（page cache）
		//   - 进程不会死，但会明显变慢
		// 类比：软墙，撞上去会减速但不会死
		// 约束：memory.high ≤ memory.max，否则没有意义
		//
		//   内存使用量 ──→ │← memory.high →│← memory.max →│
		//                  │  正常速度      │  被限流变慢   │  OOM Kill 💀
		//
		// 使用示例：
		//   docker run -m 1g --memory-reservation 512m busybox → memory.high = "536870912" (512MB)
		if res.Memory.Reservation != nil {
			WriteFile(cgPath, "memory.high", formatMemory(*res.Memory.Reservation))
		}

		// memory.swap.max — Swap 限制（对应 v1 的 memory.memsw.limit_in_bytes，对应 Docker 的 --memory-swap）
		//
		// 含义：cgroup 内所有进程最多能使用的 Swap 空间（字节）
		// 行为：
		//   - 设为 0：完全禁止使用 Swap，内存用完直接 OOM Kill
		//   - 设为具体值：允许使用这么多 Swap
		//   - 设为 "max"：不限制 Swap 使用
		// 注意：v1 的 memory.memsw.limit_in_bytes 是"内存+Swap 总量"，v2 更清晰——Swap 单独限制
		//
		// 使用示例：
		//   docker run -m 100m --memory-swap 200m busybox  → memory.swap.max = "104857600" (100MB Swap)
		//   docker run -m 100m --memory-swap 100m busybox  → memory.swap.max = "0" (禁用 Swap)
		if res.Memory.Swap != nil {
			WriteFile(cgPath, "memory.swap.max", formatMemory(*res.Memory.Swap))
		}
	}

	// ─────────────────────────────────────────────────────────────
	// CPU 限制
	// ─────────────────────────────────────────────────────────────
	if res.CPU != nil {
		// cpu.weight — CPU 相对权重（对应 v1 的 cpu.shares，对应 Docker 的 -c）
		//
		// 含义：该 cgroup 在 CPU 竞争时的相对权重，决定能分到多少 CPU 时间
		// 范围：1 ~ 10000，默认 100
		// 行为：不是绝对限制，只在 CPU 资源紧张时才起作用
		//   - CPU 空闲时：权重 1 的容器也能用满全部 CPU
		//   - CPU 竞争时：按权重比例分配（A=200, B=100 → A 分到 2/3, B 分到 1/3）
		// 转换：weight = shares × 100 / 1024（v1 默认 1024 → v2 默认 100）
		//
		// 使用示例：
		//   mini-docker run -c 512 busybox /bin/sh  → cpu.weight = "50" (512*100/1024)
		//   mini-docker run -c 1024 busybox /bin/sh → cpu.weight = "100" (默认值)
		//   docker run -c 2048 busybox              → cpu.weight = "200"
		if res.CPU.Shares != nil {
			weight := cpusharesToWeight(*res.CPU.Shares)
			WriteFile(cgPath, "cpu.weight", strconv.FormatInt(weight, 10))
		}

		// cpu.max — CPU 绝对时间限制（对应 v1 的 cpu.cfs_quota_us + cpu.cfs_period_us，对应 Docker 的 --cpus）
		//
		// 含义：格式为 "quota period"，即"在 period 微秒的周期内，最多允许使用 quota 微秒的 CPU 时间"
		// 行为：绝对限制，即使 CPU 空闲也不能超；超限后进程被限流（throttled）
		// v1 中 quota 和 period 是两个独立文件，v2 合并为一个文件
		//
		// 示例：
		//   "200000 100000" → 每 100ms 最多用 200ms CPU → 2 核 CPU
		//   "50000 100000"  → 每 100ms 最多用 50ms CPU → 0.5 核 CPU
		//   "max 100000"    → 不限制
		//
		//   时间线（1 核 CPU，quota=50000, period=100000）：
		//   0ms    50ms              100ms   150ms             200ms
		//   |██████|░░░░░░░░░░░░░░░░|██████|░░░░░░░░░░░░░░░░|
		//    运行   被限流(throttled)  运行    被限流
		//
		// 三种设置路径：
		//   A) 通过 Cpus 计算：--cpus=2 → quota = 2 × 100000 = 200000
		//   B) 同时指定 Quota 和 Period
		//   C) 只指定 Quota，Period 默认 100000μs
		//
		// 使用示例：
		//   docker run --cpus=2 busybox       → cpu.max = "200000 100000" (2 核)
		//   docker run --cpus=0.5 busybox     → cpu.max = "50000 100000" (0.5 核)
		//   docker run --cpu-period=100000 --cpu-quota=50000 busybox → cpu.max = "50000 100000"
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

	// ─────────────────────────────────────────────────────────────
	// PID 限制
	// ─────────────────────────────────────────────────────────────
	if res.Pids != nil && res.Pids.Limit != nil {
		// pids.max — 最大进程数（对应 v1 的 pids.max，对应 Docker 的 --pids-limit）
		//
		// 含义：该 cgroup 内允许同时存在的最大进程/线程数
		// 行为：当进程数达到上限后，fork()/clone() 系统调用返回 -EAGAIN，新进程无法创建
		// 主要防御：Fork 炸弹（:(){ :|:& };:），防止恶意或异常进程耗尽宿主机 PID 资源
		//
		//   进程数 ──→ │← pids.max →│
		//              │  允许创建   │  fork() 返回 -EAGAIN ❌
		//
		// 使用示例：
		//   docker run --pids-limit 100 busybox  → pids.max = "100"
		//   docker run --pids-limit 1000 busybox → pids.max = "1000"
		if err := WriteFile(cgPath, "pids.max", strconv.FormatInt(*res.Pids.Limit, 10)); err != nil {
			return fmt.Errorf("设置 pids.max 失败: %w", err)
		}
	}

	return nil
}

// 当容器停止并退出时，需要清理对应的 cgroup 目录以释放内核资源
func (m *managerV2) Destroy() error {
	if m.path == "" {
		return nil
	}
	return unix.Rmdir(m.path) //在 Linux cgroup v2 中，只有当 cgroup 内没有存活的进程，且不包含子 cgroup 时，调用 rmdir 才能成功删除该目录。
}

func (m *managerV2) GetPaths() map[string]string {
	return map[string]string{
		"": m.path,
	}
}

// 用于监控容器当前的资源消耗状态
// 用 GetStats 而不是 /proc 是因为：
// ①不需要进入容器 namespace，宿主机直接读取；
// ②cgroup 提供整个进程组的聚合统计；
// ③同时包含 usage 和 limit 信息；
// ④提供 throttle 等调度细节，/proc 无法获取。
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

// cgroup v2 提供了一个非常优雅的进程暂停机制。在 v1 中需要依靠独立的 freezer 子系统，而在 v2 中它被内置在每个 cgroup 目录下的 cgroup.freeze 文件中
// 暂停容器内的所有进程,暂停整个资源组内的进程,容器PID为1的进程派生出来的进程也在同一个cgroup资源组中
func (m *managerV2) Freeze() error {
	if m.path == "" {
		return fmt.Errorf("cgroup 路径未初始化")
	}
	return WriteFile(m.path, "cgroup.freeze", "1") // 写入 1 暂停
}

// // 恢复容器内的所有进程
func (m *managerV2) Thaw() error {
	if m.path == "" {
		return fmt.Errorf("cgroup 路径未初始化")
	}
	return WriteFile(m.path, "cgroup.freeze", "0") // 写入 0 恢复
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
// weight = shares * 100/1024
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
