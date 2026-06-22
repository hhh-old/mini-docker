package containerstore

/*
=======================================================================
  容器状态常量 —— 跨包共享的唯一权威定义

  对齐 containerd 架构分层原则：
  - containerstore 是容器元数据的共享包，daemon/task/runtime 等上层包均可引用
  - libcontainer 是底层运行时库，不应被 daemon/containerd 等上层包直接引用
  - 将状态常量提升到此包，消除上层包对 libcontainer 的跨层依赖

  状态定义与 libcontainer 完全一致，仅位置提升：
  - StatusCreated  容器已创建，进程已启动但等待 start 信号
  - StatusRunning  容器正在运行
  - StatusPaused   容器已暂停（cgroup freezer）
  - StatusStopped  容器已停止

=======================================================================
*/

// Status 容器状态类型
type Status = string

const (
	StatusCreated  Status = "created"
	StatusCreating Status = "creating"
	StatusRunning  Status = "running"
	StatusPaused   Status = "paused"
	StatusStopped  Status = "stopped"
)
