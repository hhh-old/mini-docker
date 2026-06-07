package native

import (
	"fmt"

	"mini-docker/containerd/metadata"
)

// NativeSnapshotter 直接目录复制的 Snapshotter (未来扩展)
// 对齐 containerd 的 native snapshotter
// 当前为 stub 实现
type NativeSnapshotter struct{}

// NewSnapshotter 创建 Native Snapshotter (尚未实现)
func NewSnapshotter(root string, db *metadata.DB) (*NativeSnapshotter, error) {
	return nil, fmt.Errorf("native snapshotter 尚未实现")
}
