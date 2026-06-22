//go:build !linux

package walking

import (
	"context"
	"fmt"

	"mini-docker/containerd/content"
	"mini-docker/containerd/diff"
	"mini-docker/containerd/snapshots"
)

// Diff 计算两个快照之间的文件系统差异（非 Linux 桩实现）
func (d *LayerDiffer) Diff(ctx context.Context, lower, upper []snapshots.Mount, contentStore content.Store, opts ...diff.DiffOpt) (diff.DiffResult, error) {
	return diff.DiffResult{}, fmt.Errorf("层差异计算仅在 Linux 上可用")
}
