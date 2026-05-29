//go:build !linux

package cgroups

import (
	"fmt"

	"mini-docker/libcontainer/configs"
)

// Manager cgroup 管理器接口
type Manager interface {
	Apply(pid int) error
	Destroy() error
	GetPaths() map[string]string
	GetStats() (*Stats, error)
	Freeze() error
	Thaw() error
	Set(container *configs.Resources) error
}

type Stats struct {
	MemoryStats *MemoryStats `json:"memory_stats,omitempty"`
	CPUStats    *CPUStats    `json:"cpu_stats,omitempty"`
	PidsStats   *PidsStats   `json:"pids_stats,omitempty"`
	BlkioStats  *BlkioStats  `json:"blkio_stats,omitempty"`
}

type MemoryStats struct {
	Usage    uint64 `json:"usage"`
	MaxUsage uint64 `json:"max_usage"`
	Limit    uint64 `json:"limit"`
	Failcnt  uint64 `json:"failcnt"`
}

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

type PidsStats struct {
	Current uint64 `json:"current"`
	Limit   uint64 `json:"limit"`
}

type BlkioStats struct {
	IoServiceBytesRecursive []BlkioEntry `json:"io_service_bytes_recursive"`
	IoServicedRecursive     []BlkioEntry `json:"io_serviced_recursive"`
}

type BlkioEntry struct {
	Major uint64 `json:"major"`
	Minor uint64 `json:"minor"`
	Op    string `json:"op"`
	Value uint64 `json:"value"`
}

func NewManager(config *configs.Resources, name string) (Manager, error) {
	return nil, fmt.Errorf("cgroup 仅在 Linux 上可用")
}

func IsCgroupV2() bool { return false }

func RemoveCgroup(cgroupName string) {}
