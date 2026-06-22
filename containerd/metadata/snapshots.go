package metadata

import (
	"encoding/json"
	"fmt"
	"strconv"

	bolt "go.etcd.io/bbolt"

	"mini-docker/containerd/snapshots"
)

// nextSnapshotIDKey 是 boltDB 中存储下一个快照 ID 的键
// 放在 BucketSnapshots 桶内，使用特殊键名避免与快照 Name 冲突
var nextSnapshotIDKey = []byte("__next_snapshot_id")

// NextSnapshotID 原子递增并返回下一个快照内部数字 ID
// 对齐 containerd 的存储模型：每个快照在磁盘上用数字 ID 命名目录
// 首次调用返回 "1"，之后依次递增
func NextSnapshotID(tx *bolt.Tx) (string, error) {
	b := tx.Bucket(BucketSnapshots)
	if b == nil {
		return "", fmt.Errorf("snapshots bucket 不存在")
	}

	// 读取当前值，默认从 0 开始（第一次会递增为 1）
	var next uint64 = 0
	if data := b.Get(nextSnapshotIDKey); data != nil {
		if v, err := strconv.ParseUint(string(data), 10, 64); err == nil {
			next = v
		}
	}

	// 递增
	next++

	// 写回
	if err := b.Put(nextSnapshotIDKey, []byte(strconv.FormatUint(next, 10))); err != nil {
		return "", fmt.Errorf("写入 next_snapshot_id 失败: %w", err)
	}

	return strconv.FormatUint(next, 10), nil
}

// SaveSnapshot 保存快照元数据
// 以快照 Name 作为 boltDB 的键，Info（含 ID 字段）序列化为 JSON 值
func SaveSnapshot(tx *bolt.Tx, info *snapshots.Info) error {
	b := tx.Bucket(BucketSnapshots)
	if b == nil {
		return fmt.Errorf("snapshots bucket 不存在")
	}
	data, err := json.Marshal(info)
	if err != nil {
		return fmt.Errorf("序列化快照元数据失败: %w", err)
	}
	return b.Put([]byte(info.Name), data)
}

// LoadSnapshot 加载快照元数据（按 Name 查找）
func LoadSnapshot(tx *bolt.Tx, key string) (*snapshots.Info, error) {
	b := tx.Bucket(BucketSnapshots)
	if b == nil {
		return nil, fmt.Errorf("snapshots bucket 不存在")
	}
	data := b.Get([]byte(key))
	if data == nil {
		return nil, fmt.Errorf("快照 %s 不存在", key)
	}
	var info snapshots.Info
	if err := json.Unmarshal(data, &info); err != nil {
		return nil, fmt.Errorf("反序列化快照元数据失败: %w", err)
	}
	return &info, nil
}

// LoadSnapshotByID 按内部数字 ID 加载快照元数据
// 遍历所有快照，找到 ID 字段匹配的记录
func LoadSnapshotByID(tx *bolt.Tx, id string) (*snapshots.Info, error) {
	b := tx.Bucket(BucketSnapshots)
	if b == nil {
		return nil, fmt.Errorf("snapshots bucket 不存在")
	}
	c := b.Cursor()
	for k, v := c.First(); k != nil; k, v = c.Next() {
		// 跳过内部元数据键
		if string(k) == string(nextSnapshotIDKey) {
			continue
		}
		var info snapshots.Info
		if err := json.Unmarshal(v, &info); err != nil {
			continue
		}
		if info.ID == id {
			return &info, nil
		}
	}
	return nil, fmt.Errorf("快照 ID %s 不存在", id)
}

// DeleteSnapshot 删除快照元数据（按 Name 删除）
func DeleteSnapshot(tx *bolt.Tx, key string) error {
	b := tx.Bucket(BucketSnapshots)
	if b == nil {
		return fmt.Errorf("snapshots bucket 不存在")
	}
	return b.Delete([]byte(key))
}

// CountSnapshotsByParent 返回以 parentKey 为父快照的子快照数量
// 用于 Remove 前检查是否仍有子快照引用该快照
func CountSnapshotsByParent(tx *bolt.Tx, parentKey string) (int, error) {
	count := 0
	if err := WalkSnapshots(tx, func(info *snapshots.Info) error {
		if info.Parent == parentKey {
			count++
		}
		return nil
	}); err != nil {
		return 0, err
	}
	return count, nil
}

// DeleteSnapshotByID 按内部数字 ID 删除快照元数据
// 先通过 ID 查找快照的 Name，再按 Name 删除
func DeleteSnapshotByID(tx *bolt.Tx, id string) error {
	b := tx.Bucket(BucketSnapshots)
	if b == nil {
		return fmt.Errorf("snapshots bucket 不存在")
	}
	c := b.Cursor()
	for k, v := c.First(); k != nil; k, v = c.Next() {
		// 跳过内部元数据键
		if string(k) == string(nextSnapshotIDKey) {
			continue
		}
		var info snapshots.Info
		if err := json.Unmarshal(v, &info); err != nil {
			continue
		}
		if info.ID == id {
			return b.Delete(k)
		}
	}
	return fmt.Errorf("快照 ID %s 不存在", id)
}

// WalkSnapshots 遍历所有快照
// 回调函数接收每个快照的 Info，返回 error 可中断遍历
func WalkSnapshots(tx *bolt.Tx, fn func(*snapshots.Info) error) error {
	b := tx.Bucket(BucketSnapshots)
	if b == nil {
		return fmt.Errorf("snapshots bucket 不存在")
	}
	c := b.Cursor()
	for k, v := c.First(); k != nil; k, v = c.Next() {
		// 跳过内部元数据键
		if string(k) == string(nextSnapshotIDKey) {
			continue
		}
		var info snapshots.Info
		if err := json.Unmarshal(v, &info); err != nil {
			continue
		}
		if err := fn(&info); err != nil {
			return err
		}
	}
	return nil
}

// WalkSnapshotsByID 遍历所有快照，提供按内部 ID 查找孤立目录的能力
// 与 WalkSnapshots 的区别：回调额外传入快照的内部 ID，
// 调用方可将数据库中的 ID 集合与磁盘上的目录名对比，发现孤立目录
func WalkSnapshotsByID(tx *bolt.Tx, fn func(id string, info *snapshots.Info) error) error {
	b := tx.Bucket(BucketSnapshots)
	if b == nil {
		return fmt.Errorf("snapshots bucket 不存在")
	}
	c := b.Cursor()
	for k, v := c.First(); k != nil; k, v = c.Next() {
		// 跳过内部元数据键
		if string(k) == string(nextSnapshotIDKey) {
			continue
		}
		var info snapshots.Info
		if err := json.Unmarshal(v, &info); err != nil {
			continue
		}
		if err := fn(info.ID, &info); err != nil {
			return err
		}
	}
	return nil
}

// GetSnapshotIDMap 返回所有已存在快照的内部 ID 集合
// 用于清理时对比磁盘目录，发现孤立目录（磁盘上有但数据库中无对应 ID 的目录）
func GetSnapshotIDMap(tx *bolt.Tx) (map[string]struct{}, error) {
	b := tx.Bucket(BucketSnapshots)
	if b == nil {
		return nil, fmt.Errorf("snapshots bucket 不存在")
	}
	idMap := make(map[string]struct{})
	c := b.Cursor()
	for k, v := c.First(); k != nil; k, v = c.Next() {
		// 跳过内部元数据键
		if string(k) == string(nextSnapshotIDKey) {
			continue
		}
		var info snapshots.Info
		if err := json.Unmarshal(v, &info); err != nil {
			continue
		}
		if info.ID != "" {
			idMap[info.ID] = struct{}{}
		}
	}
	return idMap, nil
}
