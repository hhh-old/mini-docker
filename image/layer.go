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
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
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

// CreateLayer 创建新的镜像层
func CreateLayer(parentID string, diffPath string) (*LayerInfo, error) {
	// 计算层 ID（基于内容哈希）
	layerID, err := computeLayerID(diffPath)
	if err != nil {
		return nil, fmt.Errorf("计算层 ID 失败: %w", err)
	}

	layerDir := filepath.Join(overlay2StorePath, layerID)
	layerDiffDir := filepath.Join(layerDir, "diff")

	// 检查是否已存在（层共享：相同内容 = 相同层）
	if _, err := os.Stat(layerDir); err == nil {
		existing, err := loadLayerInfo(layerID)
		if err == nil {
			return existing, nil // 层已存在，直接复用
		}
	}

	// 创建层目录
	if err := os.MkdirAll(layerDiffDir, 0755); err != nil {
		return nil, fmt.Errorf("创建层目录失败: %w", err)
	}

	// 复制 diff 内容到层目录
	if diffPath != "" && diffPath != layerDiffDir {
		if err := copyDir(diffPath, layerDiffDir); err != nil {
			return nil, fmt.Errorf("复制层内容失败: %w", err)
		}
	}

	// 计算链 ID
	chainID := layerID
	if parentID != "" {
		parentLayer, err := loadLayerInfo(parentID)
		if err == nil && parentLayer.ChainID != "" {
			// ChainID = SHA256(parentChainID + " " + layerID)
			chainID = computeChainID(parentLayer.ChainID, layerID)
		}
	}

	// 计算层大小
	size := calculateDirSize(layerDiffDir)

	info := &LayerInfo{
		ID:        layerID,
		Parent:    parentID,
		DiffPath:  layerDiffDir,
		Size:      size,
		ChainID:   chainID,
		CreatedAt: time.Now().Format("2006-01-02 15:04:05"),
	}

	// 保存层元数据
	if err := saveLayerInfo(info); err != nil {
		return nil, fmt.Errorf("保存层元数据失败: %w", err)
	}

	// 写入 lower 文件（记录父层）
	if parentID != "" {
		lowerFile := filepath.Join(layerDir, "lower")
		os.WriteFile(lowerFile, []byte(parentID), 0644)
	}

	return info, nil
}

// GetLayerDiffPath 获取层的 diff 目录路径
func GetLayerDiffPath(layerID string) (string, error) {
	diffPath := filepath.Join(overlay2StorePath, layerID, "diff")
	if _, err := os.Stat(diffPath); os.IsNotExist(err) {
		return "", fmt.Errorf("层 %s 不存在", layerID)
	}
	return diffPath, nil
}

// BuildLowerDir 构建 OverlayFS 的 lowerdir 参数
// Docker 的方式：从底层到顶层，用 : 分隔
func BuildLowerDir(layerIDs []string) string {
	var dirs []string
	for _, id := range layerIDs {
		dirs = append(dirs, filepath.Join(overlay2StorePath, id, "diff"))
	}
	// OverlayFS 要求 lowerdir 从顶层到底层排列
	// 反转顺序：最新的层在最前面
	for i, j := 0, len(dirs)-1; i < j; i, j = i+1, j-1 {
		dirs[i], dirs[j] = dirs[j], dirs[i]
	}
	lowerDir := ""
	for i, dir := range dirs {
		if i > 0 {
			lowerDir += ":"
		}
		lowerDir += dir
	}
	return lowerDir
}

// SaveImageToDB 保存镜像到镜像数据库
func SaveImageToDB(entry *ImageDBEntry) error {
	if err := os.MkdirAll(imagedbPath, 0755); err != nil {
		return err
	}

	data, err := json.MarshalIndent(entry, "", "  ")
	if err != nil {
		return err
	}

	// 以镜像名+标签为文件名
	dbPath := filepath.Join(imagedbPath, entry.Name+":"+entry.Tag+".json")
	return os.WriteFile(dbPath, data, 0644)
}

// LoadImageFromDB 从镜像数据库加载镜像
func LoadImageFromDB(name, tag string) (*ImageDBEntry, error) {
	if tag == "" {
		tag = "latest"
	}

	dbPath := filepath.Join(imagedbPath, name+":"+tag+".json")
	data, err := os.ReadFile(dbPath)
	if err != nil {
		return nil, fmt.Errorf("镜像 %s:%s 不存在于数据库", name, tag)
	}

	var entry ImageDBEntry
	if err := json.Unmarshal(data, &entry); err != nil {
		return nil, fmt.Errorf("解析镜像元数据失败: %w", err)
	}

	return &entry, nil
}

// ListLayerDB 列出所有镜像层
func ListLayerDB() ([]*LayerInfo, error) {
	if err := os.MkdirAll(overlay2StorePath, 0755); err != nil {
		return nil, err
	}

	entries, err := os.ReadDir(overlay2StorePath)
	if err != nil {
		return nil, err
	}

	var layers []*LayerInfo
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		info, err := loadLayerInfo(entry.Name())
		if err != nil {
			continue
		}
		layers = append(layers, info)
	}

	return layers, nil
}

// ---- 内部辅助函数 ----

func computeLayerID(diffPath string) (string, error) {
	if diffPath == "" {
		return fmt.Sprintf("%x", sha256.Sum256([]byte(fmt.Sprintf("%d", time.Now().UnixNano())))), nil
	}

	h := sha256.New()
	err := filepath.Walk(diffPath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		// 将文件路径和大小加入哈希
		relPath, _ := filepath.Rel(diffPath, path)
		h.Write([]byte(relPath))
		h.Write([]byte(fmt.Sprintf("%d", info.Size())))
		return nil
	})
	if err != nil {
		return "", err
	}

	return fmt.Sprintf("%x", h.Sum(nil))[:64], nil
}

func computeChainID(parentChainID string, layerID string) string {
	h := sha256.New()
	h.Write([]byte(parentChainID + " " + layerID))
	return fmt.Sprintf("%x", h.Sum(nil))
}

func saveLayerInfo(info *LayerInfo) error {
	if err := os.MkdirAll(filepath.Join(overlay2StorePath, info.ID), 0755); err != nil {
		return err
	}

	data, err := json.MarshalIndent(info, "", "  ")
	if err != nil {
		return err
	}

	return os.WriteFile(filepath.Join(overlay2StorePath, info.ID, "metadata.json"), data, 0644)
}

func loadLayerInfo(layerID string) (*LayerInfo, error) {
	data, err := os.ReadFile(filepath.Join(overlay2StorePath, layerID, "metadata.json"))
	if err != nil {
		return nil, err
	}

	var info LayerInfo
	if err := json.Unmarshal(data, &info); err != nil {
		return nil, err
	}

	return &info, nil
}

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

		// 复制文件
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
