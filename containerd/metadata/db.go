package metadata

import (
	"os"
	"path/filepath"
	"time"

	bolt "go.etcd.io/bbolt"
)

// DB 封装 boltdb 数据库，对齐 containerd 的 metadata 存储架构
// 替代原有散落的 JSON 文件 (repositories.json + imagedb/*.json + layerdb/*)
// 优势：事务保证、原子操作、避免并发导致的引用断裂
type DB struct {
	db *bolt.DB
}

// Bucket 名称常量（对齐 containerd 的 boltdb schema）
// bbolt 数据库只认 []byte 类型。
// bbolt 是一个底层的键值存储引擎，它的 API 设计非常纯粹，所有的 Key（键）和 Bucket 的名字都必须是字节切片（[]byte）。
// []byte("images")：字符串转字节切片（类型转换）
// []byte{"images"}：切片字面量初始化
// bbolt 这种键值数据库中，数据的存储层级是这样的：
// 数据库 (DB) -> 桶 (Bucket) -> 键值对 (Key-Value)
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

	// 设置 Timeout 避免 boltdb 在 WSL 等环境下因文件锁残留而永久阻塞
	// 如果另一个进程持有锁，等待 5 秒后超时返回错误
	opts := &bolt.Options{Timeout: 5 * time.Second}
	db, err := bolt.Open(path, 0600, opts) //如果路径下的文件不存在，它会自动帮你创建一个新的数据库文件，程序不会报错。
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
