// Package walking 实现层差异计算器，对齐 containerd 的 diff/walking 子包
//
// 在真实 containerd 中，diff/walking/ 是 Comparer 接口的默认实现子包，
// 与 diff/apply/（Applier 实现）平级。
//
// Differ 的职责：从两个快照的文件系统计算差异，生成 OCI Layer
//
//	Filesystem → 扫描差异 → 生成 tar+gzip → Content Store
//
// WalkingDiff 组合了 LayerApplier 和 LayerDiffer，
// 对齐 containerd: walking differ 是默认的 diff 插件实现。
package walking

import (
	"mini-docker/containerd/diff"
	"mini-docker/containerd/diff/apply"
)

// WalkingDiff 组合了 LayerApplier 和 LayerDiffer
// 对齐 containerd: walking differ 是默认的 diff 插件实现
// 同时包含 Applier（将层差异应用到 Active 快照）和 Differ（计算两个快照的差异）
type WalkingDiff struct {
	Applier *apply.LayerApplier // 层差异应用器
	Differ  *LayerDiffer        // 层差异计算器
}

// NewWalkingDiff 创建组合了 Applier + Differ 的 walking diff 实例
// 对齐 containerd: 插件注册时使用此构造函数
func NewWalkingDiff() *WalkingDiff {
	return &WalkingDiff{
		Applier: apply.NewLayerApplier(),
		Differ:  NewLayerDiffer(),
	}
}

// 编译期接口检查
var _ diff.Applier = (*apply.LayerApplier)(nil)
var _ diff.Differ = (*LayerDiffer)(nil)
