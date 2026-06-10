package metadata

import (
	"encoding/json"
	"fmt"

	bolt "go.etcd.io/bbolt"
)

// Image 镜像索引记录（对齐 containerd images.Image 概念）
//
// 与 OCI Image Manifest 的区别：
//   - OCIManifest（containerd/images/registry.go）是 Registry 拉下来的不可变 blob，
//     存放在 contentStore，描述"该镜像由哪些 blob 组成"
//   - 本类型是本地 boltdb 中的索引记录，描述"name:tag → imageID → layers/snapshot"，
//     包含本地状态字段（CreatedAt、TopLayerSnapshotID、Config 等），Registry 不会生成这些
//
// 字段命名对齐 containerd images.Image 风格：TopLayerSnapshotID 明确语义为最顶层快照标识符，
// Annotations（复数）对齐 OCI/manifest 规范，避免与单数 Annotation 混淆。
//
// Size 字段是衍生数据(按 LayerDigests 实时计算得到),不进 boltdb:
//   - LoadImage / ListImages 读出后由 Service 层填充
//   - SaveImage 写入时由 JSON 的 `omitempty` 跳过空值
//   - 这样确保 "LayerDigests 变更 → Size 重新计算" 而不会出现"boltdb 里存了陈旧 Size"
type Image struct {
	ImageID   string `json:"image_id"`
	Name      string `json:"name"`
	Tag       string `json:"tag"`
	CreatedAt string `json:"created_at"`
	// TopLayerSnapshotID 最顶层快照在 Snapshotter 中的 key（cacheID），
	// 用于 PrepareSnapshot 的 parent 参数；本地构建镜像时等于镜像名
	TopLayerSnapshotID string `json:"top_layer_snapshot_id"`
	// LayerDigests 镜像各层的 compressed digest 列表（如 sha256:abc...）
	// 注意：与 OCIManifest.Layers (描述符) 不同，本字段只存 digest 字符串
	LayerDigests []string          `json:"layer_digests"`
	Config       ImageConfig       `json:"config"`
	Annotations  map[string]string `json:"annotations,omitempty"`
	// ConfigDigest 镜像配置 blob 的 digest（如 sha256:abc...）
	// 对齐 containerd: GC 需要标记 config blob 防止被误删
	// OCI 镜像的 imageID 即为 config digest（去掉 sha256: 前缀），但保留完整 digest
	// 格式便于与 content store 中的 key 对齐
	ConfigDigest string `json:"config_digest,omitempty"`
	// Size 衍生数据：仅在响应/展示时填充，SaveImage 写入时通过 omitempty 跳过
	Size string `json:"size,omitempty"`
}

// ImageConfig 镜像配置（对齐 Docker OCI Image Config）
// 字段补齐 OCI Config 规范中缺失的 Entrypoint 与 User
type ImageConfig struct {
	Cmd          []string          `json:"cmd,omitempty"`
	Env          []string          `json:"env,omitempty"`
	WorkingDir   string            `json:"working_dir,omitempty"`
	ExposedPorts []string          `json:"exposed_ports,omitempty"`
	Labels       map[string]string `json:"labels,omitempty"`
	Entrypoint   []string          `json:"entrypoint,omitempty"`
	User         string            `json:"user,omitempty"`
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
		var m Image
		if err := json.Unmarshal(v, &m); err != nil {
			continue
		}
		if m.ImageID == excludingImageID {
			continue
		}
		for _, l := range m.LayerDigests {
			if l == digest {
				return true
			}
		}
	}
	return false
}

// SaveImage 保存镜像元数据到 BucketImages
func SaveImage(tx *bolt.Tx, manifest *Image) error {
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
func LoadImage(tx *bolt.Tx, imageID string) (*Image, error) {
	b := tx.Bucket(BucketImages)
	if b == nil {
		return nil, fmt.Errorf("images bucket 不存在")
	}
	data := b.Get([]byte(imageID))
	if data == nil {
		return nil, fmt.Errorf("镜像 %s 不存在", imageID)
	}
	var m Image
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
func ListImages(tx *bolt.Tx) ([]*Image, error) {
	b := tx.Bucket(BucketImages)
	if b == nil {
		return nil, fmt.Errorf("images bucket 不存在")
	}
	var images []*Image
	c := b.Cursor()
	for k, v := c.First(); k != nil; k, v = c.Next() {
		var m Image
		if err := json.Unmarshal(v, &m); err != nil {
			continue
		}
		images = append(images, &m)
	}
	return images, nil
}
