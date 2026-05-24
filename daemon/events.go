package daemon

/*
=======================================================================
  EventBus —— 容器事件总线
=======================================================================

  对齐 Docker 的事件系统：
  - docker events 命令实时输出容器生命周期事件
  - Daemon 内部各模块通过事件总线解耦

  事件类型：
  - container_create  容器创建
  - container_start   容器启动
  - container_exit    容器退出
  - container_stop    容器停止
  - container_pause   容器暂停
  - container_unpause 容器恢复
  - container_rm      容器删除

=======================================================================
*/

import (
	"sync"
	"time"
)

// Event 容器事件
type Event struct {
	Type      string    `json:"type"`      // 事件类型
	Container string    `json:"container"` // 容器 ID
	ExitCode  int       `json:"exit_code"` // 退出码（仅 container_exit）
	Time      time.Time `json:"time"`      // 事件时间
	Message   string    `json:"message"`   // 事件描述
}

// EventBus 事件总线
type EventBus struct {
	mu         sync.RWMutex
	subs       []chan Event // 订阅者通道
	archive    []Event      // 事件归档（供新订阅者回放）
	maxArchive int          // 最大归档数
}

// NewEventBus 创建事件总线
func NewEventBus() *EventBus {
	return &EventBus{
		subs:       make([]chan Event, 0),
		archive:    make([]Event, 0),
		maxArchive: 1000,
	}
}

// Run 启动事件总线（当前实现中归档是同步的，此处保留扩展空间）
func (eb *EventBus) Run() {
}

// Publish 发布事件
func (eb *EventBus) Publish(event Event) {
	eb.mu.Lock()
	defer eb.mu.Unlock()

	// 归档事件
	if len(eb.archive) >= eb.maxArchive {
		eb.archive = eb.archive[1:]
	}
	eb.archive = append(eb.archive, event)

	// 分发给所有订阅者
	for _, ch := range eb.subs {
		select {
		case ch <- event:
		default:
			// 订阅者消费太慢，丢弃事件（避免阻塞）
		}
	}
}

// Subscribe 订阅事件，返回事件通道
func (eb *EventBus) Subscribe() chan Event {
	eb.mu.Lock()
	defer eb.mu.Unlock()

	ch := make(chan Event, 64)
	eb.subs = append(eb.subs, ch)
	return ch
}

// Unsubscribe 取消订阅
func (eb *EventBus) Unsubscribe(ch chan Event) {
	eb.mu.Lock()
	defer eb.mu.Unlock()

	for i, sub := range eb.subs {
		if sub == ch {
			eb.subs = append(eb.subs[:i], eb.subs[i+1:]...)
			close(ch)
			return
		}
	}
}

// GetArchive 获取事件归档
func (eb *EventBus) GetArchive() []Event {
	eb.mu.RLock()
	defer eb.mu.RUnlock()

	result := make([]Event, len(eb.archive))
	copy(result, eb.archive)
	return result
}
