//go:build !linux

package spec

func getNamespaceFlags() map[string]uintptr {
	return map[string]uintptr{
		"pid":     0x20000000,
		"network": 0x40000000,
		"ipc":     0x08000000,
		"uts":     0x04000000,
		"mount":   0x00020000,
		"user":    0x10000000,
		"cgroup":  0x02000000,
	}
}
