package configs

// Resources cgroup 资源限制配置（对标 OCI runtime-spec Linux Resources）
type Resources struct {
	// CgroupName cgroup 组名
	CgroupName string `json:"cgroup_name,omitempty"`

	// Memory 内存限制配置
	Memory *Memory `json:"memory,omitempty"`

	// CPU CPU 资源配置
	CPU *CPU `json:"cpu,omitempty"`

	// Pids 进程数限制
	Pids *Pids `json:"pids,omitempty"`

	// BlockIO 块设备 I/O 配置
	BlockIO *BlockIO `json:"block_io,omitempty"`

	// Devices 设备白名单
	Devices []*Device `json:"devices,omitempty"`
}

// Memory 内存资源限制
type Memory struct {
	// Limit 内存限制（字节）
	Limit *int64 `json:"limit,omitempty"`

	// Reservation 内存软限制（字节）
	Reservation *int64 `json:"reservation,omitempty"`

	// Swap Swap 限制（字节）
	Swap *int64 `json:"swap,omitempty"`

	// Kernel 内核内存限制（字节）
	Kernel *int64 `json:"kernel,omitempty"`

	// KernelTCP TCP 内存限制（字节）
	KernelTCP *int64 `json:"kernel_tcp,omitempty"`

	// Swappiness Swap 倾向（0-100）
	Swappiness *uint64 `json:"swappiness,omitempty"`
}

// CPU CPU 资源配置
type CPU struct {
	// Shares CPU 份额（相对权重，1024 = 默认）
	Shares *int64 `json:"shares,omitempty"`

	// Quota 每个周期内可用的 CPU 时间（微秒）
	Quota *int64 `json:"quota,omitempty"`

	// Period CPU 调度周期（微秒，默认 100000）
	Period *int64 `json:"period,omitempty"`

	// RealtimeRuntime 实时调度的 CPU 时间（微秒）
	RealtimeRuntime *int64 `json:"realtime_runtime,omitempty"`

	// RealtimePeriod 实时调度周期（微秒）
	RealtimePeriod *int64 `json:"realtime_period,omitempty"`

	// Cpus 绑定的 CPU 核心列表（如 "0-3,7"）
	Cpus *string `json:"cpus,omitempty"`

	// Mems 绑定的 NUMA 内存节点（如 "0-3"）
	Mems *string `json:"mems,omitempty"`
}

// Pids 进程数限制
type Pids struct {
	// Limit 最大进程数
	Limit *int64 `json:"limit,omitempty"`
}

// BlockIO 块设备 I/O 配置
type BlockIO struct {
	// Weight I/O 权重（10-1000，默认 500）
	Weight *uint16 `json:"weight,omitempty"`
}

// Device 设备白名单配置
type Device struct {
	// Allow 是否允许
	Allow bool `json:"allow"`

	// Type 设备类型（c=字符设备, b=块设备, a=全部）
	Type rune `json:"type"`

	// Major 主设备号（-1 表示全部）
	Major *int64 `json:"major,omitempty"`

	// Minor 次设备号（-1 表示全部）
	Minor *int64 `json:"minor,omitempty"`

	// Access 访问权限（r=读, w=写, m=mknod）
	Access string `json:"access,omitempty"`
}
