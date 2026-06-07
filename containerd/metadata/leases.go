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
	Objects   []LeaseObject     `json:"objects,omitempty"` // 关联的保护对象列表
}

// LeaseObjectType 保护对象类型（对齐 containerd: content 和 snapshot 两种资源类型）
type LeaseObjectType string

const (
	// LeaseObjectContent 表示 content blob（digest 格式: sha256:abc...）
	LeaseObjectContent LeaseObjectType = "content"
	// LeaseObjectSnapshot 表示快照（key 格式: abc...，digest 的 hex 部分）
	LeaseObjectSnapshot LeaseObjectType = "snapshot"
)

// LeaseObject 保护对象（对齐 containerd: 每个对象有类型和标识）
type LeaseObject struct {
	Type LeaseObjectType `json:"type"`
	ID   string          `json:"id"`
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
// 对齐 containerd: 每个对象有类型（content 或 snapshot），GC 根据类型分别标记
func AddLeaseObject(tx *bolt.Tx, leaseID string, objType LeaseObjectType, objID string) error {
	info, err := LoadLease(tx, leaseID)
	if err != nil {
		return err
	}
	// 去重检查
	for _, obj := range info.Objects {
		if obj.Type == objType && obj.ID == objID {
			return nil
		}
	}
	info.Objects = append(info.Objects, LeaseObject{Type: objType, ID: objID})
	return SaveLease(tx, info)
}

// ListLeases 列出所有租约（通过 WalkLeases 实现，避免重复遍历逻辑）
func ListLeases(tx *bolt.Tx) ([]*LeaseInfo, error) {
	var leases []*LeaseInfo
	err := WalkLeases(tx, func(info *LeaseInfo) error {
		leases = append(leases, info)
		return nil
	})
	return leases, err
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
