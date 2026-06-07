package metadata

import (
	"fmt"

	bolt "go.etcd.io/bbolt"
)

// SaveTag 保存标签映射 (name:tag → image_id)
func SaveTag(tx *bolt.Tx, name, tag, imageID string) error {
	b := tx.Bucket(BucketTags)
	if b == nil {
		return fmt.Errorf("tags bucket 不存在")
	}
	key := name + ":" + tag
	return b.Put([]byte(key), []byte(imageID))
}

// ResolveImageID 通过 name:tag 查找镜像 ID
func ResolveImageID(tx *bolt.Tx, name, tag string) (string, error) {
	b := tx.Bucket(BucketTags)
	if b == nil {
		return "", fmt.Errorf("tags bucket 不存在")
	}
	key := name + ":" + tag
	v := b.Get([]byte(key))
	if v == nil {
		return "", fmt.Errorf("标签 %s:%s 不存在", name, tag)
	}
	return string(v), nil
}

// RemoveTag 删除标签映射
func RemoveTag(tx *bolt.Tx, name, tag string) error {
	b := tx.Bucket(BucketTags)
	if b == nil {
		return fmt.Errorf("tags bucket 不存在")
	}
	key := name + ":" + tag
	return b.Delete([]byte(key))
}

// ListTags 列出所有标签
// 返回: map[name]map[tag]imageID
func ListTags(tx *bolt.Tx) (map[string]map[string]string, error) {
	b := tx.Bucket(BucketTags)
	if b == nil {
		return nil, fmt.Errorf("tags bucket 不存在")
	}
	result := make(map[string]map[string]string)
	c := b.Cursor()
	for k, v := c.First(); k != nil; k, v = c.Next() {
		key := string(k)
		imageID := string(v)

		// 解析 "name:tag" 格式
		var name, tag string
		for i := len(key) - 1; i >= 0; i-- {
			if key[i] == ':' {
				name = key[:i]
				tag = key[i+1:]
				break
			}
		}
		if name == "" {
			continue
		}

		if result[name] == nil {
			result[name] = make(map[string]string)
		}
		result[name][tag] = imageID
	}
	return result, nil
}

// RemoveTagsByImageID 删除指向指定 imageID 的所有标签
func RemoveTagsByImageID(tx *bolt.Tx, imageID string) error {
	b := tx.Bucket(BucketTags)
	if b == nil {
		return fmt.Errorf("tags bucket 不存在")
	}
	target := []byte(imageID)
	var keysToDelete [][]byte

	c := b.Cursor()
	for k, v := c.First(); k != nil; k, v = c.Next() {
		if string(v) == string(target) {
			keysToDelete = append(keysToDelete, k)
		}
	}

	for _, key := range keysToDelete {
		if err := b.Delete(key); err != nil {
			return err
		}
	}
	return nil
}
