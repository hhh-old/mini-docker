//go:build linux

package pty

import (
	"fmt"
	"os"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// PTY 是实现交互式 Shell（如 -it 模式）的核心底层机制
// 核心概念：什么是 PTY（伪终端对）？
// 一个伪终端（PTY）在内核中是一对成对出现的虚拟字符设备：
// Master（主设备）：由管理器（在这里是 shim 进程）持有。
// Slave（从设备）：表现得像一个真实的物理终端（如键盘和显示器，通常路径为 /dev/pts/<数字>）。它会被绑定到容器内运行的 Shell（如 /bin/sh）的输入、输出和错误流上。由容器进程持有该设备。
// 数据流向如下：
// [用户键盘输入] ──> CLI ──> Daemon ──> Shim ──(写入)──> PTY Master ──(内核自动中转)──> PTY Slave ──> [容器内 Shell]
// [用户屏幕输出] <── CLI <── Daemon <── Shim <──(读取)── <── PTY Master <──(内核自动中转) <── PTY Slave <── [容器内 Shell]
type PTY struct {
	Master *os.File
	Slave  *os.File
	Name   string
}

// 分配并建立伪终端对
func Open() (*PTY, error) {
	//打开 /dev/ptmx（伪终端克隆器,/dev/ptmx 是 Linux 的多路复用设备文件。打开它时，内核会在后台自动创建一个全新的 Master 设备，并为它分配一个对应的、处于锁定状态的 Slave 设备
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR, 0)
	if err != nil {
		return nil, fmt.Errorf("打开 ptmx 失败: %w", err)
	}
	//解锁从设备（unlockpt）
	//在 Linux 中，新分配的 Slave 终端默认是锁定的（安全机制，防止未授权访问）。必须在 Master 的文件描述符上执行 ioctl 命令 TIOCSPTLCK（清空锁定标志位）将其解锁[1]，其他进程才能够打开这个 Slave 终端
	if err := unlockpt(master); err != nil {
		master.Close()
		return nil, fmt.Errorf("unlockpt 失败: %w", err)
	}
	//获取从设备文件名（ptsname）
	//通过向 Master 文件描述符发送 TIOCGPTN（Get Pty Number）指令，获取内核分配的 PTY 唯一数字编号（例如 3），并拼接成标准的字符设备路径（如 /dev/pts/3）
	slaveName, err := ptsname(master)
	if err != nil {
		master.Close()
		return nil, fmt.Errorf("获取 ptsname 失败: %w", err)
	}

	slave, err := os.OpenFile(slaveName, os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		master.Close()
		return nil, fmt.Errorf("打开 slave %s 失败: %w", slaveName, err)
	}

	return &PTY{
		Master: master,
		Slave:  slave,
		Name:   slaveName,
	}, nil
}

func (p *PTY) Close() {
	if p.Master != nil {
		p.Master.Close()
	}
	if p.Slave != nil {
		p.Slave.Close()
	}
}

// 动态改变终端窗口大小
// 当你在宿主机上拉伸终端窗口，或者在 Web 终端上改变浏览器大小时，容器内的布局（如 vim、top 等全屏程序）需要相应调整
func (p *PTY) SetWinsize(rows, cols uint16) error {
	if p.Master == nil {
		return fmt.Errorf("master fd 无效")
	}
	ws := struct {
		Rows uint16
		Cols uint16
		X    uint16
		Y    uint16
	}{Rows: rows, Cols: cols, X: 0, Y: 0}
	_, _, errno := syscall.Syscall(
		syscall.SYS_IOCTL,
		p.Master.Fd(),
		syscall.TIOCSWINSZ,
		uintptr(unsafe.Pointer(&ws)),
	)
	if errno != 0 {
		return fmt.Errorf("TIOCSWINSZ 失败: %v", errno)
	}
	return nil
}

func unlockpt(f *os.File) error {
	var u int32
	_, _, errno := syscall.Syscall(
		syscall.SYS_IOCTL,
		f.Fd(),
		unix.TIOCSPTLCK,
		uintptr(unsafe.Pointer(&u)),
	)
	if errno != 0 {
		return fmt.Errorf("TIOCSPTLCK: %v", errno)
	}
	return nil
}

func ptsname(f *os.File) (string, error) {
	var n uint32
	_, _, errno := syscall.Syscall(
		syscall.SYS_IOCTL,
		f.Fd(),
		unix.TIOCGPTN,
		uintptr(unsafe.Pointer(&n)),
	)
	if errno != 0 {
		return "", fmt.Errorf("TIOCGPTN: %v", errno)
	}
	return fmt.Sprintf("/dev/pts/%d", n), nil
}
