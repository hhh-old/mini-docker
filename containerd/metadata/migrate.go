package metadata

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	bolt "go.etcd.io/bbolt"
)

// MigrateFromJSON 从旧版 JSON 文件迁移到 boltdb
// 检测 imagedb/ + layerdb/ + repositories.json 是否存在，
// 如果存在则读取并写入 boltdb
// 返回: 迁移的项目数, error
func MigrateFromJSON(db *DB, imageDBDir, layerDBDir, tagDBPath, layerStoreDir, imageStoreDir string) (int, error) {
	count := 0

	// 如果 boltdb 已有数据，跳过迁移
	hasData := false
	err := db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(BucketImages)
		if b != nil {
			k, _ := b.Cursor().First()
			if k != nil {
				hasData = true
			}
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	if hasData {
		return 0, nil
	}

	// 迁移 repositories.json → BucketTags
	if n, err := migrateTagDB(db, tagDBPath); err != nil {
		fmt.Printf("  警告: 迁移标签数据库失败: %v\n", err)
	} else {
		count += n
	}

	// 迁移 imagedb/*.json → BucketImages
	if n, err := migrateImageDB(db, imageDBDir); err != nil {
		fmt.Printf("  警告: 迁移镜像数据库失败: %v\n", err)
	} else {
		count += n
	}

	// 迁移 layerdb/* → BucketLayers
	if n, err := migrateLayerDB(db, layerDBDir); err != nil {
		fmt.Printf("  警告: 迁移层数据库失败: %v\n", err)
	} else {
		count += n
	}

	// 迁移完成后将旧文件重命名为 .bak
	if count > 0 {
		renameToBackup(tagDBPath)
		renameDirToBackup(imageDBDir)
		renameDirToBackup(layerDBDir)
	}

	return count, nil
}

// migrateTagDB 迁移 repositories.json
func migrateTagDB(db *DB, tagDBPath string) (int, error) {
	data, err := os.ReadFile(tagDBPath)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}

	// 解析旧格式: map[name]map[tag]imageID
	var tagDB map[string]map[string]string
	if err := json.Unmarshal(data, &tagDB); err != nil {
		return 0, err
	}

	count := 0
	err = db.Update(func(tx *bolt.Tx) error {
		for name, tags := range tagDB {
			for tag, imageID := range tags {
				if err := SaveTag(tx, name, tag, imageID); err != nil {
					return err
				}
				count++
			}
		}
		return nil
	})
	return count, err
}

// migrateImageDB 迁移 imagedb/*.json
func migrateImageDB(db *DB, imageDBDir string) (int, error) {
	entries, err := os.ReadDir(imageDBDir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}

	count := 0
	err = db.Update(func(tx *bolt.Tx) error {
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
				continue
			}

			path := filepath.Join(imageDBDir, entry.Name())
			data, err := os.ReadFile(path)
			if err != nil {
				continue
			}

			var m ImageManifest
			if err := json.Unmarshal(data, &m); err != nil {
				continue
			}

			if err := SaveImage(tx, &m); err != nil {
				return err
			}
			count++
		}
		return nil
	})
	return count, err
}

// migrateLayerDB 迁移 layerdb/*
// 旧格式: layerdb/<digest>/cache-id, size, diff
func migrateLayerDB(db *DB, layerDBDir string) (int, error) {
	entries, err := os.ReadDir(layerDBDir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}

	count := 0
	err = db.Update(func(tx *bolt.Tx) error {
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}

			digest := entry.Name()
			layerDir := filepath.Join(layerDBDir, digest)

			info := &LayerInfo{Digest: digest}

			// 读取 cache-id
			if data, err := os.ReadFile(filepath.Join(layerDir, "cache-id")); err == nil {
				info.CacheID = strings.TrimSpace(string(data))
			}

			// 读取 size
			if data, err := os.ReadFile(filepath.Join(layerDir, "size")); err == nil {
				var size int64
				fmt.Sscanf(strings.TrimSpace(string(data)), "%d", &size)
				info.Size = size
			}

			// 读取 diff (diff_id)
			if data, err := os.ReadFile(filepath.Join(layerDir, "diff")); err == nil {
				diffID := strings.TrimSpace(string(data))
				if diffID != "" {
					info.DiffID = diffID
				}
			}

			if err := SaveLayer(tx, info); err != nil {
				return err
			}
			count++
		}
		return nil
	})
	return count, err
}

// renameToBackup 将文件重命名为 .bak
func renameToBackup(path string) {
	if _, err := os.Stat(path); err != nil {
		return
	}
	os.Rename(path, path+".bak")
}

// renameDirToBackup 将目录重命名为 .bak
func renameDirToBackup(dir string) {
	if _, err := os.Stat(dir); err != nil {
		return
	}
	os.Rename(dir, dir+".bak")
}
