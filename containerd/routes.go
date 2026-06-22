package containerd

/*
=======================================================================
  路由常量 —— containerd 服务端请求分发键
=======================================================================

  这些常量是 server 端 dispatch 的 key（与 client 端 SendRequest 的 Request.Type 字段对应）。
  放在独立文件 routes.go 中，让 api.go 只保留真正的协议结构体（Request/Response）。

  命名规范：
  - 镜像相关：ReqPullImage / ReqListImages / ReqRemoveImage / ReqInspectImage / ReqResolveImage / ReqRegisterImage
  - 任务相关：ReqCreateTask / ReqStartTask / ReqKillTask / ReqDeleteTask / ReqPauseTask / ReqResumeTask
  - Shim 生命周期：ReqShutdownShim / ReqRestartShim / ReqIsShimAlive / ReqReadShimPID
  - 任务状态：ReqGetTaskState / ReqGetExitInfo / ReqReadExitInfo / ReqWaitForCreate / ReqListTasks
  - 流式连接：ReqAttachTask / ReqExecTaskStream / ReqResizeTask
  - 快照：ReqPrepareSnapshot / ReqRemoveSnapshot / ReqDiffPath / ReqCommitSnapshot / ReqWalkSnapshots / ReqViewSnapshot / ReqStatSnapshot / ReqMountsSnapshot / ReqUpdateSnapshot / ReqUsageSnapshot / ReqCleanupSnapshot
  - 容器元数据：ReqCreateContainer / ReqGetContainer / ReqListContainers / ReqUpdateContainer / ReqDeleteContainer
  - 运维：ReqGC / ReqPing

=======================================================================
*/

const (
	// ReqCreateTask 创建容器任务（启动 shim + runtime create）
	ReqCreateTask = "create_task"
	// ReqStartTask 启动容器任务（通知 shim 执行 runtime start）
	ReqStartTask = "start_task"
	// ReqKillTask 向容器发送信号
	ReqKillTask = "kill_task"
	// ReqDeleteTask 删除容器任务（停止 shim + 清理 runtime 目录）
	ReqDeleteTask = "delete_task"
	// ReqShutdownShim 仅关闭 shim（比 DeleteTask 更轻量，用于自然退出的容器）
	ReqShutdownShim = "shutdown_shim"
	// ReqRestartShim 重启 shim 以接管已有容器
	ReqRestartShim = "restart_shim"
	// ReqGetTaskState 获取容器任务状态
	ReqGetTaskState = "get_task_state"
	// ReqGetExitInfo 获取容器退出信息
	ReqGetExitInfo = "get_exit_info"
	// ReqWaitForCreate 等待容器创建完成
	ReqWaitForCreate = "wait_for_create"
	// ReqListTasks 列出所有容器任务
	ReqListTasks = "list_tasks"
	// ReqPauseTask 暂停容器任务
	ReqPauseTask = "pause_task"
	// ReqResumeTask 恢复容器任务
	ReqResumeTask = "resume_task"
	// ReqAttachTask 连接到容器的 TTY（流式连接）
	ReqAttachTask = "attach_task"
	// ReqExecTaskStream 在容器内执行命令（流式连接）
	ReqExecTaskStream = "exec_task_stream"
	// ReqResizeTask 调整容器终端大小
	ReqResizeTask = "resize_task"
	// ReqIsShimAlive 检查 shim 进程是否存活
	ReqIsShimAlive = "is_shim_alive"
	// ReqReadShimPID 读取 shim PID
	ReqReadShimPID = "read_shim_pid"
	// ReqReadExitInfo 读取退出信息（从磁盘文件）
	ReqReadExitInfo = "read_exit_info"
	// ReqPullImage 拉取镜像
	ReqPullImage = "pull_image"
	// ReqListImages 列出镜像
	ReqListImages = "list_images"
	// ReqRemoveImage 删除镜像
	ReqRemoveImage = "remove_image"
	// ReqInspectImage 查看镜像详情
	ReqInspectImage = "inspect_image"
	// ReqResolveImage 解析镜像引用，返回 snapshot ID
	ReqResolveImage = "resolve_image"
	// ReqRegisterImage 注册一个已构建好的镜像（builder 调用）
	ReqRegisterImage = "register_image"
	// ReqCommitContainer 将容器的可写层提交为新镜像（对齐 docker commit）
	ReqCommitContainer = "commit_container"
	// ReqCreateContainer 创建容器元数据记录（对齐 containerd: containers.Store.Create）
	ReqCreateContainer = "create_container"
	// ReqGetContainer 查询容器元数据（对齐 containerd: containers.Store.Get）
	ReqGetContainer = "get_container"
	// ReqListContainers 列出所有容器元数据（对齐 containerd: containers.Store.List）
	ReqListContainers = "list_containers"
	// ReqUpdateContainer 更新容器元数据（对齐 containerd: containers.Store.Update）
	ReqUpdateContainer = "update_container"
	// ReqDeleteContainer 删除容器元数据（对齐 containerd: containers.Store.Delete）
	ReqDeleteContainer = "delete_container"
	// ReqGC 手动触发垃圾回收
	ReqGC = "gc"
	// ReqPrepareSnapshot 创建容器可写层快照（对齐 containerd: Snapshotter.Prepare）
	ReqPrepareSnapshot = "prepare_snapshot"
	// ReqRemoveSnapshot 删除容器快照（对齐 containerd: Snapshotter.Remove）
	ReqRemoveSnapshot = "remove_snapshot"
	// ReqDiffPath 获取快照的 diff 目录路径（通过 Stat + diff.FSDir 实现）
	ReqDiffPath = "diff_path"
	// ReqCommitSnapshot 提交快照（对齐 containerd: Snapshotter.Commit，builder 构建流程使用）
	ReqCommitSnapshot = "commit_snapshot"
	// ReqWalkSnapshots 遍历快照（对齐 containerd: Snapshotter.Walk，builder 构建流程使用）
	ReqWalkSnapshots = "walk_snapshots"
	// ReqViewSnapshot 创建只读活跃快照（对齐 containerd: Snapshotter.View）
	ReqViewSnapshot = "view_snapshot"
	// ReqStatSnapshot 查询快照元信息（对齐 containerd: Snapshotter.Stat）
	ReqStatSnapshot = "stat_snapshot"
	// ReqCleanupSnapshot 清理已移除/废弃快照的磁盘资源（对齐 containerd: Snapshotter.Cleanup）
	ReqCleanupSnapshot = "cleanup_snapshot"
	// ReqMountsSnapshot 获取快照的挂载信息（对齐 containerd: Snapshotter.Mounts）
	ReqMountsSnapshot = "mounts_snapshot"
	// ReqUpdateSnapshot 更新快照元信息（对齐 containerd: Snapshotter.Update）
	ReqUpdateSnapshot = "update_snapshot"
	// ReqUsageSnapshot 查询快照资源使用量（对齐 containerd: Snapshotter.Usage）
	ReqUsageSnapshot = "usage_snapshot"
	// ReqPing 健康检查
	ReqPing = "ping"
	// ReqContentInfo 查询 blob 元信息（对齐 containerd: Content Store Info）
	ReqContentInfo = "content_info"
	// ReqContentPath 获取 blob 本地存储路径（对齐 containerd: Content Store Path）
	ReqContentPath = "content_path"
	// ReqContentExists 检查 blob 是否存在（对齐 containerd: Content Store Exists）
	ReqContentExists = "content_exists"
	// ReqContentDelete 删除 blob（对齐 containerd: Content Store Delete）
	ReqContentDelete = "content_delete"
	// ReqContentWrite 写入 blob 数据（对齐 containerd: Content Store Writer）
	ReqContentWrite = "content_write"
	// ReqContentCommit 提交 blob 并校验 digest（对齐 containerd: Content Store Commit）
	ReqContentCommit = "content_commit"
	// ReqContentWalk 遍历所有 blob 元信息（对齐 containerd: Content Store Walk）
	ReqContentWalk = "content_walk"
	// ReqContentUpdate 更新 blob 标签（对齐 containerd: Content Store Update）
	ReqContentUpdate = "content_update"

	// ReqDiffApply 将层差异应用到 Active 快照（对齐 containerd: Diff Apply）
	ReqDiffApply = "diff_apply"
	// ReqDiffDiff 计算两个快照之间的差异（对齐 containerd: Diff Diff）
	ReqDiffDiff = "diff_diff"

	// ReqPublishEvent 发布事件到 containerd 事件总线
	ReqPublishEvent = "publish_event"
	// ReqSubscribeEvents 订阅事件流（长连接）
	ReqSubscribeEvents = "subscribe_events"
	// ReqGetEventArchive 获取事件归档（支持 since/until 时间过滤）
	ReqGetEventArchive = "get_event_archive"
)
