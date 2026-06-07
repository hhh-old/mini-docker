package metadata

import (
	"encoding/json"
	"fmt"

	bolt "go.etcd.io/bbolt"
)

// LayerInfo 层元数据（对齐 Docker layerdb 中的层记录）
type LayerInfo struct {
	Digest   string `json:"digest"`
	CacheID  string `json:"cache_id"`
	Size     int64  `json:"size"`
	DiffID   string `json:"diff_id,omitempty"`
	ParentID string `json:"parent_id,omitempty"`
}

// SaveLayer 保存层元数据
func SaveLayer(tx *bolt.Tx, info *LayerInfo) error {
	b := tx.Bucket(BucketLayers)
	if b == nil {
		return fmt.Errorf("layers bucket 不存在")
	}
	data, err := json.Marshal(info)
	if err != nil {
		return fmt.Errorf("序列化层元数据失败: %w", err)
	}
	return b.Put([]byte(info.Digest), data)
}

// LoadLayer 加载层元数据
func LoadLayer(tx *bolt.Tx, digest string) (*LayerInfo, error) {
	b := tx.Bucket(BucketLayers)
	if b == nil {
		return nil, fmt.Errorf("layers bucket 不存在")
	}
	data := b.Get([]byte(digest))
	if data == nil {
		return nil, fmt.Errorf("层 %s 不存在", digest)
	}
	var info LayerInfo
	if err := json.Unmarshal(data, &info); err != nil {
		return nil, fmt.Errorf("反序列化层元数据失败: %w", err)
	}
	return &info, nil
}

// DeleteLayer 删除层元数据
func DeleteLayer(tx *bolt.Tx, digest string) error {
	b := tx.Bucket(BucketLayers)
	if b == nil {
		return fmt.Errorf("layers bucket 不存在")
	}
	return b.Delete([]byte(digest))
}

// HasOtherRefs 检查层是否还被其他镜像引用（除 excludingImageID 外）
// 用于 GC: 如果某层只被当前要删除的镜像引用，则可以安全清理
func HasOtherRefs(tx *bolt.Tx, digest string, excludingImageID string) bool {
	b := tx.Bucket(BucketImages)
	if b == nil {
		return false
	}
	c := b.Cursor()
	for k, v := c.First(); k != nil; k, v = c.Next() {
		var m ImageManifest
		if err := json.Unmarshal(v, &m); err != nil {
			continue
		}
		if m.ImageID == excludingImageID {
			continue
		}
		for _, l := range m.Layers {
			if l == digest {
				return true
			}
		}
	}
	return false
}
