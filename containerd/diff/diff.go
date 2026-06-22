// Package diff 定义层差异的接口与类型，对齐 containerd 的 diff 包结构
//
// 真实 containerd 的 diff 包组织：
//
//	containerd/diff/
//	├── diff.go              ← 接口定义：Applier + Comparer + Config + Opt
//	├── apply/               ← Applier 实现（独立子包）
//	└── walking/             ← Comparer/Differ 实现（独立子包）
//
// mini-docker 对齐此结构：
//   - diff.go 只定义接口和公共类型
//   - apply/ 子包实现 LayerApplier
//   - walking/ 子包实现 LayerDiffer + WalkingDiff
//
// 核心设计原则：接口与实现分离。调用方只依赖 diff.Applier / diff.Differ 接口，
// 具体实现通过插件系统注入。
package diff

import (
	"context"
	"path/filepath"
	"strings"

	"mini-docker/containerd/content"
	"mini-docker/containerd/snapshots"
)

// ---------------------------------------------------------------------------
// 核心接口（对齐 containerd: diff.Applier + diff.Comparer）
// ---------------------------------------------------------------------------

// Applier 层差异应用器接口
// 对齐 containerd: diff.Applier —— 负责将层差异应用到 Active 快照
//
// 在真实 containerd 中，Applier.Apply 的签名是：
//
//	Apply(ctx, desc ocispec.Descriptor, mounts []mount.Mount) (d ocispec.Descriptor, err error)
//
// 即接收 OCI Descriptor 和 Mount 对象，从 Mount 中提取目标路径。
// mini-docker 简化了 Descriptor 为 digest/diffID/blobPath，但保留了 Mount 抽象。
type Applier interface {
	// Apply 将层差异应用到 Active 快照
	// digest: 该层的压缩 digest (sha256:...)
	// diffID: 该层的未压缩 digest (sha256:...)，用于校验
	// blobPath: tar.gz 文件的路径
	// mounts: Active 快照的挂载信息（从中提取 upperdir 和 lowerdir）
	Apply(ctx context.Context, digest, diffID, blobPath string, mounts []snapshots.Mount) error
}

// Differ 层差异计算器接口
// 对齐 containerd: diff.Comparer —— 负责从两个快照的挂载信息计算文件系统差异
//
// 核心用途：
//   - docker commit: 从容器可写层(upper)与镜像层(lower)的差异生成新层
//   - docker push: 将每个 Committed 快照导出为 tar blob
//   - 镜像构建: 从构建步骤的差异生成新层
//
// 流程：
//
//	lower mounts (父快照) ─┐
//	                       ├─→ Differ.Diff() ─→ tar.gz blob ─→ Content Store
//	upper mounts (子快照) ─┘
//
// 对齐 containerd: Diff 接收 []mount.Mount 而非原始目录路径。
// Mount 对象是 Snapshotter 与 Differ 之间的抽象边界：
//   - Differ 不需要知道快照的内部目录结构
//   - 切换 Snapshotter 实现时 Differ 无需修改
//   - Mount 的 Options 中包含 lowerdir/upperdir 等信息，Differ 从中提取路径
type Differ interface {
	// Diff 计算两个快照之间的文件系统差异，生成 tar.gz blob 写入 Content Store
	// lower: 父快照的挂载信息（可为 nil，表示从空目录开始）
	// upper: 子快照的挂载信息（包含差异内容）
	// contentStore: Content Store 接口，用于写入生成的 blob
	// opts: 可选参数
	Diff(ctx context.Context, lower, upper []snapshots.Mount, contentStore content.Store, opts ...DiffOpt) (DiffResult, error)
}

// ---------------------------------------------------------------------------
// 差异计算类型与配置
// ---------------------------------------------------------------------------

// DiffType 差异计算类型
type DiffType int

const (
	DiffLayer DiffType = iota // 标准层差异（用于 docker commit / push）
)

// DiffConfig 差异计算配置
type DiffConfig struct {
	MediaType string            // 输出媒体类型，默认 "application/vnd.oci.image.layer.v1.tar+gzip"
	Ref       string            // Content Store 引用标识（用于 Lease 跟踪）
	Labels    map[string]string // 标签
}

// DiffOpt 差异计算选项
type DiffOpt func(*DiffConfig) error

// WithDiffMediaType 设置输出媒体类型
func WithDiffMediaType(mt string) DiffOpt {
	return func(c *DiffConfig) error {
		c.MediaType = mt
		return nil
	}
}

// WithDiffLabels 设置标签
func WithDiffLabels(labels map[string]string) DiffOpt {
	return func(c *DiffConfig) error {
		c.Labels = labels
		return nil
	}
}

// DiffResult 差异计算结果
// 对齐 containerd: diff.Differ 返回的是 content descriptor
type DiffResult struct {
	Digest    string            // 压缩 blob 的 digest (sha256:...)
	DiffID    string            // 未压缩 tar 的 digest (sha256:...)
	Size      int64             // 压缩 blob 的大小
	MediaType string            // 媒体类型
	Labels    map[string]string // 标签
}

// ---------------------------------------------------------------------------
// 公共辅助函数
// ---------------------------------------------------------------------------

// FSDir 返回给定快照 ID 的 fs/ 目录路径
// 替代旧版 OverlaySnapshotter.DiffPath 方法
// 对齐 containerd: snapshots/<id>/fs 为快照的文件系统目录
func FSDir(snapshotterRoot, snapshotID string) string {
	return filepath.Join(snapshotterRoot, "snapshots", snapshotID, "fs")
}

// DigestToCacheID 将 digest 转换为 cacheID（去掉 sha256: 前缀）
// 委托给 content 包的统一实现
var DigestToCacheID = content.DigestToCacheID

// UpperDir 从 Mount 对象中提取 upperdir 路径
// 对齐 containerd: overlay mount 的 Options 中包含 upperdir=<path>
// 对于 bind mount，Source 即为 upperdir
// 对于无 upperdir 的 mount（如 Committed/View），返回空字符串
func UpperDir(mounts []snapshots.Mount) string {
	for _, m := range mounts {
		switch m.Type {
		case "overlay":
			for _, opt := range m.Options {
				if strings.HasPrefix(opt, "upperdir=") {
					return strings.TrimPrefix(opt, "upperdir=")
				}
			}
		case "bind":
			return m.Source
		}
	}
	return ""
}

// LowerDirs 从 Mount 对象中提取 lowerdir 路径列表
// 对齐 containerd: overlay mount 的 Options 中包含 lowerdir=<path1>:<path2>:...
// 对于 bind mount，Source 即为 lowerdir
// 返回顺序：最近父层在前，最远祖先在后（OverlayFS lowerdir 的要求）
func LowerDirs(mounts []snapshots.Mount) []string {
	for _, m := range mounts {
		switch m.Type {
		case "overlay":
			for _, opt := range m.Options {
				if strings.HasPrefix(opt, "lowerdir=") {
					dirs := strings.Split(strings.TrimPrefix(opt, "lowerdir="), ":")
					return dirs
				}
			}
		case "bind":
			return []string{m.Source}
		}
	}
	return nil
}
