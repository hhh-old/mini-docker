// Package apply 实现层差异应用器，对齐 containerd 的 diff/apply 子包
//
// 在真实 containerd 中，diff/apply/ 是 Applier 接口的独立实现子包，
// 与 diff/walking/（Comparer 实现）平级。
//
// Applier 的职责：将 OCI Layer (tar.gz) 应用到 Snapshot 的文件系统
//
//	Layer → 解压 → 处理 whiteout → 写入 snapshot
package apply

import (
	"context"
	"fmt"

	"mini-docker/containerd/diff"
	"mini-docker/containerd/snapshots"
)

// LayerApplier 层差异应用器实现
// 对齐 containerd: diff.Applier 的默认实现
type LayerApplier struct{}

// NewLayerApplier 创建层差异应用器
func NewLayerApplier() *LayerApplier {
	return &LayerApplier{}
}

// Apply 将层差异应用到 Active 快照
// 对齐 containerd: 从 Mount 对象提取 upperdir（解压目标）和 lowerdir（父层路径）
// 流程：
//  1. 从 Mount 对象提取 upperdir 和 lowerdir
//  2. 调用 extractLayerBlob 解压 tar.gz 到 upperdir
//  3. 校验 DiffID
//  4. 调用 processWhiteouts 处理 OCI whiteout 文件
func (a *LayerApplier) Apply(ctx context.Context, digest, diffID, blobPath string, mounts []snapshots.Mount) error {
	// 从 Mount 对象提取 upperdir（Active 快照的可写层目录）
	snapFSPath := diff.UpperDir(mounts)
	if snapFSPath == "" {
		return fmt.Errorf("无法从 mounts 中提取 upperdir: %+v", mounts)
	}

	// 从 Mount 对象提取 lowerdir（父层目录列表，用于 opaque whiteout 的 xattr 设置参考）
	parentFSDirs := diff.LowerDirs(mounts)

	// 解压 tar.gz 到 fs/ 目录，同时计算 DiffID
	actualDiffID, err := extractLayerBlob(blobPath, snapFSPath)
	if err != nil {
		return fmt.Errorf("解压层失败: %w", err)
	}

	// 校验 DiffID
	if diffID != "" && actualDiffID != diffID {
		return fmt.Errorf("DiffID 校验失败: 期望 %s, 实际 %s", diffID, actualDiffID)
	}

	// 处理 whiteout 文件
	if err := processWhiteouts(snapFSPath, parentFSDirs); err != nil {
		return fmt.Errorf("处理 whiteout 文件失败: %w", err)
	}

	return nil
}
