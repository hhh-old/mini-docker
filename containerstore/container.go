package containerstore

import (
	"encoding/json"
	"fmt"
	"mini-docker/libcontainer/cgroups"
	"mini-docker/network"
	"os"
	"path/filepath"
	"strings"

	"mini-docker/constants"
	"mini-docker/containerd/metadata"
	"mini-docker/types"
)

const (
	containerDataDir = constants.ContainerDataDir
)

// ContainerInfo 容器元数据，类型别名为 metadata.ContainerInfo
// 统一使用 metadata.ContainerInfo 作为唯一权威定义，消除重复结构体和 JSON 桥接
type ContainerInfo = metadata.ContainerInfo

// CreateOverlayDirs 在 snapshots/<id>/ 下创建容器的 OverlayFS 目录
// 对齐 containerd: 容器可写层由 Snapshotter 管理，与镜像层统一管理
//
// Deprecated: 此函数绕过 Snapshotter 直接创建目录，不写入 BoltDB 元数据，
// 导致 GC 和 lowerDirs() 无法感知该快照。请使用 Snapshotter.Prepare() 替代。
// 当前容器创建流程已通过 containerd.Client.PrepareSnapshot() 走 Snapshotter.Prepare()。
// 新目录结构: <root>/snapshots/<id>/fs/（对齐 containerd，替代旧版 <root>/<key>/diff/）
func CreateOverlayDirs(containerID string) (*types.OverlayDirs, error) {
	// 容器可写层放在 snapshots/<container-id>/ 下（对齐 containerd 新目录结构）
	baseDir := filepath.Join(constants.SnapshotterDir, "snapshots", containerID)
	fsDir := filepath.Join(baseDir, "fs")
	mergedDir := filepath.Join(baseDir, "merged")
	upperDir := filepath.Join(baseDir, "upper")
	workDir := filepath.Join(baseDir, "work")

	os.RemoveAll(baseDir)

	for _, dir := range []string{fsDir, mergedDir, upperDir, workDir} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return nil, fmt.Errorf("创建目录 %s 失败: %w", dir, err)
		}
	}

	// 创建 link 文件（对齐 containerd: 快照目录都有 link 文件）
	os.WriteFile(filepath.Join(baseDir, "link"), []byte(containerID[:16]), 0644)

	return &types.OverlayDirs{
		Merged: mergedDir,
		Upper:  upperDir,
		Work:   workDir,
	}, nil
}

// CleanupContainerNetwork 通过重建 NetworkManager 并调用 Disconnect() 来清理网络资源
// 统一使用 NetworkManager.Disconnect() 作为网络清理的唯一实现，避免逻辑重复
func CleanupContainerNetwork(networkName, portMap, containerIP, vethHost string) {
	if networkName == "" && vethHost == "" {
		return
	}
	nm := network.NewManagerFromInfo(networkName, portMap, containerIP, vethHost)
	nm.Disconnect()
}

func CleanupCgroup(cgroupName string) {
	cgroups.RemoveCgroup(cgroupName)
}

// ReadContainerLogs 读取容器日志（对齐 Docker 的 json-log 格式）
func ReadContainerLogs(containerID string) ([]string, error) {
	shimLogPath := filepath.Join(constants.ShimDir, containerID, "container.log")
	data, err := os.ReadFile(shimLogPath)
	if err != nil {
		return nil, fmt.Errorf("读取日志失败: %w", err)
	}

	var lines []string
	for _, line := range strings.Split(string(data), "\n") {
		if line == "" {
			continue
		}
		var entry map[string]string
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			lines = append(lines, line)
			continue
		}
		if logMsg, ok := entry["log"]; ok {
			lines = append(lines, logMsg)
		}
	}

	return lines, nil
}
