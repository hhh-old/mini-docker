//go:build !linux

package images

// DigestToCacheID 已迁移到 content.DigestToCacheID（统一实现，避免多包重复定义）

// LayerDiffDir 非 Linux 桩实现（实际 mini-docker 仅在 Linux 运行容器）
func LayerDiffDir(digest string) string { return "" }

// cleanOpaqueDir 处理 opaque whiteout（非 Linux 桩实现）
func cleanOpaqueDir(dir string) {}

// syscallStat 非 Linux 平台不存在 syscall.Stat_t，定义为空结构体
// calculateRootFSSizeBytes 中 info.Sys() 断言会失败，跳过硬链接去重（非 Linux 无硬链接问题）
type syscallStat struct {
	Dev uint64
	Ino uint64
}
