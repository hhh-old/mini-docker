//go:build linux

package containerd

/*
=======================================================================
  快照管理处理器（对齐 Docker: containerd Snapshotter Service）
=======================================================================

  本文件只负责快照相关的请求路由：
  - 创建/删除容器可写层快照
  - 注册已存在的快照元数据
  - 获取快照的 diff 目录路径

  GC 相关代码（handleGC、adapter 类型）已分离到：
  - handler_gc_linux.go    处理器
  - containerd/gc/adapter.go  适配器

  修复了之前的"厨房水槽"反模式：本文件不再承担 GC 适配器职责。

=======================================================================
*/

import (
	"context"
	"fmt"
)

// ---------------------------------------------------------------------------
// 快照处理器
// ---------------------------------------------------------------------------

// handlePrepareSnapshot 创建容器可写层快照
// 它做两件事：
// 1. 在 <root>/<containerID>/ 下创建 overlay 三件套 目录（diff/upper/work）
// 2. 在 boltdb 里注册一条 SnapshotInfo （ Kind=KindActive , ReadWrite=true , Parent=<顶层镜像层 cacheID> ）
// 3. 返回 []snapshots.Mount ，描述如何把上层可写 + 下层只读组合挂载成容器 rootfs
func (c *Containerd) handlePrepareSnapshot(req Request) Response {
	containerID := req.Args["key"]
	parent := req.Args["parent"]
	if containerID == "" {
		return Response{Success: false, Message: "快照 key 不能为空"}
	}
	if c.snapshotter == nil {
		return Response{Success: false, Message: "Snapshotter 未初始化"}
	}

	ctx := context.Background()
	mounts, err := c.snapshotter.Prepare(ctx, containerID, parent)
	if err != nil {
		return Response{Success: false, Message: fmt.Sprintf("创建快照失败: %v", err)}
	}

	// 返回挂载信息（包含 overlay 的 lowerdir、upperdir、workdir 路径）
	mountData := make([]map[string]interface{}, len(mounts))
	for i, m := range mounts {
		mountData[i] = map[string]interface{}{
			"type":    m.Type,
			"source":  m.Source,
			"options": m.Options,
		}
	}

	return Response{Success: true, Data: map[string]interface{}{"mounts": mountData}}
}

// handleRemoveSnapshot 删除容器快照
// 对齐 containerd: 通过 Snapshotter.Remove() 删除快照，同时清理 boltdb 中的元数据
func (c *Containerd) handleRemoveSnapshot(req Request) Response {
	key := req.Args["key"]
	if key == "" {
		return Response{Success: false, Message: "快照 key 不能为空"}
	}
	if c.snapshotter == nil {
		return Response{Success: false, Message: "Snapshotter 未初始化"}
	}

	ctx := context.Background()
	if err := c.snapshotter.Remove(ctx, key); err != nil {
		return Response{Success: false, Message: fmt.Sprintf("删除快照失败: %v", err)}
	}

	return Response{Success: true}
}

// handleRegisterCommitted 注册已存在的快照元数据（对齐 containerd: Snapshotter.RegisterCommitted）
// 用于 builder 等场景：文件已落盘但需要补注册元数据到 boltdb
func (c *Containerd) handleRegisterCommitted(req Request) Response {
	key := req.Args["key"]
	if key == "" {
		return Response{Success: false, Message: "快照 key 不能为空"}
	}
	parent := req.Args["parent"]
	if c.snapshotter == nil {
		return Response{Success: false, Message: "Snapshotter 未初始化"}
	}

	ctx := context.Background()
	if err := c.snapshotter.RegisterCommitted(ctx, key, parent); err != nil {
		return Response{Success: false, Message: fmt.Sprintf("注册快照元数据失败: %v", err)}
	}

	return Response{Success: true}
}

// handleDiffPath 获取快照的 diff 目录路径（对齐 containerd: Snapshotter.DiffPath）
func (c *Containerd) handleDiffPath(req Request) Response {
	key := req.Args["key"]
	if key == "" {
		return Response{Success: false, Message: "快照 key 不能为空"}
	}
	if c.snapshotter == nil {
		return Response{Success: false, Message: "Snapshotter 未初始化"}
	}

	ctx := context.Background()
	path, err := c.snapshotter.DiffPath(ctx, key)
	if err != nil {
		return Response{Success: false, Message: fmt.Sprintf("获取 diff 路径失败: %v", err)}
	}

	return Response{Success: true, Data: map[string]interface{}{"path": path}}
}
