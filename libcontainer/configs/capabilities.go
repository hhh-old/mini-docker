package configs

import "fmt"

// Docker 默认授予容器的 14 个 Capability
var DefaultCapabilities = []string{
	"CAP_CHOWN",
	"CAP_DAC_OVERRIDE",
	"CAP_FSETID",
	"CAP_FOWNER",
	"CAP_MKNOD",
	"CAP_NET_RAW",
	"CAP_SETGID",
	"CAP_SETUID",
	"CAP_SETFCAP",
	"CAP_SETPCAP",
	"CAP_NET_BIND_SERVICE",
	"CAP_SYS_CHROOT",
	"CAP_KILL",
	"CAP_AUDIT_WRITE",
}

// AllKnownCapabilities 所有已知的 Capability
var AllKnownCapabilities = []string{
	"CAP_CHOWN",
	"CAP_DAC_OVERRIDE",
	"CAP_DAC_READ_SEARCH",
	"CAP_FOWNER",
	"CAP_FSETID",
	"CAP_KILL",
	"CAP_SETGID",
	"CAP_SETUID",
	"CAP_SETPCAP",
	"CAP_LINUX_IMMUTABLE",
	"CAP_NET_BIND_SERVICE",
	"CAP_NET_BROADCAST",
	"CAP_NET_ADMIN",
	"CAP_NET_RAW",
	"CAP_IPC_LOCK",
	"CAP_IPC_OWNER",
	"CAP_SYS_MODULE",
	"CAP_SYS_RAWIO",
	"CAP_SYS_CHROOT",
	"CAP_SYS_PTRACE",
	"CAP_SYS_PACCT",
	"CAP_SYS_ADMIN",
	"CAP_SYS_BOOT",
	"CAP_SYS_NICE",
	"CAP_SYS_RESOURCE",
	"CAP_SYS_TIME",
	"CAP_SYS_TTY_CONFIG",
	"CAP_MKNOD",
	"CAP_LEASE",
	"CAP_AUDIT_WRITE",
	"CAP_AUDIT_CONTROL",
	"CAP_SETFCAP",
	"CAP_MAC_OVERRIDE",
	"CAP_MAC_ADMIN",
	"CAP_SYSLOG",
	"CAP_WAKE_ALARM",
	"CAP_BLOCK_SUSPEND",
	"CAP_AUDIT_READ",
}

// CapNameToValue Capability 名称到数值的映射
var CapNameToValue = map[string]int{
	"CHOWN":            0,
	"DAC_OVERRIDE":     1,
	"DAC_READ_SEARCH":  2,
	"FOWNER":           3,
	"FSETID":           4,
	"KILL":             5,
	"SETGID":           6,
	"SETUID":           7,
	"SETPCAP":          8,
	"LINUX_IMMUTABLE":  9,
	"NET_BIND_SERVICE": 10,
	"NET_BROADCAST":    11,
	"NET_ADMIN":        12,
	"NET_RAW":          13,
	"IPC_LOCK":         14,
	"IPC_OWNER":        15,
	"SYS_MODULE":       16,
	"SYS_RAWIO":        17,
	"SYS_CHROOT":       18,
	"SYS_PTRACE":       19,
	"SYS_PACCT":        20,
	"SYS_ADMIN":        21,
	"SYS_BOOT":         22,
	"SYS_NICE":         23,
	"SYS_RESOURCE":     24,
	"SYS_TIME":         25,
	"SYS_TTY_CONFIG":   26,
	"MKNOD":            27,
	"LEASE":            28,
	"AUDIT_WRITE":      29,
	"AUDIT_CONTROL":    30,
	"SETFCAP":          31,
	"MAC_OVERRIDE":     32,
	"MAC_ADMIN":        33,
	"SYSLOG":           34,
	"WAKE_ALARM":       35,
	"BLOCK_SUSPEND":    36,
	"AUDIT_READ":       37,
}

// CapValueToName Capability 数值到名称的映射
var CapValueToName map[int]string

func init() {
	CapValueToName = make(map[int]string)
	for name, val := range CapNameToValue {
		CapValueToName[val] = name
	}
}

// ResolveCapName 解析 Capability 名称（支持带/不带 CAP_ 前缀）
func ResolveCapName(name string) (int, error) {
	if len(name) > 4 && name[:4] == "CAP_" {
		name = name[4:]
	}
	if val, ok := CapNameToValue[name]; ok {
		return val, nil
	}
	return 0, fmt.Errorf("未知的 Capability: %s", name)
}
