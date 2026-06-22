//go:build linux

package containerd

/*
=======================================================================
  快照管理处理器（对齐 Docker: containerd Snapshotter Service）
=======================================================================

  本文件只负责快照相关的请求路由：
  - 创建/删除容器可写层快照
  - 只读查看快照（View）
  - 查询快照元信息（Stat）
  - 更新快照元信息（Update）
  - 查询快照资源使用量（Usage）
  - 获取快照挂载信息（Mounts）
  - 获取快照的 diff 目录路径
  - 清理已移除/废弃快照的磁盘资源（Cleanup）

  GC 相关代码（handleGC、adapter 类型）已分离到：
  - handler_gc_linux.go    处理器
  - containerd/gc/adapter.go  适配器

  修复了之前的"厨房水槽"反模式：本文件不再承担 GC 适配器职责。

=======================================================================
*/

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"mini-docker/constants"
	"mini-docker/containerd/diff"
	"mini-docker/containerd/snapshots"
)

// ---------------------------------------------------------------------------
// 快照处理器
// ---------------------------------------------------------------------------

// handlePrepareSnapshot 创建容器可写层快照
// 它做三件事：
// 1. 在 <root>/snapshots/<id>/ 下创建 fs/ （可写层/内容层）和 work/ （overlay 工作目录）
// 2. 在 boltdb 里注册一条 SnapshotInfo（Kind=KindActive, Parent=<顶层镜像层 cacheID>）
// 3. 返回 []snapshots.Mount（通常只有一个 overlay mount），描述如何把上层可写 + 下层只读组合挂载成容器 rootfs
func (c *Containerd) handlePrepareSnapshot(req Request) Response {
	containerID := req.Args["key"]
	parent := req.Args["parent"]
	if containerID == "" {
		return Response{Success: false, Message: "快照 key 不能为空"}
	}
	if c.getSnapshotterService() == nil {
		return Response{Success: false, Message: "Snapshotter Service 未初始化"}
	}

	ctx := context.Background()
	mounts, err := c.getSnapshotterService().Prepare(ctx, containerID, parent)
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
	if c.getSnapshotterService() == nil {
		return Response{Success: false, Message: "Snapshotter Service 未初始化"}
	}

	ctx := context.Background()
	if err := c.getSnapshotterService().Remove(ctx, key); err != nil {
		return Response{Success: false, Message: fmt.Sprintf("删除快照失败: %v", err)}
	}

	return Response{Success: true}
}

// handleDiffPath 获取快照的 diff 目录路径
// 新接口移除了 DiffPath 方法，改为通过 Stat 获取快照 ID，再用 diff.FSDir 构造路径
func (c *Containerd) handleDiffPath(req Request) Response {
	key := req.Args["key"]
	if key == "" {
		return Response{Success: false, Message: "快照 key 不能为空"}
	}
	if c.getSnapshotterService() == nil {
		return Response{Success: false, Message: "Snapshotter Service 未初始化"}
	}

	ctx := context.Background()
	// 通过 Stat 获取快照元信息，拿到内部 ID
	info, err := c.getSnapshotterService().Stat(ctx, key)
	if err != nil {
		return Response{Success: false, Message: fmt.Sprintf("获取快照信息失败: %v", err)}
	}

	// 用 diff.FSDir 构造 fs/ 目录路径
	path := diff.FSDir(constants.SnapshotterDir, info.ID)

	return Response{Success: true, Data: map[string]interface{}{"path": path}}
}

// handleCommitSnapshot 提交快照（对齐 containerd: Snapshotter.Commit）
// builder 构建流程使用：RUN/COPY 指令执行完毕后，将 Active 快照提交为 Committed
// 新接口签名：Commit(ctx, name, key)，name 为提交后的快照名称，key 为源 Active 快照名称
func (c *Containerd) handleCommitSnapshot(req Request) Response {
	name := req.Args["name"]
	key := req.Args["key"]
	if key == "" {
		return Response{Success: false, Message: "快照 key 不能为空"}
	}
	if name == "" {
		return Response{Success: false, Message: "提交后的快照名称 name 不能为空"}
	}
	if c.getSnapshotterService() == nil {
		return Response{Success: false, Message: "Snapshotter Service 未初始化"}
	}

	ctx := context.Background()
	// 新接口：Commit(ctx, name, key) 返回 error，不再返回 []Mount
	if err := c.getSnapshotterService().Commit(ctx, name, key); err != nil {
		return Response{Success: false, Message: fmt.Sprintf("提交快照失败: %v", err)}
	}

	return Response{Success: true}
}

// handleWalkSnapshots 遍历所有快照（对齐 containerd: Snapshotter.Walk）
// builder 构建流程使用：构建完成后遍历快照，收集已 Commit 层的 digest
// 新接口 WalkFunc 签名：(ctx context.Context, info snapshots.Info) error
func (c *Containerd) handleWalkSnapshots(req Request) Response {
	if c.getSnapshotterService() == nil {
		return Response{Success: false, Message: "Snapshotter Service 未初始化"}
	}

	ctx := context.Background()
	var infos []snapshots.Info
	err := c.getSnapshotterService().Walk(ctx, func(ctx context.Context, info snapshots.Info) error {
		infos = append(infos, info)
		return nil
	})
	if err != nil {
		return Response{Success: false, Message: fmt.Sprintf("遍历快照失败: %v", err)}
	}

	return Response{Success: true, Data: map[string]interface{}{"snapshots": infos}}
}

// handleViewSnapshot 创建只读活跃快照（对齐 containerd: Snapshotter.View）
// 用于挂载查看镜像内容等只读场景，无 upperdir/workdir
func (c *Containerd) handleViewSnapshot(req Request) Response {
	key := req.Args["key"]
	parent := req.Args["parent"]
	if key == "" {
		return Response{Success: false, Message: "快照 key 不能为空"}
	}
	if c.getSnapshotterService() == nil {
		return Response{Success: false, Message: "Snapshotter Service 未初始化"}
	}

	ctx := context.Background()
	mounts, err := c.getSnapshotterService().View(ctx, key, parent)
	if err != nil {
		return Response{Success: false, Message: fmt.Sprintf("创建只读快照失败: %v", err)}
	}

	// 返回挂载信息
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

// handleStatSnapshot 查询快照元信息（对齐 containerd: Snapshotter.Stat）
// 返回指定快照的 Info，包含 ID、Name、Parent、Kind、Labels 等
func (c *Containerd) handleStatSnapshot(req Request) Response {
	key := req.Args["key"]
	if key == "" {
		return Response{Success: false, Message: "快照 key 不能为空"}
	}
	if c.getSnapshotterService() == nil {
		return Response{Success: false, Message: "Snapshotter Service 未初始化"}
	}

	ctx := context.Background()
	info, err := c.getSnapshotterService().Stat(ctx, key)
	if err != nil {
		return Response{Success: false, Message: fmt.Sprintf("查询快照信息失败: %v", err)}
	}

	return Response{Success: true, Data: map[string]interface{}{"info": info}}
}

// handleCleanup 清理已移除/废弃快照的磁盘资源（对齐 containerd: Snapshotter.Cleanup）
func (c *Containerd) handleCleanup(req Request) Response {
	if c.getSnapshotterService() == nil {
		return Response{Success: false, Message: "Snapshotter Service 未初始化"}
	}

	ctx := context.Background()
	if err := c.getSnapshotterService().Cleanup(ctx); err != nil {
		return Response{Success: false, Message: fmt.Sprintf("清理快照资源失败: %v", err)}
	}

	return Response{Success: true}
}

// handleMountsSnapshot 获取快照的挂载信息（对齐 containerd: Snapshotter.Mounts）
func (c *Containerd) handleMountsSnapshot(req Request) Response {
	key := req.Args["key"]
	if key == "" {
		return Response{Success: false, Message: "快照 key 不能为空"}
	}
	if c.getSnapshotterService() == nil {
		return Response{Success: false, Message: "Snapshotter Service 未初始化"}
	}

	ctx := context.Background()
	mounts, err := c.getSnapshotterService().Mounts(ctx, key)
	if err != nil {
		return Response{Success: false, Message: fmt.Sprintf("获取挂载信息失败: %v", err)}
	}

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

// handleUpdateSnapshot 更新快照元信息（对齐 containerd: Snapshotter.Update）
func (c *Containerd) handleUpdateSnapshot(req Request) Response {
	name := req.Args["name"]
	if name == "" {
		return Response{Success: false, Message: "快照名称 name 不能为空"}
	}
	if c.getSnapshotterService() == nil {
		return Response{Success: false, Message: "Snapshotter Service 未初始化"}
	}

	ctx := context.Background()

	// 构建要更新的 Info
	info := snapshots.Info{Name: name}
	if labelsJSON, ok := req.Args["labels"]; ok {
		if err := json.Unmarshal([]byte(labelsJSON), &info.Labels); err != nil {
			return Response{Success: false, Message: fmt.Sprintf("解析 labels 失败: %v", err)}
		}
	}

	// 解析 fieldpaths
	var fieldpaths []string
	if fp, ok := req.Args["fieldpaths"]; ok && fp != "" {
		fieldpaths = strings.Split(fp, ",")
	}

	updated, err := c.getSnapshotterService().Update(ctx, info, fieldpaths...)
	if err != nil {
		return Response{Success: false, Message: fmt.Sprintf("更新快照信息失败: %v", err)}
	}

	return Response{Success: true, Data: map[string]interface{}{"info": updated}}
}

// handleUsageSnapshot 查询快照资源使用量（对齐 containerd: Snapshotter.Usage）
func (c *Containerd) handleUsageSnapshot(req Request) Response {
	key := req.Args["key"]
	if key == "" {
		return Response{Success: false, Message: "快照 key 不能为空"}
	}
	if c.getSnapshotterService() == nil {
		return Response{Success: false, Message: "Snapshotter Service 未初始化"}
	}

	ctx := context.Background()
	usage, err := c.getSnapshotterService().Usage(ctx, key)
	if err != nil {
		return Response{Success: false, Message: fmt.Sprintf("查询资源使用量失败: %v", err)}
	}

	return Response{Success: true, Data: map[string]interface{}{"usage": usage}}
}
