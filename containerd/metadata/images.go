package metadata

import (
	"encoding/json"
	"fmt"

	bolt "go.etcd.io/bbolt"
)

// ImageManifest 镜像清单（从 image 包导入的概念，这里独立定义避免循环引用）
// 对齐 Docker OCI Image Manifest
type ImageManifest struct {
	ImageID    string            `json:"image_id"`
	Name       string            `json:"name"`
	Tag        string            `json:"tag"`
	CreatedAt  string            `json:"created_at"`
	RootFSPath string            `json:"rootfs_path"`
	Layers     []string          `json:"layers"`
	Config     ImageConfig       `json:"config"`
	Annotation map[string]string `json:"annotation,omitempty"`
}

// ImageConfig 镜像配置（对齐 Docker OCI Image Config）
type ImageConfig struct {
	Cmd          []string          `json:"cmd,omitempty"`
	Env          []string          `json:"env,omitempty"`
	WorkingDir   string            `json:"working_dir,omitempty"`
	ExposedPorts []string          `json:"exposed_ports,omitempty"`
	Labels       map[string]string `json:"labels,omitempty"`
}

// SaveImage 保存镜像元数据到 BucketImages
func SaveImage(tx *bolt.Tx, manifest *ImageManifest) error {
	b := tx.Bucket(BucketImages)
	if b == nil {
		return fmt.Errorf("images bucket 不存在")
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		return fmt.Errorf("序列化镜像清单失败: %w", err)
	}
	return b.Put([]byte(manifest.ImageID), data)
}

// LoadImage 从 BucketImages 加载镜像元数据
func LoadImage(tx *bolt.Tx, imageID string) (*ImageManifest, error) {
	b := tx.Bucket(BucketImages)
	if b == nil {
		return nil, fmt.Errorf("images bucket 不存在")
	}
	data := b.Get([]byte(imageID))
	if data == nil {
		return nil, fmt.Errorf("镜像 %s 不存在", imageID)
	}
	var m ImageManifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("反序列化镜像清单失败: %w", err)
	}
	return &m, nil
}

// DeleteImage 从 BucketImages 删除镜像元数据
func DeleteImage(tx *bolt.Tx, imageID string) error {
	b := tx.Bucket(BucketImages)
	if b == nil {
		return fmt.Errorf("images bucket 不存在")
	}
	return b.Delete([]byte(imageID))
}

// ListImages 列出所有镜像元数据
func ListImages(tx *bolt.Tx) ([]*ImageManifest, error) {
	b := tx.Bucket(BucketImages)
	if b == nil {
		return nil, fmt.Errorf("images bucket 不存在")
	}
	var images []*ImageManifest
	c := b.Cursor()
	for k, v := c.First(); k != nil; k, v = c.Next() {
		var m ImageManifest
		if err := json.Unmarshal(v, &m); err != nil {
			continue
		}
		images = append(images, &m)
	}
	return images, nil
}

// ImageExists 检查镜像是否存在
func ImageExists(tx *bolt.Tx, imageID string) bool {
	b := tx.Bucket(BucketImages)
	if b == nil {
		return false
	}
	return b.Get([]byte(imageID)) != nil
}
