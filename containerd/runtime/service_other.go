//go:build !linux

package runtime

// Service Runtime Manager 非 Linux 平台桩实现
// 在 Linux 上由 service_linux.go 提供完整实现；其他平台仅保证包可编译。
type Service struct{}

// NewService 创建 Runtime Service 桩实现
func NewService(shimMgr interface{}) *Service {
	return &Service{}
}
