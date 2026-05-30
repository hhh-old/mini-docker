//go:build linux

package containerinit

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
	"strings"

	"mini-docker/libcontainer/configs"
	"mini-docker/utils"
)

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
	keepSet := make(map[int]bool)
	for _, name := range configs.DefaultCapabilities {
		name = strings.TrimPrefix(name, "CAP_")
		if val, ok := configs.CapNameToValue[name]; ok {
			keepSet[val] = true
		}
	}
	// 2. 应用 capDrop
	for _, name := range capDrop {
		if strings.ToUpper(name) == "ALL" {
			keepSet = make(map[int]bool)
			continue
		}
		cap, err := configs.ResolveCapName(name)
		if err != nil {
			fmt.Printf("  警告: %v，跳过\n", err)
			continue
		}
		delete(keepSet, cap) // 从保留集中移除
	}
	// 3. 应用 capAdd
	for _, name := range capAdd {
		cap, err := configs.ResolveCapName(name)
		if err != nil {
			fmt.Printf("  警告: %v，跳过\n", err)
			continue
		}
		keepSet[cap] = true // 添加到保留集
	}
	// 4. 计算最终需要 drop 的能力
	var toDrop []int
	for _, capName := range configs.AllKnownCapabilities {
		capName = strings.TrimPrefix(capName, "CAP_")
		val, ok := configs.CapNameToValue[capName]
		if !ok {
			continue
		}
		if !keepSet[val] {
			toDrop = append(toDrop, val)
		}
	}
	// 5. 对每个需要 drop 的能力调用系统调用
	for _, cap := range toDrop {
		if err := utils.DropCapability(cap); err != nil {
			fmt.Printf("  提示: drop CAP_%s 失败（可能不受支持）: %v\n",
				configs.CapValueToName[cap], err)
		}
	}

	return nil
}

// ApplyCapabilitiesFromEnv 从环境变量读取并应用 Capability 设置
// 在 init 进程中调用
func ApplyCapabilitiesFromEnv() {
	capAddStr := os.Getenv("MINI_DOCKER_CAP_ADD")
	capDropStr := os.Getenv("MINI_DOCKER_CAP_DROP")

	if capAddStr == "" && capDropStr == "" {
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
		fmt.Printf("  警告: 设置 Capability 失败: %v\n", err)
	}
}
