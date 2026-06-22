package events

import (
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"
)

// subscription 表示一个事件订阅
type subscription struct {
	id      string
	ch      chan *Envelope
	filters []string
}

// EventBus 实现事件的发布/订阅与归档。
// 与真实 containerd 一样，事件是 ephemeral pub/sub，但保留一份内存归档
// 用于历史查询和短时间回放。
type EventBus struct {
	mu         sync.RWMutex
	subs       []*subscription
	archive    []*Envelope
	maxArchive int
}

// NewEventBus 创建事件总线，默认保留最近 1000 条事件。
func NewEventBus() *EventBus {
	return &EventBus{
		subs:       make([]*subscription, 0),
		archive:    make([]*Envelope, 0),
		maxArchive: 1000,
	}
}

// Publish 发布事件到所有匹配订阅者，并加入归档。
func (eb *EventBus) Publish(ev *Envelope) {
	if ev.Timestamp.IsZero() {
		ev.Timestamp = time.Now()
	}
	if ev.Namespace == "" {
		ev.Namespace = "default"
	}

	eb.mu.Lock()
	if len(eb.archive) >= eb.maxArchive {
		eb.archive = eb.archive[1:]
	}
	eb.archive = append(eb.archive, ev)

	subs := make([]*subscription, len(eb.subs))
	copy(subs, eb.subs)
	eb.mu.Unlock()

	for _, sub := range subs {
		if MatchTopic(ev.Topic, sub.filters) {
			select {
			case sub.ch <- ev:
			default:
				// 订阅者消费过慢，直接丢弃事件，避免阻塞发布者。
			}
		}
	}
}

// Subscribe 创建新订阅，filters 为 topic glob 列表。
func (eb *EventBus) Subscribe(filters ...string) (*subscription, error) {
	sub := &subscription{
		id:      generateSubID(),
		ch:      make(chan *Envelope, 64),
		filters: filters,
	}

	eb.mu.Lock()
	eb.subs = append(eb.subs, sub)
	eb.mu.Unlock()

	return sub, nil
}

// Unsubscribe 取消订阅并关闭通道。
func (eb *EventBus) Unsubscribe(sub *subscription) {
	eb.mu.Lock()
	defer eb.mu.Unlock()

	for i, s := range eb.subs {
		if s.id == sub.id {
			eb.subs = append(eb.subs[:i], eb.subs[i+1:]...)
			close(s.ch)
			return
		}
	}
}

// GetArchive 返回归档副本，支持按时间范围过滤。
// since/until 为零值时不做对应边界限制。
func (eb *EventBus) GetArchive(since, until time.Time) []*Envelope {
	eb.mu.RLock()
	defer eb.mu.RUnlock()

	result := make([]*Envelope, 0, len(eb.archive))
	for _, ev := range eb.archive {
		if !since.IsZero() && ev.Timestamp.Before(since) {
			continue
		}
		if !until.IsZero() && ev.Timestamp.After(until) {
			continue
		}
		result = append(result, ev)
	}
	return result
}

// SubscriptionCh 返回订阅通道。
func (sub *subscription) Ch() <-chan *Envelope {
	return sub.ch
}

func generateSubID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
