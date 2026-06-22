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

	// ContentStoreDir 是 Content Store 的根目录（对齐 containerd: io.containerd.content.v1.content/blobs/sha256/）
	// 存储所有 blob（manifest/config/layer 的原始压缩数据），以 digest 为文件名
	ContentStoreDir = MiniDockerRoot + "/content/sha256"

	// SnapshotterDir 是 OverlayFS Snapshotter 的根目录（对齐 containerd: io.containerd.snapshotter.v1.overlayfs/）
	// 存储解压后的镜像层快照
	SnapshotterDir = MiniDockerRoot + "/snapshots/overlay"

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

	// ContainerdSocketPath 是 containerd 独立进程的 Unix Socket 路径
	// 对齐 Docker: containerd 通过 /run/containerd/containerd.sock 与 dockerd 通信
	ContainerdSocketPath = MiniDockerRunRoot + "/containerd.sock"

	// DaemonConfigPath 是 daemon 配置文件路径（对齐 Docker: /etc/docker/daemon.json）
	DaemonConfigPath = "/etc/mini-docker/daemon.json"

	// ContainerdLogPath 是 containerd 独立进程的日志文件路径
	ContainerdLogPath = "/var/log/mini-docker/containerd.log"

	// ContainerdConfigPath 是 containerd 配置文件路径（对齐 containerd: /etc/containerd/config.toml）
	ContainerdConfigPath = "/etc/mini-docker/containerd.toml"
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

// Registry 相关常量
const (
	// DefaultRegistry 是默认的 Docker Registry 地址
	DefaultRegistry = "registry-1.docker.io"

	// DefaultRegistryHost 是 Docker Hub 的认证和 manifest 服务地址
	// Docker Hub 将 registry-1.docker.io 用于实际 blob 存储，
	// 而 index.docker.io 用于 manifest 和认证
	DefaultRegistryHost = "index.docker.io"

	// RegistryPullTimeout 是 Registry HTTP 请求超时时间
	RegistryPullTimeout = 30 * time.Minute

	// RegistryBlobChunkSize 是从 Registry 下载 blob 时的缓冲区大小 (1MB)
	RegistryBlobChunkSize = 1024 * 1024
)
