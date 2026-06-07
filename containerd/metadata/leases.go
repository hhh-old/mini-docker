package metadata

import (
	"encoding/json"
	"fmt"

	bolt "go.etcd.io/bbolt"
)

// LeaseInfo 租约信息（对齐 containerd 的 lease 机制）
// 用于保护正在使用的内容不被 GC 回收
type LeaseInfo struct {
	ID        string            `json:"id"`
	CreatedAt string            `json:"created_at"`
	Labels    map[string]string `json:"labels,omitempty"`
	Objects   []string          `json:"objects,omitempty"` // 关联的 digest 列表
}

// SaveLease 保存租约
func SaveLease(tx *bolt.Tx, info *LeaseInfo) error {
	b := tx.Bucket(BucketLeases)
	if b == nil {
		return fmt.Errorf("leases bucket 不存在")
	}
	data, err := json.Marshal(info)
	if err != nil {
		return fmt.Errorf("序列化租约失败: %w", err)
	}
	return b.Put([]byte(info.ID), data)
}

// LoadLease 加载租约
func LoadLease(tx *bolt.Tx, id string) (*LeaseInfo, error) {
	b := tx.Bucket(BucketLeases)
	if b == nil {
		return nil, fmt.Errorf("leases bucket 不存在")
	}
	data := b.Get([]byte(id))
	if data == nil {
		return nil, fmt.Errorf("租约 %s 不存在", id)
	}
	var info LeaseInfo
	if err := json.Unmarshal(data, &info); err != nil {
		return nil, fmt.Errorf("反序列化租约失败: %w", err)
	}
	return &info, nil
}

// DeleteLease 删除租约
func DeleteLease(tx *bolt.Tx, id string) error {
	b := tx.Bucket(BucketLeases)
	if b == nil {
		return fmt.Errorf("leases bucket 不存在")
	}
	return b.Delete([]byte(id))
}

// AddLeaseObject 向租约添加保护对象
func AddLeaseObject(tx *bolt.Tx, leaseID, digest string) error {
	info, err := LoadLease(tx, leaseID)
	if err != nil {
		return err
	}
	// 去重检查
	for _, obj := range info.Objects {
		if obj == digest {
			return nil
		}
	}
	info.Objects = append(info.Objects, digest)
	return SaveLease(tx, info)
}

// ListLeases 列出所有租约
func ListLeases(tx *bolt.Tx) ([]*LeaseInfo, error) {
	b := tx.Bucket(BucketLeases)
	if b == nil {
		return nil, fmt.Errorf("leases bucket 不存在")
	}
	var leases []*LeaseInfo
	c := b.Cursor()
	for k, v := c.First(); k != nil; k, v = c.Next() {
		var info LeaseInfo
		if err := json.Unmarshal(v, &info); err != nil {
			continue
		}
		leases = append(leases, &info)
	}
	return leases, nil
}

// WalkLeases 遍历租约
func WalkLeases(tx *bolt.Tx, fn func(*LeaseInfo) error) error {
	b := tx.Bucket(BucketLeases)
	if b == nil {
		return fmt.Errorf("leases bucket 不存在")
	}
	c := b.Cursor()
	for k, v := c.First(); k != nil; k, v = c.Next() {
		var info LeaseInfo
		if err := json.Unmarshal(v, &info); err != nil {
			continue
		}
		if err := fn(&info); err != nil {
			return err
		}
	}
	return nil
}
