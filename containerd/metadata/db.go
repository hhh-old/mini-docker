package metadata

import (
	"os"
	"path/filepath"

	bolt "go.etcd.io/bbolt"
)

// DB 封装 boltdb 数据库，对齐 containerd 的 metadata 存储架构
// 替代原有散落的 JSON 文件 (repositories.json + imagedb/*.json + layerdb/*)
// 优势：事务保证、原子操作、避免并发导致的引用断裂
type DB struct {
	db *bolt.DB
}

// Bucket 名称常量（对齐 containerd 的 boltdb schema）
var (
	BucketImages    = []byte("images")    // image_id → ImageManifest JSON
	BucketLayers    = []byte("layers")    // digest → LayerInfo JSON
	BucketTags      = []byte("tags")      // "name:tag" → image_id
	BucketSnapshots = []byte("snapshots") // snap_key → SnapshotInfo JSON
	BucketLeases    = []byte("leases")    // lease_id → LeaseInfo JSON
	BucketContent   = []byte("content")   // digest → Info JSON
)

// allBuckets 所有需要初始化的 Bucket 列表
var allBuckets = [][]byte{
	BucketImages,
	BucketLayers,
	BucketTags,
	BucketSnapshots,
	BucketLeases,
	BucketContent,
}

// Open 打开 metadata 数据库
// path: 数据库文件路径 (如 /var/lib/mini-docker/metadata.db)
func Open(path string) (*DB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return nil, err
	}

	db, err := bolt.Open(path, 0600, nil)
	if err != nil {
		return nil, err
	}

	// 初始化所有 Bucket
	err = db.Update(func(tx *bolt.Tx) error {
		for _, name := range allBuckets {
			if _, err := tx.CreateBucketIfNotExists(name); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		db.Close()
		return nil, err
	}

	return &DB{db: db}, nil
}

// Close 关闭数据库
func (d *DB) Close() error {
	return d.db.Close()
}

// Update 执行读写事务
func (d *DB) Update(fn func(*bolt.Tx) error) error {
	return d.db.Update(fn)
}

// View 执行只读事务
func (d *DB) View(fn func(*bolt.Tx) error) error {
	return d.db.View(fn)
}
