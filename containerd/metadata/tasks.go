package metadata

/*
=======================================================================
  TaskState —— 容器运行时状态（仅存内存，不持久化）

  对齐 containerd 架构原则：
  - ContainerInfo（静态元数据）持久化到 BoltDB
  - TaskState（运行时状态）仅存内存，始终从 shim/runtime 实时获取
  - 网络/IP/cgroup/退出码/用户停止标记/重启计数等运行时/历史状态
    由 Daemon 自己维护，存储在 containerstore.ContainerRuntimeState 中

  真实 containerd 中 Task 状态的来源是 shim 进程本身，
  containerd 每次 Get/State 都通过 TTRPC 调用 shim 获取实时状态。
  TaskState 的 sync.Map 缓存仅用于 Daemon 写入运行时状态的过渡场景
  （如 restoreContainers 中构建初始状态），不用于 GetState 的返回。

=======================================================================
*/

// TaskState 容器运行时状态（仅存内存，不持久化）
type TaskState struct {
	ContainerID string `json:"container_id"`
	PID         int    `json:"pid"`
	ShimPID     int    `json:"shim_pid"`
	Status      string `json:"status"`
}
