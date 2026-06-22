package events

import "time"

// Service 是事件总线服务，作为 containerd 插件对外暴露。
// 所有服务插件都通过它发布事件，外部订阅者通过它订阅或查询归档。
type Service struct {
	bus *EventBus
}

// NewService 创建事件服务。
func NewService() *Service {
	return &Service{bus: NewEventBus()}
}

// Publish 发布事件。
func (s *Service) Publish(ev *Envelope) {
	s.bus.Publish(ev)
}

// Subscribe 创建订阅。
func (s *Service) Subscribe(filters ...string) (*subscription, error) {
	return s.bus.Subscribe(filters...)
}

// Unsubscribe 取消订阅。
func (s *Service) Unsubscribe(sub *subscription) {
	s.bus.Unsubscribe(sub)
}

// GetArchive 返回历史事件归档。
func (s *Service) GetArchive(since, until time.Time) []*Envelope {
	return s.bus.GetArchive(since, until)
}
