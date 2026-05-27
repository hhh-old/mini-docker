package constants

import "time"

// 时间格式常量
const (
	// TimeFormat 是统一的时间格式常量
	TimeFormat = "2006-01-02 15:04:05"
)

// 存储路径常量
const (
	// MiniDockerRoot 是 mini-docker 的根存储路径
	MiniDockerRoot = "/var/lib/mini-docker"

	// MiniDockerRunRoot 是 mini-docker 的运行时根路径
	MiniDockerRunRoot = "/var/run/mini-docker"

	// ContainerStoreDir 是容器信息存储目录
	ContainerStoreDir = MiniDockerRunRoot

	// ContainerDataDir 是容器数据存储目录
	ContainerDataDir = MiniDockerRoot + "/containers"

	// ImageStoreDir 是镜像存储目录
	ImageStoreDir = MiniDockerRoot + "/images"

	// NetworkStoreDir 是网络存储目录
	NetworkStoreDir = MiniDockerRoot + "/networks"

	// VolumeStoreDir 是卷存储目录
	VolumeStoreDir = MiniDockerRoot + "/volumes"

	// RuntimeDir 是运行时存储目录
	RuntimeDir = MiniDockerRoot + "/runtime"

	// ShimDir 是 shim 进程存储目录
	ShimDir = MiniDockerRunRoot + "/shim"

	// SocketPath 是 daemon socket 路径
	SocketPath = MiniDockerRunRoot + "/mini-docker.sock"

	// DaemonPidFile 是 daemon PID 文件路径
	DaemonPidFile = MiniDockerRunRoot + "/daemon.pid"

	// DaemonLogPath 是 daemon 日志文件路径
	DaemonLogPath = "/var/log/mini-docker/daemon.log"
)

// 缓冲区大小常量
const (
	// DefaultBufferSize 是默认缓冲区大小 (64KB)
	DefaultBufferSize = 65536
)

// 超时时间常量
const (
	// DefaultConnectTimeout 是默认连接超时时间
	DefaultConnectTimeout = 30 * time.Second

	// LongOperationTimeout 是长操作超时时间（pull/build 等）
	LongOperationTimeout = 10 * time.Minute

	// ShimConnectTimeout 是 shim 连接超时时间
	ShimConnectTimeout = 5 * time.Second

	// SocketWaitTimeout 是 socket 等待超时时间
	SocketWaitTimeout = 15 * time.Second

	// PollInterval 是轮询间隔
	PollInterval = 100 * time.Millisecond

	// ShutdownWaitTime 是关闭等待时间
	ShutdownWaitTime = 500 * time.Millisecond
)

// 容器相关常量
const (
	DefaultTmpfsSize = "size=64m"

	CgroupPrefix        = "mini-docker-"
	CgroupRootPath      = "/sys/fs/cgroup"
	GracefulStopTimeout = 2 * time.Second
)

// 重启策略常量
const (
	DefaultMaxRetries  = 5
	RestartBackoffBase = 100 * time.Millisecond
	RestartBackoffMax  = 60 * time.Second
)

// 日志相关常量
const (
	// MaxContainerLogSize 容器日志文件最大大小 (10MB)
	MaxContainerLogSize = 10 * 1024 * 1024
)

// 网络相关常量
const (
	// DefaultSubnet 是默认子网
	DefaultSubnet = "172.33.0.0/16"

	// DefaultGateway 是默认网关
	DefaultGateway = "172.33.0.1"
)
