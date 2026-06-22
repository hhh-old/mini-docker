//go:build linux

package containerd

import (
	"encoding/json"
	"fmt"

	"mini-docker/containerd/containers"
	"mini-docker/containerd/metadata"
	"mini-docker/containerd/plugin"
)

/*
=======================================================================
  Container 元数据处理器 —— 对齐 containerd 的 containers.Store gRPC 服务

  真实 containerd 中，Container Service 是独立的 gRPC 服务，提供容器的
  元数据 CRUD（Create/Get/List/Update/Delete），与 Task Service 分离：
  - Container Service: 容器静态元数据（ID/Name/Image/Labels/Spec），容器停止后仍存在
  - Task Service: 容器动态运行时（PID/状态/退出码），容器停止后 Task 消失

  改造前：Daemon 直接调用 containerstore 包函数操作 boltdb，绕过 containerd
  改造后：Daemon 通过 RPC 调用 containerd 的 Container Service，对齐真实架构

=======================================================================
*/

// getContainersService 从插件管理器获取容器管理服务
func (c *Containerd) getContainersService() *containers.Service {
	inst, _ := c.plugins.Get(plugin.TypeService, "containers")
	if inst == nil {
		return nil
	}
	return inst.(*containers.Service)
}

// handleCreateContainer 创建容器元数据记录
// 对齐 containerd: containers.Store.Create(ctx, container)
func (c *Containerd) handleCreateContainer(req Request) Response {
	svc := c.getContainersService()
	if svc == nil {
		return Response{Success: false, Message: "容器服务未初始化"}
	}

	var info metadata.ContainerInfo
	if err := json.Unmarshal([]byte(req.Args["info"]), &info); err != nil {
		return Response{Success: false, Message: fmt.Sprintf("解析容器信息失败: %v", err)}
	}

	if err := svc.Create(&info); err != nil {
		return Response{Success: false, Message: fmt.Sprintf("创建容器失败: %v", err)}
	}

	return Response{Success: true, Data: info}
}

// handleGetContainer 查询容器元数据
// 对齐 containerd: containers.Store.Get(ctx, id)
// 支持 ID 和名称两种查询方式
func (c *Containerd) handleGetContainer(req Request) Response {
	svc := c.getContainersService()
	if svc == nil {
		return Response{Success: false, Message: "容器服务未初始化"}
	}

	id := req.Args["id"]
	if id == "" {
		return Response{Success: false, Message: "需要指定容器ID或名称"}
	}

	info, err := svc.Get(id)
	if err != nil {
		return Response{Success: false, Message: fmt.Sprintf("查询容器失败: %v", err)}
	}

	return Response{Success: true, Data: info}
}

// handleListContainers 列出所有容器元数据
// 对齐 containerd: containers.Store.List(ctx, filters)
func (c *Containerd) handleListContainers(req Request) Response {
	svc := c.getContainersService()
	if svc == nil {
		return Response{Success: false, Message: "容器服务未初始化"}
	}

	containers, err := svc.List()
	if err != nil {
		return Response{Success: false, Message: fmt.Sprintf("列出容器失败: %v", err)}
	}

	return Response{Success: true, Data: containers}
}

// handleUpdateContainer 更新容器元数据
// 对齐 containerd: containers.Store.Update(ctx, container, fieldpaths)
func (c *Containerd) handleUpdateContainer(req Request) Response {
	svc := c.getContainersService()
	if svc == nil {
		return Response{Success: false, Message: "容器服务未初始化"}
	}

	var info metadata.ContainerInfo
	if err := json.Unmarshal([]byte(req.Args["info"]), &info); err != nil {
		return Response{Success: false, Message: fmt.Sprintf("解析容器信息失败: %v", err)}
	}

	if err := svc.Update(&info); err != nil {
		return Response{Success: false, Message: fmt.Sprintf("更新容器失败: %v", err)}
	}

	return Response{Success: true, Data: info}
}

// handleDeleteContainer 删除容器元数据
// 对齐 containerd: containers.Store.Delete(ctx, id)
func (c *Containerd) handleDeleteContainer(req Request) Response {
	svc := c.getContainersService()
	if svc == nil {
		return Response{Success: false, Message: "容器服务未初始化"}
	}

	id := req.Args["id"]
	if id == "" {
		return Response{Success: false, Message: "需要指定容器ID"}
	}

	if err := svc.Delete(id); err != nil {
		return Response{Success: false, Message: fmt.Sprintf("删除容器失败: %v", err)}
	}

	return Response{Success: true, Message: fmt.Sprintf("容器 %s 元数据已删除", id)}
}
