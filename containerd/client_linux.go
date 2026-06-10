//go:build linux

package containerd

/*
=======================================================================
  Client —— Daemon 通过此客户端与 containerd 独立进程通信

  对齐 Docker 的 C/S 架构：
  ┌──────────┐    Unix Socket     ┌──────────────┐
  │ Daemon   │ ───────────────→  │  containerd  │
  │ (client) │  RPC 调用          │  (server)    │
  └──────────┘                    └──────────────┘

  Client 提供与原 Service 相同的方法签名，但底层通过 Unix Socket
  调用 containerd 独立进程的 API，而非本地函数调用。

  对齐 Docker: dockerd 不直接操作 shim/runc，而是通过 containerd 的
  gRPC API 间接管理容器生命周期。

=======================================================================
*/

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"mini-docker/constants"
	"mini-docker/containerd/gc"
	"mini-docker/containerd/metadata"
	"mini-docker/containerd/snapshots"
	"mini-docker/containerstore"
	"mini-docker/libcontainer"
	"mini-docker/types"

	"golang.org/x/sys/unix"
)

// Client containerd 远程客户端（Daemon 侧使用）
// 对齐 Docker: dockerd 通过 containerd 的 gRPC 客户端与 containerd 通信
type Client struct {
	socketPath string
	timeout    time.Duration
}

// NewClient 创建 containerd 客户端
func NewClient() *Client {
	return &Client{
		socketPath: constants.ContainerdSocketPath,
		timeout:    constants.DefaultConnectTimeout,
	}
}

// CreateTask 创建容器任务（对齐原 Service.CreateTask）
func (c *Client) CreateTask(info *containerstore.ContainerInfo) (shimPID int, err error) {
	resp, err := SendRequest(Request{
		Type: ReqCreateTask,
		Args: map[string]string{"container_id": info.ID},
	})
	if err != nil {
		return 0, err
	}
	if !resp.Success {
		return 0, fmt.Errorf("%s", resp.Message)
	}

	data, _ := json.Marshal(resp.Data)
	var result struct {
		ShimPID int `json:"shim_pid"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return 0, fmt.Errorf("解析 shim PID 失败: %w", err)
	}
	return result.ShimPID, nil
}

// KillTask 向容器发送信号（对齐原 Service.KillTask）
func (c *Client) KillTask(containerID string, signal syscall.Signal) error {
	resp, err := SendRequest(Request{
		Type: ReqKillTask,
		Args: map[string]string{
			"container_id": containerID,
			"signal":       strconv.Itoa(int(signal)),
		},
	})
	if err != nil {
		return err
	}
	if !resp.Success {
		return fmt.Errorf("%s", resp.Message)
	}
	return nil
}

// GetTaskState 获取容器任务状态（对齐原 Service.GetTaskState）
func (c *Client) GetTaskState(containerID string) (*libcontainer.ContainerState, error) {
	resp, err := SendRequest(Request{
		Type: ReqGetTaskState,
		Args: map[string]string{"container_id": containerID},
	})
	if err != nil {
		return nil, err
	}
	if !resp.Success {
		return nil, fmt.Errorf("%s", resp.Message)
	}

	data, _ := json.Marshal(resp.Data)
	var state libcontainer.ContainerState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("解析任务状态失败: %w", err)
	}
	return &state, nil
}

// GetExitInfo 获取容器退出信息（对齐原 Service.GetExitInfo）
func (c *Client) GetExitInfo(containerID string) (*ExitInfo, error) {
	resp, err := SendRequest(Request{
		Type: ReqGetExitInfo,
		Args: map[string]string{"container_id": containerID},
	})
	if err != nil {
		return nil, err
	}
	if !resp.Success {
		return nil, fmt.Errorf("%s", resp.Message)
	}

	data, _ := json.Marshal(resp.Data)
	var info ExitInfo
	if err := json.Unmarshal(data, &info); err != nil {
		return nil, fmt.Errorf("解析退出信息失败: %w", err)
	}
	return &info, nil
}

// DeleteTask 删除容器任务（对齐原 Service.DeleteTask）
func (c *Client) DeleteTask(containerID string) error {
	resp, err := SendRequest(Request{
		Type: ReqDeleteTask,
		Args: map[string]string{"container_id": containerID},
	})
	if err != nil {
		return err
	}
	if !resp.Success {
		return fmt.Errorf("%s", resp.Message)
	}
	return nil
}

// ShutdownShim 关闭 shim（对齐原 Service.ShutdownShim）
func (c *Client) ShutdownShim(containerID string) {
	SendRequest(Request{
		Type: ReqShutdownShim,
		Args: map[string]string{"container_id": containerID},
	})
}

// RestartShim 重启 shim 以接管容器（对齐原 Service.RestartShim）
func (c *Client) RestartShim(containerID string, containerPID int) (int, error) {
	resp, err := SendRequest(Request{
		Type: ReqRestartShim,
		Args: map[string]string{
			"container_id":  containerID,
			"container_pid": strconv.Itoa(containerPID),
		},
	})
	if err != nil {
		return 0, err
	}
	if !resp.Success {
		return 0, fmt.Errorf("%s", resp.Message)
	}

	data, _ := json.Marshal(resp.Data)
	var result struct {
		ShimPID int `json:"shim_pid"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return 0, fmt.Errorf("解析 shim PID 失败: %w", err)
	}
	return result.ShimPID, nil
}

// WaitForCreate 等待容器创建完成（对齐原 Service.WaitForCreate）
func (c *Client) WaitForCreate(containerID string, timeout time.Duration) (int, error) {
	resp, err := SendRequest(Request{
		Type: ReqWaitForCreate,
		Args: map[string]string{
			"container_id": containerID,
			"timeout":      strconv.Itoa(int(timeout.Milliseconds())),
		},
	})
	if err != nil {
		return 0, err
	}
	if !resp.Success {
		return 0, fmt.Errorf("%s", resp.Message)
	}

	data, _ := json.Marshal(resp.Data)
	var result struct {
		Pid int `json:"pid"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return 0, fmt.Errorf("解析 PID 失败: %w", err)
	}
	return result.Pid, nil
}

// StartTask 启动容器任务（对齐原 Service.StartTask）
func (c *Client) StartTask(containerID string) error {
	resp, err := SendRequest(Request{
		Type: ReqStartTask,
		Args: map[string]string{"container_id": containerID},
	})
	if err != nil {
		return err
	}
	if !resp.Success {
		return fmt.Errorf("%s", resp.Message)
	}
	return nil
}

// PauseTask 暂停容器任务（对齐原 Service.PauseTask）
func (c *Client) PauseTask(containerID string) error {
	resp, err := SendRequest(Request{
		Type: ReqPauseTask,
		Args: map[string]string{"container_id": containerID},
	})
	if err != nil {
		return err
	}
	if !resp.Success {
		return fmt.Errorf("%s", resp.Message)
	}
	return nil
}

// ResumeTask 恢复容器任务（对齐原 Service.ResumeTask）
func (c *Client) ResumeTask(containerID string) error {
	resp, err := SendRequest(Request{
		Type: ReqResumeTask,
		Args: map[string]string{"container_id": containerID},
	})
	if err != nil {
		return err
	}
	if !resp.Success {
		return fmt.Errorf("%s", resp.Message)
	}
	return nil
}

// AttachTask 连接到容器 TTY（对齐原 Service.AttachTask）
// 返回一个可用于双向 I/O 转发的连接
func (c *Client) AttachTask(containerID string) (net.Conn, error) {
	conn, resp, err := SendStreamRequest(Request{
		Type: ReqAttachTask,
		Args: map[string]string{"container_id": containerID},
	})
	if err != nil {
		return nil, err
	}
	if resp != nil && !resp.Success {
		conn.Close()
		return nil, fmt.Errorf("attach 失败: %s", resp.Message)
	}
	return conn, nil
}

// ExecTaskStream 在容器内执行命令（对齐原 Service.ExecTaskStream）
// 返回一个可用于 I/O 转发的连接
func (c *Client) ExecTaskStream(containerID string, args []string, tty bool) (net.Conn, error) {
	argsJSON, _ := json.Marshal(args)
	conn, resp, err := SendStreamRequest(Request{
		Type: ReqExecTaskStream,
		Args: map[string]string{
			"container_id": containerID,
			"tty":          strconv.FormatBool(tty),
			"args_json":    string(argsJSON),
		},
	})
	if err != nil {
		return nil, err
	}
	if resp != nil && !resp.Success {
		conn.Close()
		return nil, fmt.Errorf("%s", resp.Message)
	}
	return conn, nil
}

// ResizeTask 调整容器终端大小（对齐原 Service.ResizeTask）
func (c *Client) ResizeTask(containerID string, rows, cols uint16) error {
	resp, err := SendRequest(Request{
		Type: ReqResizeTask,
		Args: map[string]string{
			"container_id": containerID,
			"rows":         strconv.Itoa(int(rows)),
			"cols":         strconv.Itoa(int(cols)),
		},
	})
	if err != nil {
		return err
	}
	if !resp.Success {
		return fmt.Errorf("%s", resp.Message)
	}
	return nil
}

// ListTasks 列出所有容器任务（对齐原 Service.ListTasks）
func (c *Client) ListTasks() ([]*libcontainer.ContainerState, error) {
	resp, err := SendRequest(Request{
		Type: ReqListTasks,
		Args: map[string]string{},
	})
	if err != nil {
		return nil, err
	}
	if !resp.Success {
		return nil, fmt.Errorf("%s", resp.Message)
	}

	data, _ := json.Marshal(resp.Data)
	var states []*libcontainer.ContainerState
	if err := json.Unmarshal(data, &states); err != nil {
		return nil, fmt.Errorf("解析任务列表失败: %w", err)
	}
	return states, nil
}

// IsShimAlive 通过 containerd 检查 shim 是否存活
func (c *Client) IsShimAlive(containerID string) bool {
	resp, err := SendRequest(Request{
		Type: ReqIsShimAlive,
		Args: map[string]string{"container_id": containerID},
	})
	if err != nil {
		return false
	}
	if !resp.Success {
		return false
	}
	data, _ := json.Marshal(resp.Data)
	var result struct {
		Alive bool `json:"alive"`
	}
	if json.Unmarshal(data, &result) != nil {
		return false
	}
	return result.Alive
}

// ReadShimPID 通过 containerd 读取 shim PID
func (c *Client) ReadShimPID(containerID string) int {
	resp, err := SendRequest(Request{
		Type: ReqReadShimPID,
		Args: map[string]string{"container_id": containerID},
	})
	if err != nil {
		return 0
	}
	if !resp.Success {
		return 0
	}
	data, _ := json.Marshal(resp.Data)
	var result struct {
		Pid int `json:"pid"`
	}
	if json.Unmarshal(data, &result) != nil {
		return 0
	}
	return result.Pid
}

// ReadExitInfo 通过 containerd 读取退出信息
func (c *Client) ReadExitInfo(containerID string) (*ExitInfo, error) {
	resp, err := SendRequest(Request{
		Type: ReqReadExitInfo,
		Args: map[string]string{"container_id": containerID},
	})
	if err != nil {
		return nil, err
	}
	if !resp.Success {
		return nil, fmt.Errorf("%s", resp.Message)
	}

	data, _ := json.Marshal(resp.Data)
	var info ExitInfo
	if err := json.Unmarshal(data, &info); err != nil {
		return nil, fmt.Errorf("解析退出信息失败: %w", err)
	}
	return &info, nil
}

// PullImage 拉取镜像（流式进度推送）
func (c *Client) PullImage(imageName string, progressFn func(ProgressFrameData)) (*metadata.Image, error) {
	resp, err := SendStreamProgressRequest(Request{
		Type: ReqPullImage,
		Args: map[string]string{"image": imageName},
	}, progressFn)
	if err != nil {
		return nil, err
	}
	if !resp.Success {
		return nil, fmt.Errorf("%s", resp.Message)
	}

	data, _ := json.Marshal(resp.Data)
	var info metadata.Image
	if err := json.Unmarshal(data, &info); err != nil {
		return nil, fmt.Errorf("解析镜像信息失败: %w", err)
	}
	return &info, nil
}

// ListImages 列出所有镜像
func (c *Client) ListImages() ([]*metadata.Image, error) {
	resp, err := SendRequest(Request{
		Type: ReqListImages,
		Args: map[string]string{},
	})
	if err != nil {
		return nil, err
	}
	if !resp.Success {
		return nil, fmt.Errorf("%s", resp.Message)
	}

	data, _ := json.Marshal(resp.Data)
	var list []*metadata.Image
	if err := json.Unmarshal(data, &list); err != nil {
		return nil, fmt.Errorf("解析镜像列表失败: %w", err)
	}
	return list, nil
}

// RemoveImage 删除镜像
// 对齐 Docker: imageRef 支持 name:tag 和 imageID 两种格式
// 解析由 metadata.ResolveImageRef 统一处理，客户端只需传递完整引用
func (c *Client) RemoveImage(imageRef string) error {
	resp, err := SendRequest(Request{
		Type: ReqRemoveImage,
		Args: map[string]string{"image": imageRef},
	})
	if err != nil {
		return err
	}
	if !resp.Success {
		return fmt.Errorf("%s", resp.Message)
	}
	return nil
}

// InspectImage 查看镜像详情
func (c *Client) InspectImage(imageRef string) (*metadata.Image, error) {
	resp, err := SendRequest(Request{
		Type: ReqInspectImage,
		Args: map[string]string{"image": imageRef},
	})
	if err != nil {
		return nil, err
	}
	if !resp.Success {
		return nil, fmt.Errorf("%s", resp.Message)
	}

	data, _ := json.Marshal(resp.Data)
	var manifest metadata.Image
	if err := json.Unmarshal(data, &manifest); err != nil {
		return nil, fmt.Errorf("解析镜像清单失败: %w", err)
	}
	return &manifest, nil
}

// ResolveImage 解析镜像引用，返回最顶层快照 ID（用于 PrepareSnapshot 的 parent）
func (c *Client) ResolveImage(imageRef string) (string, error) {
	resp, err := SendRequest(Request{
		Type: ReqResolveImage,
		Args: map[string]string{"image": imageRef},
	})
	if err != nil {
		return "", err
	}
	if !resp.Success {
		return "", fmt.Errorf("%s", resp.Message)
	}

	data, _ := json.Marshal(resp.Data)
	var result struct {
		TopLayerSnapshotID string `json:"top_layer_snapshot_id"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return "", fmt.Errorf("解析镜像 snapshot ID 失败: %w", err)
	}
	return result.TopLayerSnapshotID, nil
}

// RegisterImage 注册一个已构建好的镜像（builder 使用）
// 对齐 containerd: 通过 RPC 将完整的 metadata.Image 传输到服务端
// 嵌套结构（Config、Annotations）序列化为 JSON 字符串传输，避免 map[string]string 无法表达复杂类型
func (c *Client) RegisterImage(info *metadata.Image) error {
	configJSON, _ := json.Marshal(info.Config)
	annotationsJSON, _ := json.Marshal(info.Annotations)
	args := map[string]string{
		"name":                  info.Name,
		"tag":                   info.Tag,
		"image_id":              info.ImageID,
		"created_at":            info.CreatedAt,
		"top_layer_snapshot_id": info.TopLayerSnapshotID,
		"layer_digests":         strings.Join(info.LayerDigests, ","),
		"config":                string(configJSON),
		"size":                  info.Size,
		"config_digest":         info.ConfigDigest,
		"annotations":           string(annotationsJSON),
	}
	resp, err := SendRequest(Request{
		Type: ReqRegisterImage,
		Args: args,
	})
	if err != nil {
		return err
	}
	if !resp.Success {
		return fmt.Errorf("%s", resp.Message)
	}
	return nil
}

// GC 手动触发垃圾回收
func (c *Client) GC() (*gc.GCStats, error) {
	resp, err := SendRequest(Request{
		Type: ReqGC,
		Args: map[string]string{},
	})
	if err != nil {
		return nil, err
	}
	if !resp.Success {
		return nil, fmt.Errorf("%s", resp.Message)
	}

	data, _ := json.Marshal(resp.Data)
	var stats gc.GCStats
	if err := json.Unmarshal(data, &stats); err != nil {
		return nil, fmt.Errorf("解析 GC 统计失败: %w", err)
	}
	return &stats, nil
}

// PrepareSnapshot 创建容器可写层快照并在宿主机上挂载 OverlayFS
// 对齐 containerd: 通过 Snapshotter.Prepare() 创建可写快照，注册元数据到 boltdb，
// 然后在宿主机上执行 overlay mount，使 merged 目录成为真正的容器 rootfs。
// containerID: 容器 ID（作为快照 key）
// topLayerSnapshotID: 父快照 key（镜像最顶层快照的 cacheID）
// 返回 OverlayDirs 信息（merged 路径已挂载为 overlay 文件系统）
//
// 对齐真实 containerd 行为：overlay mount 在宿主机上完成（而非容器 init 进程内），
// runc/容器 init 只需 pivot_root 到已挂载的 merged 目录即可。
func (c *Client) PrepareSnapshot(containerID, topLayerSnapshotID string) (*types.OverlayDirs, error) {
	mounts, err := sendPrepareSnapshotRPC(containerID, topLayerSnapshotID)
	if err != nil {
		return nil, err
	}

	overlayDirs := &types.OverlayDirs{}
	for _, m := range mounts {
		if m.Type == "overlay" {
			for _, opt := range m.Options {
				if strings.HasPrefix(opt, "upperdir=") {
					overlayDirs.Upper = strings.TrimPrefix(opt, "upperdir=")
				} else if strings.HasPrefix(opt, "workdir=") {
					overlayDirs.Work = strings.TrimPrefix(opt, "workdir=")
				} else if strings.HasPrefix(opt, "lowerdir=") {
					overlayDirs.Lower = strings.TrimPrefix(opt, "lowerdir=")
				}
			}
		}
	}

	// merged 目录在快照目录下
	if overlayDirs.Upper != "" {
		overlayDirs.Merged = filepath.Join(filepath.Dir(overlayDirs.Upper), "merged")
		if err := os.MkdirAll(overlayDirs.Merged, 0755); err != nil {
			return nil, fmt.Errorf("创建 merged 目录失败: %w", err)
		}

		// 对齐 containerd: 在宿主机上执行 overlay mount
		// merged 目录从空目录变为真正的 overlay 文件系统
		options := fmt.Sprintf("lowerdir=%s,upperdir=%s,workdir=%s",
			overlayDirs.Lower, overlayDirs.Upper, overlayDirs.Work)
		if err := unix.Mount("overlay", overlayDirs.Merged, "overlay", 0, options); err != nil {
			return nil, fmt.Errorf("宿主机 overlay mount 失败: %w", err)
		}
	}

	return overlayDirs, nil
}

// Snapshotter 返回一个基于远程调用的 Snapshotter 代理
// 对齐 containerd: Daemon 通过此代理调用 containerd 服务端的 Snapshotter 方法
// 代理通过 Unix Socket 转发 DiffPath 等调用到服务端
func (c *Client) Snapshotter() snapshots.Snapshotter {
	return &clientSnapshotter{client: c}
}

// clientSnapshotter 基于 Unix Socket 的远程 Snapshotter 代理
// 对齐 containerd: Daemon 不直接持有 Snapshotter，而是通过 Unix Socket RPC 代理调用
//
// 本代理实现 snapshots.Snapshotter 接口：
//   - RPC 转发（支持远程）：Prepare、Remove、DiffPath、Close
//   - 本地不支持（服务器内部操作）：Mounts、Commit、Walk、Apply
//
// Prepare/Remove/DiffPath 通过对应 RPC 路由调用
// Mounts/Commit/Walk/Apply 是 Snapshotter 内部细节，
// 不需要也不应该跨进程调用
type clientSnapshotter struct {
	client *Client
}

// Prepare 通过 RPC 创建容器可写层快照
// 对齐 containerd: Snapshotter.Prepare 是 client 唯一能调用的"准备快照"方法，
// 之前返回 "应通过 PrepareSnapshot API 调用" 的错误误导调用方，
// 现在代理直接转发到 server，调用方拿到的就是标准 snapshots.Mount 列表
func (cs *clientSnapshotter) Prepare(ctx context.Context, key, parent string) ([]snapshots.Mount, error) {
	return sendPrepareSnapshotRPC(key, parent)
}

// sendPrepareSnapshotRPC 是 Client.PrepareSnapshot 和 clientSnapshotter.Prepare
// 共用的 RPC 调用 + JSON 解析逻辑，避免两条 API 路径协议不同步
// 对齐 containerd: SendRequest 返回的 Response.Data 是 map[string]interface{}，
// 需要重新 marshal/unmarshal 到强类型结构
func sendPrepareSnapshotRPC(containerID, topLayerSnapshotID string) ([]snapshots.Mount, error) {
	resp, err := SendRequest(Request{
		Type: ReqPrepareSnapshot,
		Args: map[string]string{
			"key":    containerID,
			"parent": topLayerSnapshotID,
		},
	})
	if err != nil {
		return nil, err
	}
	if !resp.Success {
		return nil, fmt.Errorf("%s", resp.Message)
	}

	data, _ := json.Marshal(resp.Data)
	// 服务端返回 {"mounts": [{...}]}, 需要先提取 mounts 字段
	var wrapper struct {
		Mounts []snapshots.Mount `json:"mounts"`
	}
	if err := json.Unmarshal(data, &wrapper); err != nil {
		return nil, fmt.Errorf("解析挂载信息失败: %w", err)
	}
	return wrapper.Mounts, nil
}

// Mounts 是 Snapshotter 内部细节（返回 overlay mount 句柄），不支持跨进程调用
func (cs *clientSnapshotter) Mounts(ctx context.Context, key string) ([]snapshots.Mount, error) {
	return nil, fmt.Errorf("clientSnapshotter: Mounts 是 Snapshotter 内部细节，不支持远程调用")
}

// Commit 通过 RPC 提交快照（对齐 containerd: Snapshotter.Commit）
// builder 构建流程使用：RUN/COPY 指令执行完毕后，将 Active 快照提交为 Committed
func (cs *clientSnapshotter) Commit(ctx context.Context, key string) ([]snapshots.Mount, error) {
	resp, err := SendRequest(Request{
		Type: ReqCommitSnapshot,
		Args: map[string]string{"key": key},
	})
	if err != nil {
		return nil, err
	}
	if !resp.Success {
		return nil, fmt.Errorf("%s", resp.Message)
	}

	data, _ := json.Marshal(resp.Data)
	var wrapper struct {
		Mounts []snapshots.Mount `json:"mounts"`
	}
	if err := json.Unmarshal(data, &wrapper); err != nil {
		return nil, fmt.Errorf("解析挂载信息失败: %w", err)
	}
	return wrapper.Mounts, nil
}

// CommitAs 是 Snapshotter 内部细节（创建新 Committed 快照），不支持跨进程调用
func (cs *clientSnapshotter) CommitAs(ctx context.Context, name, key string) ([]snapshots.Mount, error) {
	return nil, fmt.Errorf("clientSnapshotter: CommitAs 是 Snapshotter 内部细节，不支持远程调用")
}

// Remove 通过 RPC 删除容器快照
// 对齐 containerd: Snapshotter.Remove 唯一会跨进程调用的删除方法
// 直接发 RPC，不再绕道 Client.RemoveSnapshot（已删除，避免两条 API 路径）
func (cs *clientSnapshotter) Remove(ctx context.Context, key string) error {
	resp, err := SendRequest(Request{
		Type: ReqRemoveSnapshot,
		Args: map[string]string{"key": key},
	})
	if err != nil {
		return err
	}
	if !resp.Success {
		return fmt.Errorf("%s", resp.Message)
	}
	return nil
}

// Walk 通过 RPC 遍历所有快照（对齐 containerd: Snapshotter.Walk）
// builder 构建流程使用：构建完成后遍历快照，收集已 Commit 层的 digest
func (cs *clientSnapshotter) Walk(ctx context.Context, fn func(snapshots.Info) error) error {
	resp, err := SendRequest(Request{
		Type: ReqWalkSnapshots,
		Args: map[string]string{},
	})
	if err != nil {
		return err
	}
	if !resp.Success {
		return fmt.Errorf("%s", resp.Message)
	}

	data, _ := json.Marshal(resp.Data)
	var wrapper struct {
		Snapshots []snapshots.Info `json:"snapshots"`
	}
	if err := json.Unmarshal(data, &wrapper); err != nil {
		return fmt.Errorf("解析快照列表失败: %w", err)
	}
	for _, info := range wrapper.Snapshots {
		if err := fn(info); err != nil {
			return err
		}
	}
	return nil
}

// Apply 是 Snapshotter 内部细节（解压 tar.gz blob 到 diff 目录），不支持跨进程调用
// 镜像拉取发生在 containerd 进程内部，不通过客户端走 RPC
func (cs *clientSnapshotter) Apply(ctx context.Context, digest, diffID, blobPath, key string) error {
	return fmt.Errorf("clientSnapshotter: Apply 是 Snapshotter 内部细节，不支持远程调用")
}

// DiffPath 通过 RPC 获取快照的 diff 目录路径
func (cs *clientSnapshotter) DiffPath(ctx context.Context, key string) (string, error) {
	resp, err := SendRequest(Request{
		Type: ReqDiffPath,
		Args: map[string]string{"key": key},
	})
	if err != nil {
		return "", err
	}
	if !resp.Success {
		return "", fmt.Errorf("%s", resp.Message)
	}

	data, _ := json.Marshal(resp.Data)
	var result map[string]interface{}
	if err := json.Unmarshal(data, &result); err != nil {
		return "", fmt.Errorf("解析 diff 路径失败: %w", err)
	}
	path, ok := result["path"].(string)
	if !ok {
		return "", fmt.Errorf("响应中缺少 path 字段")
	}
	return path, nil
}

// Close 客户端 Snapshotter 不需要关闭，no-op
func (cs *clientSnapshotter) Close() error {
	return nil
}
