//go:build linux

package images

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"mini-docker/constants"
	"mini-docker/containerd/content"
)

// syscallStat 是 Linux 上 os.FileInfo.Sys() 返回的类型
// 用于 calculateRootFSSizeBytes 中硬链接去重
type syscallStat = syscall.Stat_t

/*
=======================================================================
  OCI 镜像层处理 —— 路径工具和 whiteout 处理
=======================================================================

  Docker 镜像层的 OCI 格式：
  - 每个层是一个 tar 归档文件（通常 gzip 压缩）
  - 层内包含相对根目录的文件路径和内容
  - 删除文件通过 whiteout 文件标记: .wh.<filename>
  - 删除目录通过 opaque whiteout 标记: .wh..wh..opq

  层存储结构（对齐 containerd）：
  /var/lib/mini-docker/snapshots/overlay/
  └── snapshots/
      └── <snapshot-id>/
          └── fs/        ← 该层解压后的文件（Committed 只读快照）

  Content Store（对齐 containerd: io.containerd.content.v1.content/blobs/sha256/）：
  /var/lib/mini-docker/content/sha256/
  └── <hex>          ← blob 原始数据（manifest/config/layer 压缩包）

  与之前版本的差异：
  - 废弃了 overlay2/ 目录（Docker 风格），改用 snapshots/overlay/（containerd 风格）
  - 废弃了 layerdb/ 目录，层元数据统一存储在 boltdb 中
  - 废弃了 blobs/ 目录，blob 统一存储在 content/sha256/ 中
  - 废弃了 BuildRootFS 预构建 rootfs，改为运行时 OverlayFS 动态合并
  - 容器运行时通过 Snapshotter 的 Mounts() 返回 overlay mount 选项，内核实时合并各层

  架构变更：
  - LayerStore.StoreLayer + Snapshotter.RegisterCommitted 的两步操作
    已合并为 Snapshotter.UnpackLayer 原子操作，消除了崩溃时元数据不一致的风险
  - extractLayerBlob 已迁移到 overlay 包，由 Snapshotter.UnpackLayer 内部调用
  - LayerStore 类型已移除，层解压和元数据注册统一由 Snapshotter 管理

=======================================================================
*/

// LayerDiffDir 返回指定层 digest 对应的 fs 目录路径
// 用于外部需要直接访问层文件内容的场景（如容器运行时构建 overlay lowerdir）
//
// Deprecated: 此函数直接拼接路径，无法解析快照 ID（仅通过 digest 推算 cacheID），
// 在新 Snapshotter 接口下目录结构已从 <root>/<key>/diff/ 变为 <root>/snapshots/<id>/fs/。
// 调用方应使用 snap.Stat(key) 获取快照 ID，再通过 diff.FSDir(root, id) 构造路径。
// 仅在无法获取 Snapshotter 实例时使用此函数（如纯计算场景）。
func LayerDiffDir(digest string) string {
	cacheID := content.DigestToCacheID(digest)
	// 新目录结构: <root>/snapshots/<id>/fs/（对齐 containerd）
	// 注意：cacheID 是快照的 name（key），在新结构中目录名使用数字 ID，
	// 此处仍用 cacheID 作为目录名仅为向后兼容，新代码请使用 diff.FSDir()
	return filepath.Join(constants.SnapshotterDir, "snapshots", cacheID, "fs")
}

// DigestToCacheID 已迁移到 content.DigestToCacheID（统一实现，避免多包重复定义）

// cleanOpaqueDir 处理 opaque whiteout（.wh..wh..opq）
// 对齐 containerd: 删除该目录下所有来自下层的文件和子目录
// opaque whiteout 表示"该目录下所有下层内容都应被隐藏"
func cleanOpaqueDir(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		// 跳过白标文件本身（由调用方处理）
		if strings.HasPrefix(entry.Name(), ".wh.") {
			continue
		}
		os.RemoveAll(filepath.Join(dir, entry.Name()))
	}
}
