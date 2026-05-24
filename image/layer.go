package image

/*
=======================================================================
  镜像分层存储 —— 对齐 Docker 的 Overlay2 分层架构
=======================================================================

  Docker 镜像分层的核心思想：
  ┌─────────────────────────────────────────────────────────────┐
  │  镜像不是单个文件，而是多个只读层的叠加                        │
  │                                                              │
  │  ┌─────────────┐  ← Layer 3 (RUN pip install flask)        │
  │  ├─────────────┤  ← Layer 2 (COPY app.py /app/)            │
  │  ├─────────────┤  ← Layer 1 (FROM python:3.9)              │
  │  └─────────────┘                                            │
  │                                                              │
  │  层共享：多个镜像可以共享相同的底层，节省磁盘空间              │
  │  内容寻址：每层通过 SHA256 哈希标识，相同内容 = 相同层        │
  └─────────────────────────────────────────────────────────────┘

  Docker overlay2 存储结构：
  /var/lib/docker/overlay2/
  ├── <layer-id-1>/
  │   ├── diff/       ← 该层的文件差异
  │   ├── link        ← 指向该层的短链接名
  │   └── lower       ← 下层 ID 列表
  ├── <layer-id-2>/
  │   ├── diff/
  │   └── lower
  └── <merged-id>/     ← 运行时叠加
      ├── merged/
      ├── upper/
      └── work/

  mini-docker 的分层实现：
  /var/lib/mini-docker/overlay2/
  └── <layer-id>/
      ├── diff/          ← 该层的文件内容
      └── metadata.json  ← 层元数据（parent, size, chainID）

=======================================================================
*/

import (
	"io"
	"os"
	"path/filepath"
)

const (
	overlay2StorePath = "/var/lib/mini-docker/overlay2"
	imagedbPath       = "/var/lib/mini-docker/image/overlay2/imagedb"
	layerdbPath       = "/var/lib/mini-docker/image/overlay2/layerdb"
)

// LayerInfo 镜像层元数据
type LayerInfo struct {
	ID        string `json:"id"`         // 层 ID（SHA256 哈希）
	Parent    string `json:"parent"`     // 父层 ID（空表示基础层）
	DiffPath  string `json:"diff_path"`  // diff 目录路径
	Size      int64  `json:"size"`       // 层大小（字节）
	ChainID   string `json:"chain_id"`   // 链 ID（用于内容寻址）
	CreatedAt string `json:"created_at"` // 创建时间
}

// ImageDBEntry 镜像数据库条目
type ImageDBEntry struct {
	Name      string   `json:"name"`       // 镜像名
	Tag       string   `json:"tag"`        // 标签
	Layers    []string `json:"layers"`     // 层 ID 列表（从底到顶）
	CreatedAt string   `json:"created_at"` // 创建时间
	Size      string   `json:"size"`       // 总大小
	RootFS    string   `json:"rootfs"`     // rootfs 路径（向后兼容）
}

// ---- 内部辅助函数 ----

func calculateDirSize(dirPath string) int64 {
	var size int64
	filepath.Walk(dirPath, func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if !info.IsDir() {
			size += info.Size()
		}
		return nil
	})
	return size
}

func copyDir(src string, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}

		relPath, _ := filepath.Rel(src, path)
		dstPath := filepath.Join(dst, relPath)

		if info.IsDir() {
			return os.MkdirAll(dstPath, info.Mode())
		}

		srcFile, err := os.Open(path)
		if err != nil {
			return nil
		}
		defer srcFile.Close()

		dstFile, err := os.OpenFile(dstPath, os.O_CREATE|os.O_WRONLY, info.Mode())
		if err != nil {
			return nil
		}
		defer dstFile.Close()

		io.Copy(dstFile, srcFile)
		return nil
	})
}
