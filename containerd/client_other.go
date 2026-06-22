//go:build !linux

package containerd

import (
	"fmt"
	"net"
	"syscall"
	"time"

	"mini-docker/containerd/content"
	"mini-docker/containerd/diff"
	"mini-docker/containerd/events"
	"mini-docker/containerd/metadata"
	"mini-docker/containerd/snapshots"
	"mini-docker/containerstore"
	"mini-docker/types"
)

type Client struct{}

func NewClient() *Client {
	return &Client{}
}

func (c *Client) CreateTask(containerID, cgroupName string) (int, error) {
	return 0, fmt.Errorf("containerd 仅支持 Linux 平台")
}

func (c *Client) KillTask(containerID string, signal syscall.Signal) error {
	return fmt.Errorf("containerd 仅支持 Linux 平台")
}

func (c *Client) GetTaskState(containerID string) (*metadata.TaskState, error) {
	return nil, fmt.Errorf("containerd 仅支持 Linux 平台")
}

func (c *Client) GetExitInfo(containerID string) (*ExitInfo, error) {
	return nil, fmt.Errorf("containerd 仅支持 Linux 平台")
}

func (c *Client) DeleteTask(containerID string) error {
	return fmt.Errorf("containerd 仅支持 Linux 平台")
}

func (c *Client) ShutdownShim(containerID string) {}

func (c *Client) RestartShim(containerID string, containerPID int) (int, error) {
	return 0, fmt.Errorf("containerd 仅支持 Linux 平台")
}

func (c *Client) WaitForCreate(containerID string, timeout time.Duration) (int, error) {
	return 0, fmt.Errorf("containerd 仅支持 Linux 平台")
}

func (c *Client) StartTask(containerID string) error {
	return fmt.Errorf("containerd 仅支持 Linux 平台")
}

func (c *Client) PauseTask(containerID string) error {
	return fmt.Errorf("containerd 仅支持 Linux 平台")
}

func (c *Client) ResumeTask(containerID string) error {
	return fmt.Errorf("containerd 仅支持 Linux 平台")
}

func (c *Client) AttachTask(containerID string) (net.Conn, error) {
	return nil, fmt.Errorf("containerd 仅支持 Linux 平台")
}

func (c *Client) ExecTaskStream(containerID string, args []string, tty bool) (net.Conn, error) {
	return nil, fmt.Errorf("containerd 仅支持 Linux 平台")
}

func (c *Client) ResizeTask(containerID string, rows, cols uint16) error {
	return fmt.Errorf("containerd 仅支持 Linux 平台")
}

func (c *Client) ListTasks() ([]*metadata.TaskState, error) {
	return nil, fmt.Errorf("containerd 仅支持 Linux 平台")
}

func (c *Client) IsShimAlive(containerID string) bool {
	return false
}

func (c *Client) ReadShimPID(containerID string) int {
	return 0
}

func (c *Client) ReadExitInfo(containerID string) (*ExitInfo, error) {
	return nil, fmt.Errorf("containerd 仅支持 Linux 平台")
}

func (c *Client) PullImage(imageName string, progressFn func(ProgressFrameData)) (*metadata.Image, error) {
	return nil, fmt.Errorf("containerd 仅支持 Linux 平台")
}

func (c *Client) ListImages() ([]*metadata.Image, error) {
	return nil, fmt.Errorf("containerd 仅支持 Linux 平台")
}

func (c *Client) RemoveImage(imageRef string) error {
	return fmt.Errorf("containerd 仅支持 Linux 平台")
}

func (c *Client) InspectImage(imageRef string) (*metadata.Image, error) {
	return nil, fmt.Errorf("containerd 仅支持 Linux 平台")
}

func (c *Client) ResolveImage(imageRef string) (string, error) {
	return "", fmt.Errorf("containerd 仅支持 Linux 平台")
}

func (c *Client) RegisterImage(info *metadata.Image) error {
	return fmt.Errorf("containerd 仅支持 Linux 平台")
}

func (c *Client) CommitContainer(containerID, imageName, tag string) (*metadata.Image, error) {
	return nil, fmt.Errorf("containerd 仅支持 Linux 平台")
}

func (c *Client) PrepareSnapshot(containerID, topLayerSnapshotID string) (*types.OverlayDirs, error) {
	return nil, fmt.Errorf("containerd 仅支持 Linux 平台")
}

func (c *Client) RemoveSnapshot(key string) error {
	return fmt.Errorf("containerd 仅支持 Linux 平台")
}

func (c *Client) Snapshotter() snapshots.Snapshotter {
	return nil
}

func (c *Client) CreateContainer(info *containerstore.ContainerInfo) error {
	return fmt.Errorf("containerd 仅支持 Linux 平台")
}

func (c *Client) GetContainer(id string) (*containerstore.ContainerInfo, error) {
	return nil, fmt.Errorf("containerd 仅支持 Linux 平台")
}

func (c *Client) ListContainers() ([]*containerstore.ContainerInfo, error) {
	return nil, fmt.Errorf("containerd 仅支持 Linux 平台")
}

func (c *Client) UpdateContainer(info *containerstore.ContainerInfo) error {
	return fmt.Errorf("containerd 仅支持 Linux 平台")
}

func (c *Client) DeleteContainer(id string) error {
	return fmt.Errorf("containerd 仅支持 Linux 平台")
}

// ContentStore 返回 Content Store 代理（非 Linux 平台返回 nil）
func (c *Client) ContentStore() content.Store {
	return nil
}

// DiffService 返回 Diff Service 代理（非 Linux 平台返回 nil）
func (c *Client) DiffService() *diff.Service {
	return nil
}

// PublishEvent 向 containerd 事件总线发布事件（非 Linux 平台不支持）
func (c *Client) PublishEvent(ev *events.Envelope) error {
	return fmt.Errorf("containerd 仅支持 Linux 平台")
}

// SubscribeEvents 订阅 containerd 事件流（非 Linux 平台不支持）
func (c *Client) SubscribeEvents(filters ...string) (net.Conn, error) {
	return nil, fmt.Errorf("containerd 仅支持 Linux 平台")
}

// GetEventArchive 获取 containerd 事件归档（非 Linux 平台不支持）
func (c *Client) GetEventArchive(since, until time.Time) ([]*events.Envelope, error) {
	return nil, fmt.Errorf("containerd 仅支持 Linux 平台")
}
