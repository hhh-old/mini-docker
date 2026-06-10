package metadata

import (
	"encoding/json"
	"fmt"

	bolt "go.etcd.io/bbolt"
)

// LeaseInfo 租约信息
// 用于保护正在拉取的内容不被 GC 回收：
// 创建时 Status 默认为 "in-progress"，GC 的 preflight 检测到后会整轮跳过；
// Pull 完成后立即 Delete lease，此时 Tags/Layers 引用链已建立，无需额外保护。
type LeaseInfo struct {
	ID        string            `json:"id"`
	CreatedAt string            `json:"created_at"`
	Labels    map[string]string `json:"labels,omitempty"`
	// Status 租约状态。
	//   - "in-progress": 拉取/构建中,GC 看到应整轮跳过(防误删半成品)
	// 进程崩溃残留的 in-progress 会被 GC 当作僵尸清理(见 Collector.preflight)。
	Status string `json:"status,omitempty"`
}

// LeaseStatus 租约状态常量
const (
	// LeaseStatusInProgress 拉取/构建中,GC 必须整轮跳过
	LeaseStatusInProgress = "in-progress"
)

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
