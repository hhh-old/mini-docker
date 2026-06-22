//go:build linux

package plugin

/*
=======================================================================
  Linux 特有插件注册 —— shim / runtime / task

  这三个插件依赖 Linux 内核特性（cgroup/namespace/overlayfs），
  因此仅在 Linux 平台注册。

=======================================================================
*/

import (
	"fmt"

	"mini-docker/containerd/containers"
	"mini-docker/containerd/events"
	"mini-docker/containerd/metadata"
	"mini-docker/containerd/runtime"
	"mini-docker/containerd/shim"
	"mini-docker/containerd/task"
)

func registerLinuxPlugins(m *Manager) {
	// ---- service.shim 插件 ----
	// Shim 进程管理服务，封装 shim 的连接、通信、创建、删除、重启等操作
	// 对齐 containerd: Shim Manager 是 Task Service 的底层依赖
	m.Register(&Plugin{
		Type: TypeService,
		ID:   "shim",
		Init: func(ic *InitContext) (interface{}, error) {
			metaDB, err := GetPlugin[*metadata.DB](ic, TypeService, "metadata")
			if err != nil {
				return nil, fmt.Errorf("获取 metadata 插件失败: %w", err)
			}
			return shim.NewShimManager(metaDB), nil
		},
		Depends: []PluginKey{
			{Type: TypeService, ID: "metadata"},
		},
	})

	// ---- service.runtime 插件 ----
	// Runtime 服务，提供 OCI Runtime 抽象层
	// 对齐 containerd: Runtime Service 是可插拔的 runtime 抽象（runc/kata/runsc）
	m.Register(&Plugin{
		Type: TypeService,
		ID:   "runtime",
		Init: func(ic *InitContext) (interface{}, error) {
			shimMgr, err := GetPlugin[*shim.ShimManager](ic, TypeService, "shim")
			if err != nil {
				return nil, fmt.Errorf("获取 shim 插件失败: %w", err)
			}
			return runtime.NewService(shimMgr), nil
		},
		Depends: []PluginKey{
			{Type: TypeService, ID: "shim"},
		},
	})

	// ---- service.task 插件 ----
	// Task 生命周期管理服务，管理容器的运行时生命周期
	// 对齐 containerd: Task Service 是容器运行时的核心服务
	// 依赖 Container Service 获取元数据，依赖 Runtime Service 执行底层运行时操作
	// TaskState 仅存内存，不持久化到 BoltDB（对齐 containerd: 运行时状态始终从 runtime/shim 实时获取）
	m.Register(&Plugin{
		Type: TypeService,
		ID:   "task",
		Init: func(ic *InitContext) (interface{}, error) {
			containersSvc, err := GetPlugin[*containers.Service](ic, TypeService, "containers")
			if err != nil {
				return nil, fmt.Errorf("获取 containers 插件失败: %w", err)
			}
			rtSvc, err := GetPlugin[*runtime.Service](ic, TypeService, "runtime")
			if err != nil {
				return nil, fmt.Errorf("获取 runtime 插件失败: %w", err)
			}
			evSvc, err := GetPlugin[*events.Service](ic, TypeService, "events")
			if err != nil {
				return nil, fmt.Errorf("获取 events 插件失败: %w", err)
			}
			return task.NewService(containersSvc, rtSvc, evSvc), nil
		},
		Depends: []PluginKey{
			{Type: TypeService, ID: "containers"},
			{Type: TypeService, ID: "runtime"},
			{Type: TypeService, ID: "events"},
		},
	})
}
