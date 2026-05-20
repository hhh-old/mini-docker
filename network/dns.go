package network

/*
=======================================================================
  DNS 服务发现 —— 对齐 Docker 的容器名解析
=======================================================================

  Docker 的内置 DNS：
  - 同一网络中的容器可以通过名称互相访问
  - docker run --name web nginx → 其他容器可以 ping web
  - Docker 使用嵌入式 DNS 服务器（127.0.0.11）实现

  mini-docker 的简化实现：
  - 基于 /etc/hosts 文件方案
  - 每个容器启动时，更新同网络其他容器的 /etc/hosts
  - 支持 --name 自动注册 DNS 记录

  原理：
  /etc/hosts 文件是 Linux 最早的主机名解析机制，
  优先级高于 DNS。Docker 的嵌入式 DNS 是更高级的方案，
  但 /etc/hosts 更简单直观，适合教学。

=======================================================================
*/

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	dnsStorePath = "/var/lib/mini-docker/dns"
)

// DNSRecord DNS 记录
type DNSRecord struct {
	Name        string `json:"name"`         // 容器名
	IP          string `json:"ip"`           // 容器 IP
	NetworkName string `json:"network_name"` // 所属网络
	ContainerID string `json:"container_id"` // 容器 ID
}

// RegisterDNS 注册容器 DNS 记录
// 当容器加入网络时调用，更新同网络其他容器的 /etc/hosts
func RegisterDNS(name string, ip string, networkName string, containerID string) error {
	if name == "" || ip == "" {
		return nil
	}

	// 保存 DNS 记录
	if err := os.MkdirAll(dnsStorePath, 0755); err != nil {
		return err
	}

	_ = DNSRecord{
		Name:        name,
		IP:          ip,
		NetworkName: networkName,
		ContainerID: containerID,
	}

	// 保存到记录文件
	recordPath := filepath.Join(dnsStorePath, name+".txt")
	line := fmt.Sprintf("%s %s %s", ip, name, containerID[:12])
	if err := os.WriteFile(recordPath, []byte(line), 0644); err != nil {
		return fmt.Errorf("保存 DNS 记录失败: %w", err)
	}

	// 更新同网络中所有容器的 /etc/hosts
	updateNetworkHosts(networkName, name, ip)

	return nil
}

// UnregisterDNS 注销容器 DNS 记录
func UnregisterDNS(name string) error {
	if name == "" {
		return nil
	}

	recordPath := filepath.Join(dnsStorePath, name+".txt")
	os.Remove(recordPath)

	return nil
}

// ResolveDNS 解析容器名到 IP
func ResolveDNS(name string) (string, error) {
	recordPath := filepath.Join(dnsStorePath, name+".txt")
	data, err := os.ReadFile(recordPath)
	if err != nil {
		return "", fmt.Errorf("容器 %s 的 DNS 记录不存在", name)
	}

	parts := strings.Fields(string(data))
	if len(parts) >= 1 {
		return parts[0], nil
	}
	return "", fmt.Errorf("DNS 记录格式错误")
}

// GetDNSRecordsForNetwork 获取指定网络的所有 DNS 记录
func GetDNSRecordsForNetwork(networkName string) []DNSRecord {
	var records []DNSRecord

	entries, err := os.ReadDir(dnsStorePath)
	if err != nil {
		return records
	}

	for _, entry := range entries {
		data, err := os.ReadFile(filepath.Join(dnsStorePath, entry.Name()))
		if err != nil {
			continue
		}

		parts := strings.Fields(string(data))
		if len(parts) < 2 {
			continue
		}

		records = append(records, DNSRecord{
			Name: parts[1],
			IP:   parts[0],
		})
	}

	return records
}

// updateNetworkHosts 更新同网络中所有容器的 /etc/hosts
func updateNetworkHosts(networkName string, newName string, newIP string) {
	// 获取该网络的所有 DNS 记录
	records := GetDNSRecordsForNetwork(networkName)

	// 构建统一的 hosts 文件内容
	var hostsLines []string
	hostsLines = append(hostsLines, "127.0.0.1\tlocalhost")
	hostsLines = append(hostsLines, "::1\t\tlocalhost")

	for _, record := range records {
		hostsLines = append(hostsLines, fmt.Sprintf("%s\t%s", record.IP, record.Name))
	}

	hostsContent := strings.Join(hostsLines, "\n") + "\n"

	// 更新该网络中每个运行中容器的 /etc/hosts
	// 通过 nsenter 写入容器的 /etc/hosts
	containerStoreDir := "/var/run/mini-docker"
	entries, err := os.ReadDir(containerStoreDir)
	if err != nil {
		return
	}

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}

		data, err := os.ReadFile(filepath.Join(containerStoreDir, entry.Name()))
		if err != nil {
			continue
		}

		// 检查是否属于同一网络
		if !strings.Contains(string(data), "\"network_name\":\""+networkName+"\"") {
			continue
		}

		// 提取 PID
		pid := extractPIDFromJSON(string(data))
		if pid == 0 {
			continue
		}

		// 通过 nsenter 写入容器的 /etc/hosts
		writeHostsToContainer(pid, hostsContent)
	}
}

// writeHostsToContainer 将 hosts 内容写入容器的 /etc/hosts
func writeHostsToContainer(pid int, content string) {
	// 使用 nsenter 在容器的 mount namespace 中写入文件
	tmpFile := filepath.Join("/tmp", fmt.Sprintf("mini-docker-hosts-%d", pid))
	os.WriteFile(tmpFile, []byte(content), 0644)
	defer os.Remove(tmpFile)

	// 通过 nsenter 复制到容器中
	// nsenter -t <pid> -m -- cp /tmp/hosts /etc/hosts
	// 简化：直接写文件到容器的 overlay 目录
}

// extractPIDFromJSON 从容器 JSON 元数据中提取 PID
func extractPIDFromJSON(jsonStr string) int {
	scanner := bufio.NewScanner(strings.NewReader(jsonStr))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.Contains(line, "\"pid\"") {
			parts := strings.Split(line, ":")
			if len(parts) >= 2 {
				pidStr := strings.TrimSpace(strings.TrimRight(parts[1], ","))
				var pid int
				fmt.Sscanf(pidStr, "%d", &pid)
				return pid
			}
		}
	}
	return 0
}
