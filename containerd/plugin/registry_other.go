//go:build !linux

package plugin

// registerLinuxPlugins 非 Linux 平台不注册 Linux 特有插件
func registerLinuxPlugins(m *Manager) {
	// shim / runtime / task 插件依赖 Linux 内核特性，非 Linux 平台不注册
}
