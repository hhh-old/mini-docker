package utils

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	"mini-docker/constants"
)

// GenerateContainerID 生成随机的容器 ID（12 字符 hex，48 bit 随机）
func GenerateContainerID() string {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

// CheckProcessAlive 检查进程是否存活
func CheckProcessAlive(pid int) bool {
	process, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return process.Signal(syscall.Signal(0)) == nil
}

// CopyFile 复制文件
func CopyFile(src, dst string) error {
	sourceFile, err := os.Open(src)
	if err != nil {
		return err
	}
	defer sourceFile.Close()

	info, err := sourceFile.Stat()
	if err != nil {
		return err
	}

	destFile, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode())
	if err != nil {
		return err
	}

	_, err = io.Copy(destFile, sourceFile)
	if err != nil {
		destFile.Close()
		return err
	}

	if err := destFile.Sync(); err != nil {
		destFile.Close()
		return err
	}

	return destFile.Close()
}

// ParseMemory 解析内存字符串（如 "256m", "1g", "512k"）为字节数
func ParseMemory(memStr string) (int64, error) {
	memStr = strings.TrimSpace(memStr)
	if memStr == "" {
		return 0, nil
	}

	multiplier := int64(1)
	switch {
	case strings.HasSuffix(memStr, "g") || strings.HasSuffix(memStr, "G"):
		multiplier = 1024 * 1024 * 1024
		memStr = memStr[:len(memStr)-1]
	case strings.HasSuffix(memStr, "m") || strings.HasSuffix(memStr, "M"):
		multiplier = 1024 * 1024
		memStr = memStr[:len(memStr)-1]
	case strings.HasSuffix(memStr, "k") || strings.HasSuffix(memStr, "K"):
		multiplier = 1024
		memStr = memStr[:len(memStr)-1]
	}

	value, err := strconv.ParseInt(memStr, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("无效的内存格式: %s", memStr)
	}

	return value * multiplier, nil
}

// NowFormatted 返回当前时间的格式化字符串
func NowFormatted() string {
	return time.Now().Format(constants.TimeFormat)
}

func GracefulStopProcess(sendSignalFn func(sig syscall.Signal) error, isAliveFn func() bool) (killed bool, err error) {
	if err := sendSignalFn(syscall.SIGTERM); err != nil {
		return false, fmt.Errorf("发送 SIGTERM 失败: %w", err)
	}

	deadline := time.Now().Add(constants.GracefulStopTimeout)
	for time.Now().Before(deadline) {
		if !isAliveFn() {
			return false, nil
		}
		time.Sleep(100 * time.Millisecond)
	}

	if isAliveFn() {
		if err := sendSignalFn(syscall.SIGKILL); err != nil {
			return true, fmt.Errorf("发送 SIGKILL 失败: %w", err)
		}
		for i := 0; i < 50; i++ {
			if !isAliveFn() {
				return true, nil
			}
			time.Sleep(100 * time.Millisecond)
		}
		return true, fmt.Errorf("SIGKILL 后进程仍未退出")
	}

	return false, nil
}

// CleanupPortMapping 清理端口映射的 iptables 规则
func CleanupPortMapping(portMap string, containerIP string) {
	parts := strings.Split(portMap, ":")
	if len(parts) != 2 {
		return
	}
	hostPort := parts[0]
	containerPort := parts[1]

	cmd := exec.Command("iptables", "-t", "nat", "-D", "PREROUTING",
		"-p", "tcp", "--dport", hostPort,
		"-j", "DNAT", "--to-destination", containerIP+":"+containerPort)
	_ = cmd.Run()

	cmd = exec.Command("iptables", "-t", "nat", "-D", "OUTPUT",
		"-p", "tcp", "--dport", hostPort,
		"-j", "DNAT", "--to-destination", containerIP+":"+containerPort)
	_ = cmd.Run()

	cmd = exec.Command("iptables", "-t", "nat", "-D", "POSTROUTING",
		"-p", "tcp", "-d", containerIP, "--dport", containerPort,
		"-j", "MASQUERADE")
	_ = cmd.Run()
}
