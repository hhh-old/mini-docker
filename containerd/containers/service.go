// Package containers 实现容器元数据管理服务
// 对齐 containerd: containers.Store 提供容器元数据的 CRUD 操作
// Daemon 通过 RPC 调用此服务，而非直接操作 boltdb
package containers

import (
	"fmt"

	bolt "go.etcd.io/bbolt"

	"mini-docker/containerd/events"
	"mini-docker/containerd/metadata"
)

// Service 容器管理服务（对齐 containerd: containers.Store）
// 提供容器元数据的 CRUD 操作，Daemon 通过 RPC 调用而非直接操作 boltdb
type Service struct {
	Meta   *metadata.DB
	events *events.Service // 事件总线服务，发布容器元数据变更事件
}

// NewService 创建容器管理服务
func NewService(metaDB *metadata.DB, ev *events.Service) *Service {
	return &Service{Meta: metaDB, events: ev}
}

// publishEvent 发布事件（events 为 nil 时静默跳过）
func (s *Service) publishEvent(topic string, ev interface{}) {
	if s.events == nil {
		return
	}
	s.events.Publish(&events.Envelope{
		Topic: topic,
		Event: ev,
	})
}

// Create 创建容器元数据记录（对齐 containerd: containers.Store.Create）
func (s *Service) Create(info *metadata.ContainerInfo) error {
	if err := s.Meta.Update(func(tx *bolt.Tx) error {
		return metadata.SaveContainer(tx, info)
	}); err != nil {
		return err
	}
	s.publishEvent("/containers/create", events.ContainerCreate{
		ContainerID: info.ID,
		Image:       info.Image,
	})
	return nil
}

// Get 查询容器元数据（对齐 containerd: containers.Store.Get）
// 支持 ID 和名称两种查询方式
func (s *Service) Get(id string) (*metadata.ContainerInfo, error) {
	var info *metadata.ContainerInfo
	// 先按 ID 查找
	err := s.Meta.View(func(tx *bolt.Tx) error {
		var err error
		info, err = metadata.LoadContainer(tx, id)
		return err
	})
	if err == nil && info != nil {
		return info, nil
	}
	// 按 ID 未找到，尝试按名称查找
	err = s.Meta.View(func(tx *bolt.Tx) error {
		var err error
		info, err = metadata.LoadContainerByName(tx, id)
		return err
	})
	if err == nil && info != nil {
		return info, nil
	}
	return nil, fmt.Errorf("容器 %s 不存在", id)
}

// List 列出所有容器元数据（对齐 containerd: containers.Store.List）
func (s *Service) List() ([]*metadata.ContainerInfo, error) {
	var result []*metadata.ContainerInfo
	err := s.Meta.View(func(tx *bolt.Tx) error {
		var err error
		result, err = metadata.ListContainers(tx)
		return err
	})
	return result, err
}

// Update 更新容器元数据（对齐 containerd: containers.Store.Update）
func (s *Service) Update(info *metadata.ContainerInfo) error {
	if err := s.Meta.Update(func(tx *bolt.Tx) error {
		return metadata.SaveContainer(tx, info)
	}); err != nil {
		return err
	}
	s.publishEvent("/containers/update", events.ContainerUpdate{
		ContainerID: info.ID,
	})
	return nil
}

// Delete 删除容器元数据（对齐 containerd: containers.Store.Delete）
func (s *Service) Delete(id string) error {
	if err := s.Meta.Update(func(tx *bolt.Tx) error {
		return metadata.DeleteContainer(tx, id)
	}); err != nil {
		return err
	}
	s.publishEvent("/containers/delete", events.ContainerDelete{
		ContainerID: id,
	})
	return nil
}
