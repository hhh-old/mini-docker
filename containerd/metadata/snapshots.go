package metadata

import (
	"encoding/json"
	"fmt"

	bolt "go.etcd.io/bbolt"

	"mini-docker/containerd/snapshots"
)

// SaveSnapshot 保存快照元数据
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

// LoadSnapshot 加载快照元数据
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

// DeleteSnapshot 删除快照元数据
func DeleteSnapshot(tx *bolt.Tx, key string) error {
	b := tx.Bucket(BucketSnapshots)
	if b == nil {
		return fmt.Errorf("snapshots bucket 不存在")
	}
	return b.Delete([]byte(key))
}

// WalkSnapshots 遍历快照
func WalkSnapshots(tx *bolt.Tx, fn func(*snapshots.Info) error) error {
	b := tx.Bucket(BucketSnapshots)
	if b == nil {
		return fmt.Errorf("snapshots bucket 不存在")
	}
	c := b.Cursor()
	for k, v := c.First(); k != nil; k, v = c.Next() {
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
