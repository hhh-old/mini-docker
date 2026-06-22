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
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	crypto_sha256 "crypto/sha256"

	"mini-docker/constants"
	"mini-docker/containerd/content"
	"mini-docker/containerd/diff"
	"mini-docker/containerd/events"
	"mini-docker/containerd/gc"
	"mini-docker/containerd/metadata"
	"mini-docker/containerd/snapshots"
	"mini-docker/containerstore"
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

// CreateTask 创建容器任务（对齐 containerd: Task.Create 只传 containerID）
func (c *Client) CreateTask(containerID, cgroupName string) (shimPID int, err error) {
	resp, err := SendRequest(Request{
		Type: ReqCreateTask,
		Args: map[string]string{
			"container_id": containerID,
			"cgroup_name":  cgroupName,
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

// GetTaskState 获取容器任务状态
// 对齐 containerd: 返回 TaskState（containerd 层类型），不暴露 runtime 层类型
func (c *Client) GetTaskState(containerID string) (*metadata.TaskState, error) {
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
	var state metadata.TaskState
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

// ListTasks 列出所有容器任务
// 对齐 containerd: 返回 TaskState（containerd 层类型），不暴露 runtime 层类型
func (c *Client) ListTasks() ([]*metadata.TaskState, error) {
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
	var states []*metadata.TaskState
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

// CommitContainer 将容器的可写层提交为新镜像（对齐 docker commit）
// 对齐 containerd: 通过 RPC 调用 containerd 的 CommitContainer
func (c *Client) CommitContainer(containerID, imageName, tag string) (*metadata.Image, error) {
	resp, err := SendRequest(Request{
		Type: ReqCommitContainer,
		Args: map[string]string{
			"container_id": containerID,
			"image_name":   imageName,
			"tag":          tag,
		},
	})
	if err != nil {
		return nil, err
	}
	if !resp.Success {
		return nil, fmt.Errorf("%s", resp.Message)
	}

	data, _ := json.Marshal(resp.Data)
	var img metadata.Image
	if err := json.Unmarshal(data, &img); err != nil {
		return nil, fmt.Errorf("解析镜像信息失败: %w", err)
	}
	return &img, nil
}

// ---------------------------------------------------------------------------
// 容器元数据 RPC 方法（对齐 containerd: containers.Store gRPC 服务）
// 改造前：Daemon 直接调用 containerstore 包函数操作 boltdb，绕过 containerd
// 改造后：Daemon 通过 RPC 调用 containerd 的 Container Service
// ---------------------------------------------------------------------------

// CreateContainer 创建容器元数据记录（对齐 containerd: containers.Store.Create）
func (c *Client) CreateContainer(info *containerstore.ContainerInfo) error {
	infoJSON, _ := json.Marshal(info)
	resp, err := SendRequest(Request{
		Type: ReqCreateContainer,
		Args: map[string]string{"info": string(infoJSON)},
	})
	if err != nil {
		return err
	}
	if !resp.Success {
		return fmt.Errorf("%s", resp.Message)
	}
	return nil
}

// GetContainer 查询容器元数据（对齐 containerd: containers.Store.Get）
// 支持 ID 和名称两种查询方式
func (c *Client) GetContainer(id string) (*containerstore.ContainerInfo, error) {
	resp, err := SendRequest(Request{
		Type: ReqGetContainer,
		Args: map[string]string{"id": id},
	})
	if err != nil {
		return nil, err
	}
	if !resp.Success {
		return nil, fmt.Errorf("%s", resp.Message)
	}

	data, _ := json.Marshal(resp.Data)
	var info containerstore.ContainerInfo
	if err := json.Unmarshal(data, &info); err != nil {
		return nil, fmt.Errorf("解析容器信息失败: %w", err)
	}
	return &info, nil
}

// ListContainers 列出所有容器元数据（对齐 containerd: containers.Store.List）
func (c *Client) ListContainers() ([]*containerstore.ContainerInfo, error) {
	resp, err := SendRequest(Request{
		Type: ReqListContainers,
		Args: map[string]string{},
	})
	if err != nil {
		return nil, err
	}
	if !resp.Success {
		return nil, fmt.Errorf("%s", resp.Message)
	}

	data, _ := json.Marshal(resp.Data)
	var containers []*containerstore.ContainerInfo
	if err := json.Unmarshal(data, &containers); err != nil {
		return nil, fmt.Errorf("解析容器列表失败: %w", err)
	}
	return containers, nil
}

// UpdateContainer 更新容器元数据（对齐 containerd: containers.Store.Update）
func (c *Client) UpdateContainer(info *containerstore.ContainerInfo) error {
	infoJSON, _ := json.Marshal(info)
	resp, err := SendRequest(Request{
		Type: ReqUpdateContainer,
		Args: map[string]string{"info": string(infoJSON)},
	})
	if err != nil {
		return err
	}
	if !resp.Success {
		return fmt.Errorf("%s", resp.Message)
	}
	return nil
}

// DeleteContainer 删除容器元数据（对齐 containerd: containers.Store.Delete）
func (c *Client) DeleteContainer(id string) error {
	resp, err := SendRequest(Request{
		Type: ReqDeleteContainer,
		Args: map[string]string{"id": id},
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

// PublishEvent 向 containerd 事件总线发布事件。
func (c *Client) PublishEvent(ev *events.Envelope) error {
	eventJSON, err := json.Marshal(ev.Event)
	if err != nil {
		return fmt.Errorf("序列化事件失败: %w", err)
	}

	resp, err := SendRequest(Request{
		Type: ReqPublishEvent,
		Args: map[string]string{
			"topic":     ev.Topic,
			"namespace": ev.Namespace,
			"timestamp": ev.Timestamp.Format(time.RFC3339Nano),
			"event":     string(eventJSON),
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

// SubscribeEvents 订阅 containerd 事件流，返回长连接。
// filters 为 topic glob 过滤规则，例如 "/tasks/**"。
func (c *Client) SubscribeEvents(filters ...string) (net.Conn, error) {
	filtersJSON, _ := json.Marshal(filters)
	conn, resp, err := SendStreamRequest(Request{
		Type: ReqSubscribeEvents,
		Args: map[string]string{"filters": string(filtersJSON)},
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

// GetEventArchive 获取 containerd 事件归档。
func (c *Client) GetEventArchive(since, until time.Time) ([]*events.Envelope, error) {
	args := map[string]string{}
	if !since.IsZero() {
		args["since"] = since.Format(time.RFC3339Nano)
	}
	if !until.IsZero() {
		args["until"] = until.Format(time.RFC3339Nano)
	}

	resp, err := SendRequest(Request{
		Type: ReqGetEventArchive,
		Args: args,
	})
	if err != nil {
		return nil, err
	}
	if !resp.Success {
		return nil, fmt.Errorf("%s", resp.Message)
	}

	data, _ := json.Marshal(resp.Data)
	var archive []*events.Envelope
	if err := json.Unmarshal(data, &archive); err != nil {
		return nil, fmt.Errorf("解析事件归档失败: %w", err)
	}
	return archive, nil
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
//
// 新接口支持两种挂载类型：
//   - overlay mount（有父快照）：解析 lowerdir/upperdir/workdir，执行 overlay mount
//   - bind mount（无父快照）：source 为 fs/ 目录，直接作为 merged 目录使用
func (c *Client) PrepareSnapshot(containerID, topLayerSnapshotID string) (*types.OverlayDirs, error) {
	mounts, err := sendPrepareSnapshotRPC(containerID, topLayerSnapshotID)
	if err != nil {
		return nil, err
	}

	overlayDirs := &types.OverlayDirs{}
	for _, m := range mounts {
		switch m.Type {
		case "overlay":
			// overlay mount：解析 lowerdir/upperdir/workdir
			for _, opt := range m.Options {
				if strings.HasPrefix(opt, "upperdir=") {
					overlayDirs.Upper = strings.TrimPrefix(opt, "upperdir=")
				} else if strings.HasPrefix(opt, "workdir=") {
					overlayDirs.Work = strings.TrimPrefix(opt, "workdir=")
				} else if strings.HasPrefix(opt, "lowerdir=") {
					overlayDirs.Lower = strings.TrimPrefix(opt, "lowerdir=")
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

		case "bind":
			// bind mount（无父快照）：source 为 fs/ 目录，直接作为 merged 目录使用
			// 不需要执行 mount，fs/ 目录本身就是容器的 rootfs
			overlayDirs.Merged = m.Source
		}
	}

	return overlayDirs, nil
}

// Snapshotter 返回一个基于远程调用的 Snapshotter 代理
// 对齐 containerd: Daemon 通过此代理调用 containerd 服务端的 Snapshotter 方法
// 代理通过 Unix Socket 转发所有 Snapshotter 接口调用到服务端
func (c *Client) Snapshotter() snapshots.Snapshotter {
	return &clientSnapshotter{client: c}
}

// clientSnapshotter 基于 Unix Socket 的远程 Snapshotter 代理
// 对齐 containerd: Daemon 不直接持有 Snapshotter，而是通过 Unix Socket RPC 代理调用
//
// 本代理实现 snapshots.Snapshotter 接口：
//   - Prepare: 创建可写快照，返回 mount 信息
//   - View: 创建只读活跃快照，返回 mount 信息
//   - Commit: 提交 Active 快照为 Committed
//   - Mounts: 获取快照的挂载信息
//   - Remove: 删除快照
//   - Stat: 查询快照元信息
//   - Update: 更新快照元信息
//   - Usage: 查询快照资源使用量
//   - Walk: 遍历所有快照
//   - Cleanup: 清理已移除/废弃快照的磁盘资源
//   - Close: 关闭（no-op）
//
// 所有方法通过 RPC 转发到 containerd 服务端执行
type clientSnapshotter struct {
	client *Client
}

// Prepare 通过 RPC 创建容器可写层快照
// 对齐 containerd: Snapshotter.Prepare 创建 Active 可写快照，返回 mount 信息
// opts 在远程调用中被忽略（标签等选项仅服务端本地调用时生效）
func (cs *clientSnapshotter) Prepare(ctx context.Context, key, parent string, opts ...snapshots.Opt) ([]snapshots.Mount, error) {
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

// Mounts 通过 RPC 获取快照的挂载信息
// 对齐 containerd: Snapshotter.Mounts 返回快照的 mount 列表
func (cs *clientSnapshotter) Mounts(ctx context.Context, key string) ([]snapshots.Mount, error) {
	resp, err := SendRequest(Request{
		Type: ReqMountsSnapshot,
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

// Commit 通过 RPC 提交快照（对齐 containerd: Snapshotter.Commit）
// builder 构建流程使用：RUN/COPY 指令执行完毕后，将 Active 快照提交为 Committed
// name: 提交后的快照名称，key: 源 Active 快照名称（提交后该 key 被消费）
// opts 在远程调用中被忽略
func (cs *clientSnapshotter) Commit(ctx context.Context, name, key string, opts ...snapshots.Opt) error {
	resp, err := SendRequest(Request{
		Type: ReqCommitSnapshot,
		Args: map[string]string{"name": name, "key": key},
	})
	if err != nil {
		return err
	}
	if !resp.Success {
		return fmt.Errorf("%s", resp.Message)
	}
	return nil
}

// Remove 通过 RPC 删除容器快照
// 对齐 containerd: Snapshotter.Remove 删除快照及其元数据
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
// 使用新的 WalkFunc 签名：(ctx context.Context, info Info) error
func (cs *clientSnapshotter) Walk(ctx context.Context, fn snapshots.WalkFunc, filters ...string) error {
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
		if err := fn(ctx, info); err != nil {
			return err
		}
	}
	return nil
}

// View 通过 RPC 创建只读活跃快照（对齐 containerd: Snapshotter.View）
// 用于挂载查看镜像内容等只读场景，无 upperdir/workdir
// opts 在远程调用中被忽略
func (cs *clientSnapshotter) View(ctx context.Context, key, parent string, opts ...snapshots.Opt) ([]snapshots.Mount, error) {
	resp, err := SendRequest(Request{
		Type: ReqViewSnapshot,
		Args: map[string]string{
			"key":    key,
			"parent": parent,
		},
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

// Stat 通过 RPC 查询快照元信息（对齐 containerd: Snapshotter.Stat）
func (cs *clientSnapshotter) Stat(ctx context.Context, key string) (snapshots.Info, error) {
	resp, err := SendRequest(Request{
		Type: ReqStatSnapshot,
		Args: map[string]string{"key": key},
	})
	if err != nil {
		return snapshots.Info{}, err
	}
	if !resp.Success {
		return snapshots.Info{}, fmt.Errorf("%s", resp.Message)
	}

	data, _ := json.Marshal(resp.Data)
	var wrapper struct {
		Info snapshots.Info `json:"info"`
	}
	if err := json.Unmarshal(data, &wrapper); err != nil {
		return snapshots.Info{}, fmt.Errorf("解析快照信息失败: %w", err)
	}
	return wrapper.Info, nil
}

// Update 通过 RPC 更新快照元信息（对齐 containerd: Snapshotter.Update）
// fieldpaths: 指定要更新的字段路径，为空则更新所有可变字段
func (cs *clientSnapshotter) Update(ctx context.Context, info snapshots.Info, fieldpaths ...string) (snapshots.Info, error) {
	args := map[string]string{
		"name": info.Name,
	}
	// 序列化 info 的 labels（可变字段）
	if info.Labels != nil {
		labelsJSON, _ := json.Marshal(info.Labels)
		args["labels"] = string(labelsJSON)
	}
	// 序列化 fieldpaths
	if len(fieldpaths) > 0 {
		args["fieldpaths"] = strings.Join(fieldpaths, ",")
	}

	resp, err := SendRequest(Request{
		Type: ReqUpdateSnapshot,
		Args: args,
	})
	if err != nil {
		return snapshots.Info{}, err
	}
	if !resp.Success {
		return snapshots.Info{}, fmt.Errorf("%s", resp.Message)
	}

	data, _ := json.Marshal(resp.Data)
	var wrapper struct {
		Info snapshots.Info `json:"info"`
	}
	if err := json.Unmarshal(data, &wrapper); err != nil {
		return snapshots.Info{}, fmt.Errorf("解析更新后的快照信息失败: %w", err)
	}
	return wrapper.Info, nil
}

// Usage 通过 RPC 查询快照资源使用量（对齐 containerd: Snapshotter.Usage）
func (cs *clientSnapshotter) Usage(ctx context.Context, key string) (snapshots.Usage, error) {
	resp, err := SendRequest(Request{
		Type: ReqUsageSnapshot,
		Args: map[string]string{"key": key},
	})
	if err != nil {
		return snapshots.Usage{}, err
	}
	if !resp.Success {
		return snapshots.Usage{}, fmt.Errorf("%s", resp.Message)
	}

	data, _ := json.Marshal(resp.Data)
	var wrapper struct {
		Usage snapshots.Usage `json:"usage"`
	}
	if err := json.Unmarshal(data, &wrapper); err != nil {
		return snapshots.Usage{}, fmt.Errorf("解析资源使用量失败: %w", err)
	}
	return wrapper.Usage, nil
}

// Cleanup 通过 RPC 清理已移除/废弃快照的磁盘资源（对齐 containerd: Snapshotter.Cleanup）
func (cs *clientSnapshotter) Cleanup(ctx context.Context) error {
	resp, err := SendRequest(Request{
		Type: ReqCleanupSnapshot,
		Args: map[string]string{},
	})
	if err != nil {
		return err
	}
	if !resp.Success {
		return fmt.Errorf("%s", resp.Message)
	}
	return nil
}

// Close 客户端 Snapshotter 不需要关闭，no-op
func (cs *clientSnapshotter) Close() error {
	return nil
}

// ---------------------------------------------------------------------------
// Content Store RPC 代理（对齐 containerd: Daemon 不直接持有 Content Store，
// 而是通过 RPC 代理调用 containerd 服务端）
// ---------------------------------------------------------------------------

// ContentStore 返回一个基于远程调用的 Content Store 代理
// 对齐 containerd: Daemon 通过此代理调用 containerd 服务端的 Content Store 方法
func (c *Client) ContentStore() content.Store {
	return &clientContentStore{client: c}
}

// clientContentStore 基于 Unix Socket 的远程 Content Store 代理
type clientContentStore struct {
	client *Client
}

// Info 通过 RPC 查询 blob 元信息
func (cs *clientContentStore) Info(ctx context.Context, digest string) (content.Info, error) {
	resp, err := SendRequest(Request{
		Type: ReqContentInfo,
		Args: map[string]string{"digest": digest},
	})
	if err != nil {
		return content.Info{}, err
	}
	if !resp.Success {
		return content.Info{}, fmt.Errorf("%s", resp.Message)
	}

	data, _ := json.Marshal(resp.Data)
	var info content.Info
	if err := json.Unmarshal(data, &info); err != nil {
		return content.Info{}, fmt.Errorf("解析 blob 信息失败: %w", err)
	}
	return info, nil
}

// Path 通过 RPC 获取 blob 本地存储路径
func (cs *clientContentStore) Path(ctx context.Context, digest string) (string, error) {
	resp, err := SendRequest(Request{
		Type: ReqContentPath,
		Args: map[string]string{"digest": digest},
	})
	if err != nil {
		return "", err
	}
	if !resp.Success {
		return "", fmt.Errorf("%s", resp.Message)
	}

	data, _ := json.Marshal(resp.Data)
	var result struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return "", fmt.Errorf("解析路径失败: %w", err)
	}
	return result.Path, nil
}

// Exists 通过 RPC 检查 blob 是否存在
func (cs *clientContentStore) Exists(ctx context.Context, digest string) bool {
	resp, err := SendRequest(Request{
		Type: ReqContentExists,
		Args: map[string]string{"digest": digest},
	})
	if err != nil || !resp.Success {
		return false
	}
	data, _ := json.Marshal(resp.Data)
	var result struct {
		Exists bool `json:"exists"`
	}
	if json.Unmarshal(data, &result) != nil {
		return false
	}
	return result.Exists
}

// Delete 通过 RPC 删除 blob
func (cs *clientContentStore) Delete(ctx context.Context, digest string) error {
	resp, err := SendRequest(Request{
		Type: ReqContentDelete,
		Args: map[string]string{"digest": digest},
	})
	if err != nil {
		return err
	}
	if !resp.Success {
		return fmt.Errorf("%s", resp.Message)
	}
	return nil
}

// Writer 通过 RPC 创建 Content Writer
// 采用分块写入模式：先创建 Writer，再分块写入，最后 Commit
func (cs *clientContentStore) Writer(ctx context.Context, expected string, size int64, mediaType string) (content.Writer, error) {
	ref := fmt.Sprintf("ref-%d", time.Now().UnixNano())

	resp, err := SendRequest(Request{
		Type: ReqContentWrite,
		Args: map[string]string{
			"ref":        ref,
			"action":     "create",
			"expected":   expected,
			"size":       strconv.FormatInt(size, 10),
			"media_type": mediaType,
		},
	})
	if err != nil {
		return nil, err
	}
	if !resp.Success {
		return nil, fmt.Errorf("%s", resp.Message)
	}

	return &rpcContentWriter{
		client:   cs.client,
		ref:      ref,
		digester: crypto_sha256.New(),
	}, nil
}

// Reader 返回 blob 的读取流
// 由于 RPC 不支持流式读取，这里直接打开本地文件（Daemon 和 containerd 共享文件系统）
func (cs *clientContentStore) Reader(ctx context.Context, digest string) (io.ReadCloser, error) {
	path, err := cs.Path(ctx, digest)
	if err != nil {
		return nil, err
	}
	return os.Open(path)
}

// Walk 通过 RPC 遍历所有 blob 元信息
func (cs *clientContentStore) Walk(ctx context.Context, fn func(content.Info) error) error {
	resp, err := SendRequest(Request{
		Type: ReqContentWalk,
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
		Infos []content.Info `json:"infos"`
	}
	if err := json.Unmarshal(data, &wrapper); err != nil {
		return fmt.Errorf("解析 blob 列表失败: %w", err)
	}
	for _, info := range wrapper.Infos {
		if err := fn(info); err != nil {
			return err
		}
	}
	return nil
}

// Update 通过 RPC 更新 blob 标签
func (cs *clientContentStore) Update(ctx context.Context, digest string, labels map[string]string) error {
	labelsJSON, _ := json.Marshal(labels)
	resp, err := SendRequest(Request{
		Type: ReqContentUpdate,
		Args: map[string]string{
			"digest": digest,
			"labels": string(labelsJSON),
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

// rpcContentWriter 基于 RPC 的 Content Writer 代理
type rpcContentWriter struct {
	client   *Client
	ref      string
	digester hash.Hash
	written  int64
}

// Write 通过 RPC 分块写入数据
func (w *rpcContentWriter) Write(p []byte) (int, error) {
	// 将二进制数据编码为 base64 传输，避免 JSON 序列化问题
	encoded := base64.StdEncoding.EncodeToString(p)

	resp, err := SendRequest(Request{
		Type: ReqContentWrite,
		Args: map[string]string{
			"ref":    w.ref,
			"action": "write",
			"data":   encoded,
		},
	})
	if err != nil {
		return 0, err
	}
	if !resp.Success {
		return 0, fmt.Errorf("%s", resp.Message)
	}

	w.digester.Write(p)
	w.written += int64(len(p))
	return len(p), nil
}

// Commit 通过 RPC 提交 blob 并校验 digest
func (w *rpcContentWriter) Commit(ctx context.Context, expectedDigest string) error {
	calculated := "sha256:" + hex.EncodeToString(w.digester.Sum(nil))
	if expectedDigest == "" {
		expectedDigest = calculated
	}

	resp, err := SendRequest(Request{
		Type: ReqContentWrite,
		Args: map[string]string{
			"ref":             w.ref,
			"action":          "commit",
			"expected_digest": expectedDigest,
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

// Status 返回已写入的字节数
func (w *rpcContentWriter) Status() (int64, error) {
	return w.written, nil
}

// Close 通过 RPC 关闭 Writer（丢弃未提交的数据）
func (w *rpcContentWriter) Close() error {
	resp, err := SendRequest(Request{
		Type: ReqContentWrite,
		Args: map[string]string{
			"ref":    w.ref,
			"action": "close",
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

// Digest 返回当前计算的 digest
func (w *rpcContentWriter) Digest() string {
	return "sha256:" + hex.EncodeToString(w.digester.Sum(nil))
}

// DiffService 返回一个基于远程调用的 Diff Service 代理
// 对齐 containerd: Daemon 通过此代理调用 containerd 服务端的 Diff Service 方法
func (c *Client) DiffService() *clientDiffService {
	return &clientDiffService{client: c}
}

// clientDiffService 基于 Unix Socket 的远程 Diff Service 代理
type clientDiffService struct {
	client *Client
}

// Apply 通过 RPC 将层差异应用到 Active 快照
func (cds *clientDiffService) Apply(ctx context.Context, digest, diffID, key string) error {
	resp, err := SendRequest(Request{
		Type: ReqDiffApply,
		Args: map[string]string{
			"digest":  digest,
			"diff_id": diffID,
			"key":     key,
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

// Diff 通过 RPC 计算两个快照之间的差异，生成 blob 写入 Content Store
// opts 会在客户端应用为 DiffConfig 后序列化到 RPC 请求中
func (cds *clientDiffService) Diff(ctx context.Context, lowerKey, upperKey string, opts ...diff.DiffOpt) (diff.DiffResult, error) {
	// 应用选项到默认配置，提取可序列化字段
	cfg := diff.DiffConfig{}
	for _, opt := range opts {
		if err := opt(&cfg); err != nil {
			return diff.DiffResult{}, fmt.Errorf("应用 diff 选项失败: %w", err)
		}
	}
	cfgJSON, err := json.Marshal(cfg)
	if err != nil {
		return diff.DiffResult{}, fmt.Errorf("序列化 diff 选项失败: %w", err)
	}

	resp, err := SendRequest(Request{
		Type: ReqDiffDiff,
		Args: map[string]string{
			"lower_key": lowerKey,
			"upper_key": upperKey,
			"config":    string(cfgJSON),
		},
	})
	if err != nil {
		return diff.DiffResult{}, err
	}
	if !resp.Success {
		return diff.DiffResult{}, fmt.Errorf("%s", resp.Message)
	}

	data, _ := json.Marshal(resp.Data)
	var result diff.DiffResult
	if err := json.Unmarshal(data, &result); err != nil {
		return diff.DiffResult{}, fmt.Errorf("解析 diff 结果失败: %w", err)
	}
	return result, nil
}
