//go:build linux

package image

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"mini-docker/utils"
)

/*
=======================================================================
  Busybox 安装模块 —— 为容器 rootfs 自动安装基础命令工具集
=======================================================================

  BusyBox 被称为"嵌入式 Linux 的瑞士军刀"，它将 200+ 个常用
  UNIX 命令（sh, ls, cat, grep, ps, mount, ip, ping 等）
  集成到单个可执行文件中。每个命令称为一个 "applet"。

  BusyBox 的工作原理：
  - 当通过符号链接调用时（如 /bin/sh → busybox），busybox 根据
    argv[0]（即调用时的程序名）判断要执行哪个 applet
  - 也可以通过 "busybox <命令>" 的方式直接调用

  两种编译方式：
  1. 静态链接（busybox-static）：不依赖任何共享库，可直接在
     最小 rootfs 中运行，是容器环境的理想选择
  2. 动态链接（busybox）：依赖 libc.so 等共享库，需要在 rootfs
     中包含对应的库文件才能运行

  本模块的安装流程：
  1. 在宿主机上查找 busybox（优先静态版本）
  2. 若未找到则从 busybox.net 下载静态版本
  3. 将 busybox 复制到 rootfs/bin/ 下
  4. 若为动态链接版本，自动复制所需的共享库
  5. 为所有 applet 创建符号链接（如 /bin/sh → busybox）
=======================================================================
*/

var busyboxSearchPaths = []string{
	"/bin/busybox-static",
	"/usr/bin/busybox-static",
	"/bin/busybox",
	"/usr/bin/busybox",
}

const busyboxDownloadURL = "https://busybox.net/downloads/binaries/1.35.0-x86_64-linux-musl/busybox"

/*
=======================================================================

	setupBusybox —— 将 busybox 安装到容器的 rootfs 中

	整体流程：
	┌─────────────────────────────────────────────────────┐
	│  1. findBusybox()  在宿主机查找 busybox             │
	│     ├─ 找到 → 复制到 rootfs/bin/busybox             │
	│     └─ 未找到 → downloadBusybox() 从网络下载        │
	│  2. isStaticBinary()  检测是否为静态链接             │
	│     ├─ 静态 → 无需额外操作                          │
	│     └─ 动态 → copySharedLibs() 复制依赖库           │
	│  3. installBusyboxApplets()  创建命令符号链接        │
	│     ├─ 优先: chroot + busybox --install             │
	│     └─ 备选: busybox --list + 手动创建符号链接      │
	└─────────────────────────────────────────────────────┘

=======================================================================
*/
func setupBusybox(rootFSPath string) error {
	binDir := filepath.Join(rootFSPath, "bin")

	busyboxPath, err := findBusybox()
	if err != nil {
		fmt.Printf("  本地未找到 busybox，尝试下载静态版本...\n")
		busyboxPath, err = downloadBusybox(binDir)
		if err != nil {
			return fmt.Errorf("获取 busybox 失败，请安装: apt install busybox-static")
		}
		fmt.Printf("  下载完成: %s\n", busyboxPath)
	} else {
		fmt.Printf("  找到 busybox: %s\n", busyboxPath)
	}

	destBusybox := filepath.Join(binDir, "busybox")
	if err := utils.CopyFile(busyboxPath, destBusybox); err != nil {
		return fmt.Errorf("复制 busybox 失败: %w", err)
	}
	if err := os.Chmod(destBusybox, 0755); err != nil {
		return fmt.Errorf("设置 busybox 权限失败: %w", err)
	}

	isStatic, _ := isStaticBinary(busyboxPath)
	if !isStatic {
		fmt.Printf("  检测到动态链接的 busybox，复制依赖库...\n")
		if err := copySharedLibs(busyboxPath, rootFSPath); err != nil {
			fmt.Printf("  警告: 复制依赖库失败（容器可能无法启动）: %v\n", err)
		}
	}

	fmt.Printf("  安装 busybox 命令符号链接...\n")
	count, err := installBusyboxApplets(rootFSPath, busyboxPath, binDir)
	if err != nil {
		return fmt.Errorf("安装 busybox 符号链接失败: %w", err)
	}
	fmt.Printf("  已安装 %d 个命令 (sh, ls, cat, ps, ip, ping, mount...)\n", count)

	return nil
}

/*
=======================================================================

	findBusybox —— 在宿主机文件系统中查找 busybox 可执行文件

	搜索策略：优先查找静态链接版本（busybox-static），因为静态版本
	不依赖共享库，可以直接在最小化的 rootfs 中运行。

	搜索路径顺序：
	1. /bin/busybox-static       ← Debian/Ubuntu 的静态版本
	2. /usr/bin/busybox-static   ← 某些发行版的静态版本
	3. /bin/busybox              ← 动态链接版本
	4. /usr/bin/busybox          ← 某些发行版的动态版本

	如果所有路径都不存在，返回错误，调用方会触发下载流程。

=======================================================================
*/
func findBusybox() (string, error) {
	for _, path := range busyboxSearchPaths {
		if _, err := os.Stat(path); err == nil {
			return path, nil
		}
	}
	return "", fmt.Errorf("未找到 busybox")
}

/*
=======================================================================

	downloadBusybox —— 从 busybox.net 下载静态编译的 busybox

	下载地址使用的是官方提供的预编译静态版本（musl libc 静态链接），
	这样下载后的二进制文件无需任何依赖库即可运行。

	下载工具选择：
	1. 优先使用 wget（-q 静默模式，-O 指定输出文件）
	2. 备选使用 curl（-f 失败时返回错误码，-S 显示错误，-L 跟随重定向，-o 指定输出）

	下载完成后设置可执行权限（0755）。

=======================================================================
*/
func downloadBusybox(destDir string) (string, error) {
	destPath := filepath.Join(destDir, "busybox")

	var cmd *exec.Cmd
	if _, err := exec.LookPath("wget"); err == nil {
		cmd = exec.Command("wget", "-q", "-O", destPath, busyboxDownloadURL)
	} else if _, err := exec.LookPath("curl"); err == nil {
		cmd = exec.Command("curl", "-fSL", "-o", destPath, busyboxDownloadURL)
	} else {
		return "", fmt.Errorf("未找到 wget 或 curl，请安装其中之一")
	}

	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("下载失败: %w", err)
	}

	if err := os.Chmod(destPath, 0755); err != nil {
		return "", fmt.Errorf("设置权限失败: %w", err)
	}

	return destPath, nil
}

/*
=======================================================================

	isStaticBinary —— 检测二进制文件是否为静态链接

	使用 ldd（List Dynamic Dependencies）命令检测：
	- 静态链接的二进制文件：ldd 会报错或输出
	  "not a dynamic executable" / "statically linked"
	- 动态链接的二进制文件：ldd 会列出所有依赖的共享库

	判断逻辑：
	- ldd 返回错误（退出码非 0）→ 很可能是静态链接
	- 输出包含 "not a dynamic executable" → 确认是静态链接
	- 其他情况 → 动态链接

	示例输出：
	静态: "not a dynamic executable"
	动态: "linux-vdso.so.1 =>  (0x00007ffc...)"
	      "libc.so.6 => /lib/x86_64-linux-gnu/libc.so.6 (0x00007f...)"

=======================================================================
*/
func isStaticBinary(binaryPath string) (bool, error) {
	cmd := exec.Command("ldd", binaryPath)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return true, nil
	}
	return strings.Contains(string(output), "not a dynamic executable"), nil
}

/*
=======================================================================

	copySharedLibs —— 将二进制文件依赖的共享库复制到 rootfs 中

	当 busybox 是动态链接版本时，容器内运行需要所有依赖的 .so 文件。
	本函数通过 ldd 获取依赖列表，然后逐个复制到 rootfs 对应位置。

	ldd 输出格式解析：
	格式1: libname.so => /path/to/lib.so (0xaddr)   ← 常见格式
	格式2: /lib64/ld-linux-x86-64.so.2 (0xaddr)     ← 动态链接器
	格式3: linux-vdso.so.1 =>  (0xaddr)              ← 内核虚拟DSO（无需复制）

	解析策略：
	- 包含 "=> " 的行：提取 => 右侧的路径（到空格为止）
	- 以 "/" 开头的行：直接提取路径（动态链接器，如 ld-linux）
	- 其他行（如 vdso）：跳过（内核提供的虚拟库，不存在于文件系统）

=======================================================================
*/
func copySharedLibs(binaryPath string, rootFSPath string) error {
	cmd := exec.Command("ldd", binaryPath)
	output, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("ldd 失败: %w", err)
	}

	lines := strings.Split(string(output), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)

		var libPath string
		if idx := strings.Index(line, "=> "); idx != -1 {
			libPath = strings.TrimSpace(line[idx+3:])
			if spaceIdx := strings.Index(libPath, " "); spaceIdx != -1 {
				libPath = libPath[:spaceIdx]
			}
		} else if strings.HasPrefix(line, "/") {
			libPath = strings.TrimSpace(line)
			if spaceIdx := strings.Index(libPath, " "); spaceIdx != -1 {
				libPath = libPath[:spaceIdx]
			}
		}

		if libPath == "" {
			continue
		}

		copyLibToRootFS(libPath, rootFSPath)
	}

	return nil
}

/*
=======================================================================

	copyLibToRootFS —— 将单个共享库文件复制到 rootfs 中

	共享库在文件系统中可能存在符号链接，例如：
	/lib/x86_64-linux-gnu/libc.so.6 → libc-2.31.so

	为了保证容器内能正确找到库，需要同时处理：
	1. 复制真实文件（通过 EvalSymlinks 解析后的路径）
	2. 如果原始路径是符号链接，在 rootfs 中也创建对应的符号链接

	示例：
	宿主机: /lib/x86_64-linux-gnu/libc.so.6 → libc-2.31.so

	rootfs 中生成:
	/var/lib/mini-docker/images/myos/rootfs/lib/x86_64-linux-gnu/libc-2.31.so  ← 真实文件
	/var/lib/mini-docker/images/myos/rootfs/lib/x86_64-linux-gnu/libc.so.6      ← 符号链接 → libc-2.31.so

=======================================================================
*/
func copyLibToRootFS(libPath string, rootFSPath string) {
	realPath, err := filepath.EvalSymlinks(libPath)
	if err != nil {
		return
	}

	realDestDir := filepath.Join(rootFSPath, filepath.Dir(realPath))
	os.MkdirAll(realDestDir, 0755)
	utils.CopyFile(realPath, filepath.Join(rootFSPath, realPath))

	if realPath != libPath {
		linkDir := filepath.Join(rootFSPath, filepath.Dir(libPath))
		os.MkdirAll(linkDir, 0755)
		linkDest := filepath.Join(rootFSPath, libPath)
		os.Remove(linkDest)
		rel, err := filepath.Rel(filepath.Dir(libPath), realPath)
		if err != nil {
			rel = realPath
		}
		os.Symlink(rel, linkDest)
	}
}

/*
=======================================================================

	installBusyboxApplets —— 为 busybox 的所有 applet 创建符号链接

	两种安装策略（优先使用第一种）：

	策略1: chroot + busybox --install -s /bin
	────────────────────────────────────────
	在 rootfs 的 chroot 环境中执行 busybox 自带的安装命令。
	这是最可靠的方式，因为 busybox --install 会根据当前
	busybox 编译时包含的 applet 列表来创建符号链接。

	chroot 的含义：将 rootfs 目录作为新的根文件系统，
	这样 busybox --install 创建的路径就是容器内的正确路径。

	策略2: busybox --list + 手动创建符号链接
	────────────────────────────────────────
	如果 chroot 执行失败（例如在非 root 权限下运行），
	则退而求其次：在宿主机上执行 "busybox --list" 获取
	所有 applet 名称，然后逐个创建符号链接。

	符号链接的原理：
	/bin/sh   → busybox    （当执行 sh 时，busybox 检测到 argv[0]="sh"）
	/bin/ls   → busybox    （当执行 ls 时，busybox 检测到 argv[0]="ls"）
	/bin/cat  → busybox    （当执行 cat 时，busybox 检测到 argv[0]="cat"）
	...

=======================================================================
*/
func installBusyboxApplets(rootFSPath string, busyboxPath string, binDir string) (int, error) {
	if err := exec.Command("chroot", rootFSPath, "/bin/busybox", "--install", "-s", "/bin").Run(); err == nil {
		return countApplets(binDir), nil
	}

	return installBusyboxSymlinks(busyboxPath, binDir)
}

/*
=======================================================================

	installBusyboxSymlinks —— 手动为 busybox 的每个 applet 创建符号链接

	流程：
	1. 执行 "busybox --list" 获取所有 applet 名称列表
	   输出示例: "sh\nls\ncat\necho\nmkdir\nmount\n..."
	2. 逐个处理每个 applet：
	   - 跳过已存在的文件（避免覆盖 busybox 本身或其他已有命令）
	   - 创建符号链接: /bin/<applet> → busybox

	注意：符号链接的目标是相对路径 "busybox"（而非绝对路径），
	这样无论 rootfs 被挂载到哪里，链接都能正确解析。

=======================================================================
*/
func installBusyboxSymlinks(busyboxPath string, binDir string) (int, error) {
	cmd := exec.Command(busyboxPath, "--list")
	output, err := cmd.Output()
	if err != nil {
		return 0, fmt.Errorf("获取 busybox 命令列表失败: %w", err)
	}

	applets := strings.Split(strings.TrimSpace(string(output)), "\n")
	count := 0
	for _, applet := range applets {
		applet = strings.TrimSpace(applet)
		if applet == "" {
			continue
		}
		linkPath := filepath.Join(binDir, applet)
		if _, err := os.Lstat(linkPath); err == nil {
			continue
		}
		if err := os.Symlink("busybox", linkPath); err != nil {
			continue
		}
		count++
	}

	return count, nil
}

/*
=======================================================================

	countApplets —— 统计 rootfs/bin/ 中已安装的 applet 数量

	通过遍历 bin 目录中的文件来统计（排除 busybox 本身），
	用于在安装完成后报告安装了多少个命令。

=======================================================================
*/
func countApplets(binDir string) int {
	entries, err := os.ReadDir(binDir)
	if err != nil {
		return 0
	}
	count := 0
	for _, entry := range entries {
		if entry.Name() != "busybox" {
			count++
		}
	}
	return count
}

/*
=======================================================================

	copyFile —— 通用文件复制工具函数

	将源文件完整复制到目标路径，保留原始文件的权限模式。

	实现方式：
	1. 打开源文件，获取文件信息（含权限）
	2. 创建目标文件（使用源文件的权限模式）
	3. 通过 io.Copy 将内容从源复制到目标

	注意：不使用 os.Rename 是因为源和目标可能跨越不同的文件系统
	（例如从宿主机的 /usr/bin 复制到 rootfs 的 overlay 挂载点）。

=======================================================================
*/
