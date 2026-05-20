//go:build linux

package container

/*
=======================================================================
  Capability —— Linux 能力限制，对齐 Docker 的安全模型
=======================================================================

  为什么需要 Capability？
  ─────────────────────────────────────────────────────────────

  Linux 传统模型：root 拥有所有权限，普通用户几乎没有权限。
  问题：容器内进程以 root 运行，拥有全部内核权限，非常危险！

  Capability 的解决方案：
  把 root 的权限拆分成约 40 个细粒度的"能力"（Capability），
  每个能力控制一类特权操作。

  Docker 的默认 Capability（14个）：
  ┌──────────────────────┬────────────────────────────────────────┐
  │ CAP_CHOWN            │ 修改文件所有者                          │
  │ CAP_DAC_OVERRIDE     │ 绕过文件权限检查                        │
  │ CAP_FSETID           │ 设置 SUID/SGID 位                      │
  │ CAP_FOWNER           │ 绕过文件所有者检查                      │
  │ CAP_MKNOD            │ 创建设备节点                            │
  │ CAP_NET_RAW          │ 使用原始套接字（ping 等）               │
  │ CAP_SETGID           │ 改变进程 GID                           │
  │ CAP_SETUID           │ 改变进程 UID                           │
  │ CAP_SETFCAP          │ 设置文件 Capability                    │
  │ CAP_SETPCAP          │ 修改进程 Capability                    │
  │ CAP_NET_BIND_SERVICE │ 绑定 1024 以下端口                     │
  │ CAP_SYS_CHROOT       │ 使用 chroot                            │
  │ CAP_KILL             │ 发送信号给其他用户进程                  │
  │ CAP_AUDIT_WRITE      │ 写审计日志                              │
  └──────────────────────┴────────────────────────────────────────┘

  Docker 默认不包含（被裁剪）的 Capability：
  ┌──────────────────────┬────────────────────────────────────────┐
  │ CAP_NET_ADMIN        │ 修改网络配置（路由表、iptables等）       │
  │ CAP_SYS_ADMIN        │ 最危险的能力（mount、hostname等）        │
  │ CAP_SYS_PTRACE       │ 跟踪其他进程                            │
  │ CAP_SYS_MODULE       │ 加载内核模块                            │
  │ CAP_SYS_RAWIO        │ 直接访问硬件                            │
  │ CAP_SYS_BOOT         │ 重启系统                                │
  │ CAP_SYS_TIME         │ 修改系统时间                            │
  │ ...                  │ 更多危险能力                            │
  └──────────────────────┴────────────────────────────────────────┘

  Docker 的 --cap-add / --cap-drop 参数：
  docker run --cap-drop ALL --cap-add NET_BIND_SERVICE nginx
  → 丢弃所有能力，只保留绑定低端口的能力

  实现原理：
  prctl(PR_CAPBSET_DROP, cap, 0, 0, 0)
  → 从进程的"能力边界集"中移除指定能力
  → 子进程继承父进程的能力边界集
  → 效果：容器内进程无法使用被移除的能力

=======================================================================
*/

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// Docker 默认授予容器的 14 个 Capability
var DefaultCapabilities = []int{
	unix.CAP_CHOWN,
	unix.CAP_DAC_OVERRIDE,
	unix.CAP_FSETID,
	unix.CAP_FOWNER,
	unix.CAP_MKNOD,
	unix.CAP_NET_RAW,
	unix.CAP_SETGID,
	unix.CAP_SETUID,
	unix.CAP_SETFCAP,
	unix.CAP_SETPCAP,
	unix.CAP_NET_BIND_SERVICE,
	unix.CAP_SYS_CHROOT,
	unix.CAP_KILL,
	unix.CAP_AUDIT_WRITE,
}

// 所有已知的 Capability（用于 --cap-drop ALL）
var AllKnownCapabilities = []int{
	unix.CAP_CHOWN,
	unix.CAP_DAC_OVERRIDE,
	unix.CAP_DAC_READ_SEARCH,
	unix.CAP_FOWNER,
	unix.CAP_FSETID,
	unix.CAP_KILL,
	unix.CAP_SETGID,
	unix.CAP_SETUID,
	unix.CAP_SETPCAP,
	unix.CAP_LINUX_IMMUTABLE,
	unix.CAP_NET_BIND_SERVICE,
	unix.CAP_NET_BROADCAST,
	unix.CAP_NET_ADMIN,
	unix.CAP_NET_RAW,
	unix.CAP_IPC_LOCK,
	unix.CAP_IPC_OWNER,
	unix.CAP_SYS_MODULE,
	unix.CAP_SYS_RAWIO,
	unix.CAP_SYS_CHROOT,
	unix.CAP_SYS_PTRACE,
	unix.CAP_SYS_PACCT,
	unix.CAP_SYS_ADMIN,
	unix.CAP_SYS_BOOT,
	unix.CAP_SYS_NICE,
	unix.CAP_SYS_RESOURCE,
	unix.CAP_SYS_TIME,
	unix.CAP_SYS_TTY_CONFIG,
	unix.CAP_MKNOD,
	unix.CAP_LEASE,
	unix.CAP_AUDIT_WRITE,
	unix.CAP_AUDIT_CONTROL,
	unix.CAP_SETFCAP,
	unix.CAP_MAC_OVERRIDE,
	unix.CAP_MAC_ADMIN,
	unix.CAP_SYSLOG,
	unix.CAP_WAKE_ALARM,
	unix.CAP_BLOCK_SUSPEND,
	unix.CAP_AUDIT_READ,
}

// CapNameToValue 将 Capability 名称映射为数值
var CapNameToValue = map[string]int{
	"CHOWN":            unix.CAP_CHOWN,
	"DAC_OVERRIDE":     unix.CAP_DAC_OVERRIDE,
	"DAC_READ_SEARCH":  unix.CAP_DAC_READ_SEARCH,
	"FOWNER":           unix.CAP_FOWNER,
	"FSETID":           unix.CAP_FSETID,
	"KILL":             unix.CAP_KILL,
	"SETGID":           unix.CAP_SETGID,
	"SETUID":           unix.CAP_SETUID,
	"SETPCAP":          unix.CAP_SETPCAP,
	"NET_BIND_SERVICE": unix.CAP_NET_BIND_SERVICE,
	"NET_BROADCAST":    unix.CAP_NET_BROADCAST,
	"NET_ADMIN":        unix.CAP_NET_ADMIN,
	"NET_RAW":          unix.CAP_NET_RAW,
	"IPC_LOCK":         unix.CAP_IPC_LOCK,
	"IPC_OWNER":        unix.CAP_IPC_OWNER,
	"SYS_MODULE":       unix.CAP_SYS_MODULE,
	"SYS_RAWIO":        unix.CAP_SYS_RAWIO,
	"SYS_CHROOT":       unix.CAP_SYS_CHROOT,
	"SYS_PTRACE":       unix.CAP_SYS_PTRACE,
	"SYS_PACCT":        unix.CAP_SYS_PACCT,
	"SYS_ADMIN":        unix.CAP_SYS_ADMIN,
	"SYS_BOOT":         unix.CAP_SYS_BOOT,
	"SYS_NICE":         unix.CAP_SYS_NICE,
	"SYS_RESOURCE":     unix.CAP_SYS_RESOURCE,
	"SYS_TIME":         unix.CAP_SYS_TIME,
	"SYS_TTY_CONFIG":   unix.CAP_SYS_TTY_CONFIG,
	"MKNOD":            unix.CAP_MKNOD,
	"LEASE":            unix.CAP_LEASE,
	"AUDIT_WRITE":      unix.CAP_AUDIT_WRITE,
	"AUDIT_CONTROL":    unix.CAP_AUDIT_CONTROL,
	"SETFCAP":          unix.CAP_SETFCAP,
	"MAC_OVERRIDE":     unix.CAP_MAC_OVERRIDE,
	"MAC_ADMIN":        unix.CAP_MAC_ADMIN,
	"SYSLOG":           unix.CAP_SYSLOG,
	"WAKE_ALARM":       unix.CAP_WAKE_ALARM,
	"BLOCK_SUSPEND":    unix.CAP_BLOCK_SUSPEND,
	"AUDIT_READ":       unix.CAP_AUDIT_READ,
}

// CapValueToName 将 Capability 数值映射为名称
var CapValueToName map[int]string

func init() {
	CapValueToName = make(map[int]string)
	for name, val := range CapNameToValue {
		CapValueToName[val] = name
	}
}

// ResolveCapName 解析 Capability 名称（支持带/不带 CAP_ 前缀）
func ResolveCapName(name string) (int, error) {
	// 去掉 CAP_ 前缀
	name = strings.TrimPrefix(strings.ToUpper(name), "CAP_")

	if val, ok := CapNameToValue[name]; ok {
		return val, nil
	}
	return 0, fmt.Errorf("未知的 Capability: %s", name)
}

// ApplyCapabilities 在容器 init 进程中应用 Capability 限制
// capAdd: 额外添加的 Capability 名称列表
// capDrop: 需要移除的 Capability 名称列表
//
// 实现逻辑（对齐 Docker）：
// 1. 从 DefaultCapabilities 开始
// 2. 应用 capDrop（移除指定的能力）
// 3. 应用 capAdd（添加指定的能力）
// 4. 计算需要 drop 的能力 = 全集 - 最终保留的集合
// 5. 对每个需要 drop 的能力调用 prctl(PR_CAPBSET_DROP)
func ApplyCapabilities(capAdd []string, capDrop []string) error {
	// 从默认集合开始
	keepSet := make(map[int]bool)
	for _, cap := range DefaultCapabilities {
		keepSet[cap] = true
	}

	// 处理 capDrop
	for _, name := range capDrop {
		if strings.ToUpper(name) == "ALL" {
			// --cap-drop ALL：清空所有能力
			keepSet = make(map[int]bool)
			continue
		}
		cap, err := ResolveCapName(name)
		if err != nil {
			fmt.Printf("  警告: %v，跳过\\n", err)
			continue
		}
		delete(keepSet, cap)
	}

	// 处理 capAdd
	for _, name := range capAdd {
		cap, err := ResolveCapName(name)
		if err != nil {
			fmt.Printf("  警告: %v，跳过\\n", err)
			continue
		}
		keepSet[cap] = true
	}

	// 计算需要 drop 的能力
	var toDrop []int
	for _, cap := range AllKnownCapabilities {
		if !keepSet[cap] {
			toDrop = append(toDrop, cap)
		}
	}

	// 执行 drop
	for _, cap := range toDrop {
		if err := dropCapability(cap); err != nil {
			// 某些 Capability 可能不受当前内核支持，仅警告
			fmt.Printf("  提示: drop CAP_%s 失败（可能不受支持）: %v\\n",
				CapValueToName[cap], err)
		}
	}

	return nil
}

// dropCapability 从当前进程的能力边界集中移除一个能力
// 底层调用: prctl(PR_CAPBSET_DROP, cap, 0, 0, 0)
func dropCapability(cap int) error {
	_, _, errno := syscall.Syscall6(
		unix.SYS_PRCTL,
		unix.PR_CAPBSET_DROP,
		uintptr(cap),
		0, 0, 0, 0,
	)
	if errno != 0 {
		return fmt.Errorf("prctl(PR_CAPBSET_DROP, %d) 失败: %v", cap, errno)
	}
	return nil
}

// SetCapabilitiesForCmd 在 fork 子进程前设置 Capability（通过 SysProcAttr）
// 注意：由于 Go 的 exec.Cmd 不直接支持设置 Capability，
// 我们需要在 init 进程内（fork 之后）调用 ApplyCapabilities。
// 这个函数用于在环境变量中传递 capAdd/capDrop 信息给 init 进程。
func SetCapabilitiesEnv(cmd *exec.Cmd, capAdd []string, capDrop []string) {
	if len(capAdd) > 0 {
		cmd.Env = append(cmd.Env, "MINI_DOCKER_CAP_ADD="+strings.Join(capAdd, ","))
	}
	if len(capDrop) > 0 {
		cmd.Env = append(cmd.Env, "MINI_DOCKER_CAP_DROP="+strings.Join(capDrop, ","))
	}
}

// ApplyCapabilitiesFromEnv 从环境变量读取并应用 Capability 设置
// 在 init 进程中调用
func ApplyCapabilitiesFromEnv() {
	capAddStr := os.Getenv("MINI_DOCKER_CAP_ADD")
	capDropStr := os.Getenv("MINI_DOCKER_CAP_DROP")

	if capAddStr == "" && capDropStr == "" {
		// 没有指定，使用默认 Capability
		return
	}

	var capAdd []string
	var capDrop []string

	if capAddStr != "" {
		capAdd = strings.Split(capAddStr, ",")
	}
	if capDropStr != "" {
		capDrop = strings.Split(capDropStr, ",")
	}

	if err := ApplyCapabilities(capAdd, capDrop); err != nil {
		fmt.Printf("  警告: 设置 Capability 失败: %v\\n", err)
	}
}
