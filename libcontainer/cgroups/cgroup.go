//go:build linux

// Package cgroups 提供 cgroup 资源限制管理，对标 runc/libcontainer/cgroups
package cgroups

import (
	"fmt"
	"path/filepath"

	"mini-docker/constants"
	"mini-docker/libcontainer/configs"
)

// Manager cgroup 管理器接口（对标 libcontainer/cgroups.Manager）
type Manager interface {
	// Apply 将 cgroup 配置应用到指定 PID
	Apply(pid int) error

	// Destroy 销毁 cgroup
	Destroy() error

	// GetPaths 获取 cgroup 路径
	GetPaths() map[string]string

	// GetStats 获取 cgroup 统计信息
	GetStats() (*Stats, error)

	// Freeze 冻结容器进程
	Freeze() error

	// Thaw 解冻容器进程
	Thaw() error

	// Set 设置 cgroup 资源限制
	Set(container *configs.Resources) error
}

// Stats cgroup 统计信息
type Stats struct {
	MemoryStats *MemoryStats `json:"memory_stats,omitempty"`
	CPUStats    *CPUStats    `json:"cpu_stats,omitempty"`
	PidsStats   *PidsStats   `json:"pids_stats,omitempty"`
	BlkioStats  *BlkioStats  `json:"blkio_stats,omitempty"`
}

// MemoryStats 内存统计
type MemoryStats struct {
	Usage    uint64 `json:"usage"`
	MaxUsage uint64 `json:"max_usage"`
	Limit    uint64 `json:"limit"`
	Failcnt  uint64 `json:"failcnt"`
}

// CPUStats CPU 统计
type CPUStats struct {
	CPUUsage struct {
		TotalUsage  uint64   `json:"total_usage"`
		PercpuUsage []uint64 `json:"percpu_usage"`
	} `json:"cpu_usage"`
	ThrottlingData struct {
		Periods          uint64 `json:"periods"`
		ThrottledPeriods uint64 `json:"throttled_periods"`
		ThrottledTime    uint64 `json:"throttled_time"`
	} `json:"throttling_data"`
}

// PidsStats 进程数统计
type PidsStats struct {
	Current uint64 `json:"current"`
	Limit   uint64 `json:"limit"`
}

// BlkioStats 块 I/O 统计
type BlkioStats struct {
	IoServiceBytesRecursive []BlkioEntry `json:"io_service_bytes_recursive"`
	IoServicedRecursive     []BlkioEntry `json:"io_serviced_recursive"`
}

// BlkioEntry 块 I/O 统计条目
type BlkioEntry struct {
	Major uint64 `json:"major"`
	Minor uint64 `json:"minor"`
	Op    string `json:"op"`
	Value uint64 `json:"value"`
}

// NewManager 创建 cgroup 管理器（自动检测 v1/v2）
func NewManager(config *configs.Resources, name string) (Manager, error) {
	if config == nil {
		config = &configs.Resources{}
	}
	if config.CgroupName == "" {
		config.CgroupName = name
	}

	if IsCgroupV2() {
		return newManagerV2(config)
	}
	return newManagerV1(config)
}

// IsCgroupV2 检测是否使用 cgroup v2
// /sys/fs/cgroup/cgroup.controllers 是 Linux 内核自己创建的,不是项目创建的，是 Linux 内核自动生成的。
// 使用 cgroup v1 还是 v2，也是由 Linux 内核（以及系统引导配置）决定的，不是应用程序能选择的
func IsCgroupV2() bool {
	_, err := ReadFile(constants.CgroupRootPath, "cgroup.controllers")
	return err == nil
}

// getCgroupPath 获取 cgroup 子系统路径
func getCgroupPath(subsystem, name string) string {
	return filepath.Join(constants.CgroupRootPath, subsystem, name)
}

// WriteFile 写入 cgroup 文件
func WriteFile(dir, file, data string) error {
	return writeFile(filepath.Join(dir, file), data)
}

// ReadFile 读取 cgroup 文件
func ReadFile(dir, file string) (string, error) {
	return readFile(filepath.Join(dir, file))
}

// mkdirAll 创建 cgroup 目录
func mkdirAll(path string) error {
	return mkdirAll_(path)
}

// formatMemory 格式化内存大小
func formatMemory(size int64) string {
	if size <= 0 {
		return ""
	}
	return fmt.Sprintf("%d", size)
}
