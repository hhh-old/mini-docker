package events

import (
	"encoding/json"
	"fmt"
	"time"
)

// Envelope 是事件信封，与 containerd 的 events.Envelope 对齐。
// Namespace 字段先预留，默认值为 "default"。
type Envelope struct {
	Timestamp time.Time   `json:"timestamp"`
	Namespace string      `json:"namespace,omitempty"`
	Topic     string      `json:"topic"`
	Event     interface{} `json:"event"`
}

// TypedEvent 用于 JSON 反序列化时保存具体事件类型。
type TypedEvent struct {
	Timestamp time.Time       `json:"timestamp"`
	Namespace string          `json:"namespace,omitempty"`
	Topic     string          `json:"topic"`
	Event     json.RawMessage `json:"event"`
}

// ContainerCreate 容器创建事件
type ContainerCreate struct {
	ContainerID string `json:"container_id"`
	Image       string `json:"image"`
}

// ContainerDelete 容器删除事件
type ContainerDelete struct {
	ContainerID string `json:"container_id"`
}

// ContainerUpdate 容器更新事件
type ContainerUpdate struct {
	ContainerID string `json:"container_id"`
}

// ImagePull 镜像拉取/创建事件
type ImagePull struct {
	Name    string `json:"name"`
	Tag     string `json:"tag"`
	ImageID string `json:"image_id"`
}

// ImageDelete 镜像删除事件
type ImageDelete struct {
	Name    string `json:"name"`
	Tag     string `json:"tag"`
	ImageID string `json:"image_id"`
}

// ImageCommit 镜像提交事件
type ImageCommit struct {
	Name    string `json:"name"`
	Tag     string `json:"tag"`
	ImageID string `json:"image_id"`
}

// TaskCreate task 创建事件
type TaskCreate struct {
	ContainerID string `json:"container_id"`
	Image       string `json:"image,omitempty"`
	PID         int    `json:"pid,omitempty"`
}

// TaskStart task 启动事件
type TaskStart struct {
	ContainerID string `json:"container_id"`
	PID         int    `json:"pid,omitempty"`
}

// TaskExit task 退出事件
type TaskExit struct {
	ContainerID string `json:"container_id"`
	ExitCode    int    `json:"exit_code"`
}

// TaskDelete task 删除事件
type TaskDelete struct {
	ContainerID string `json:"container_id"`
}

// TaskPause task 暂停事件
type TaskPause struct {
	ContainerID string `json:"container_id"`
}

// TaskResume task 恢复事件
type TaskResume struct {
	ContainerID string `json:"container_id"`
}

// TaskUnhealthy task 健康检查失败事件
type TaskUnhealthy struct {
	ContainerID string `json:"container_id"`
	Message     string `json:"message"`
}

// GCRun 垃圾回收事件
type GCRun struct {
	RemovedImages    int `json:"removed_images"`
	RemovedLayers    int `json:"removed_layers"`
	RemovedSnapshots int `json:"removed_snapshots"`
}

// MatchTopic 判断 topic 是否匹配 filters 中的 glob 规则。
// 空 filters 表示全匹配。
// 规则："*" 匹配单级，"**" 匹配多级。
func MatchTopic(topic string, filters []string) bool {
	if len(filters) == 0 {
		return true
	}
	for _, f := range filters {
		if matchGlob(topic, f) {
			return true
		}
	}
	return false
}

func matchGlob(s, pattern string) bool {
	// 简单实现：按 '/' 分段，"**" 匹配任意剩余段
	spath := splitPath(s)
	ppath := splitPath(pattern)
	return matchSegments(spath, ppath)
}

func splitPath(p string) []string {
	var parts []string
	start := 0
	for i := 0; i < len(p); i++ {
		if p[i] == '/' {
			if i > start {
				parts = append(parts, p[start:i])
			}
			start = i + 1
		}
	}
	if start < len(p) {
		parts = append(parts, p[start:])
	}
	return parts
}

func matchSegments(s, p []string) bool {
	if len(p) == 0 {
		return len(s) == 0
	}
	if p[0] == "**" {
		if len(p) == 1 {
			return true
		}
		for i := 0; i <= len(s); i++ {
			if matchSegments(s[i:], p[1:]) {
				return true
			}
		}
		return false
	}
	if len(s) == 0 {
		return false
	}
	if p[0] != "*" && p[0] != s[0] {
		return false
	}
	return matchSegments(s[1:], p[1:])
}

// EventTypeName 返回事件结构体的短名称，用于日志或调试。
func EventTypeName(ev interface{}) string {
	if ev == nil {
		return "nil"
	}
	return fmt.Sprintf("%T", ev)
}
