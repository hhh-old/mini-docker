package gc

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	bolt "go.etcd.io/bbolt"

	"mini-docker/constants"
	"mini-docker/containerd/metadata"
)

type LeaseManager struct {
	db *metadata.DB
}

func NewLeaseManager(db *metadata.DB) *LeaseManager {
	return &LeaseManager{db: db}
}

func (lm *LeaseManager) Create(ctx context.Context) (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("生成 lease ID 失败: %w", err)
	}
	leaseID := hex.EncodeToString(b)

	info := &metadata.LeaseInfo{
		ID:        leaseID,
		CreatedAt: time.Now().Format(constants.TimeFormat),
		Labels:    make(map[string]string),
	}

	if err := lm.db.Update(func(tx *bolt.Tx) error {
		return metadata.SaveLease(tx, info)
	}); err != nil {
		return "", fmt.Errorf("保存租约失败: %w", err)
	}

	return leaseID, nil
}

func (lm *LeaseManager) AddObject(ctx context.Context, leaseID, digest string) error {
	return lm.db.Update(func(tx *bolt.Tx) error {
		if err := metadata.AddLeaseObject(tx, leaseID, digest); err != nil {
			return fmt.Errorf("添加保护对象失败: %w", err)
		}
		return nil
	})
}

func (lm *LeaseManager) Delete(ctx context.Context, leaseID string) error {
	return lm.db.Update(func(tx *bolt.Tx) error {
		if err := metadata.DeleteLease(tx, leaseID); err != nil {
			return fmt.Errorf("删除租约失败: %w", err)
		}
		return nil
	})
}

func (lm *LeaseManager) List(ctx context.Context) ([]string, error) {
	var ids []string
	if err := lm.db.View(func(tx *bolt.Tx) error {
		leases, err := metadata.ListLeases(tx)
		if err != nil {
			return fmt.Errorf("列出租约失败: %w", err)
		}
		for _, l := range leases {
			ids = append(ids, l.ID)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	return ids, nil
}
