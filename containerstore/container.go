package containerstore

import (
	"encoding/json"
	"fmt"
	"mini-docker/libcontainer/cgroups"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"mini-docker/constants"
	"mini-docker/libcontainer"
	"mini-docker/network"
	"mini-docker/types"
	"mini-docker/utils"
)

const (
	containerStoreDir = constants.ContainerStoreDir
	containerDataDir  = constants.ContainerDataDir
)

// 存储位置 ： /var/run/mini-docker/<containerID>.json
type ContainerInfo struct {
	ID                string   `json:"id"`
	Name              string   `json:"name"`
	Pid               int      `json:"pid"`
	ShimPID           int      `json:"shim_pid"`
	Image             string   `json:"image"`
	Cmd               []string `json:"cmd"`
	Status            string   `json:"status"` // created, running, paused, stopped, dead, restarting
	CreatedAt         string   `json:"created_at"`
	RootFS            string   `json:"rootfs"`
	CgroupName        string   `json:"cgroup_name"`
	Network           string   `json:"network"`
	VethHost          string   `json:"veth_host"`
	ContainerIP       string   `json:"container_ip"`
	PortMap           string   `json:"port_map"`
	OverlayMerged     string   `json:"overlay_merged"`
	OverlayUpper      string   `json:"overlay_upper"`
	OverlayWork       string   `json:"overlay_work"`
	RestartPolicy     string   `json:"restart_policy"`      // no, always, on-failure
	MaxRestartRetries int      `json:"max_restart_retries"` // on-failure 最大重启次数
	Tty               bool     `json:"tty"`
	ExitCode          int      `json:"exit_code"`
	FinishedAt        string   `json:"finished_at"`
	Volumes           []string `json:"volumes"` // 记录容器的 volume 挂载
	HealthCmd         string   `json:"health_cmd"`
	HealthInterval    string   `json:"health_interval"`
	HealthTimeout     string   `json:"health_timeout"`
	HealthRetries     int      `json:"health_retries"`
	Memory            string   `json:"memory"`
	CPUShares         string   `json:"cpu_shares"`
}

func ListContainers() ([]*ContainerInfo, error) {
	if err := os.MkdirAll(containerStoreDir, 0755); err != nil {
		return nil, err
	}

	entries, err := os.ReadDir(containerStoreDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var containers []*ContainerInfo
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}

		infoPath := filepath.Join(containerStoreDir, entry.Name())
		data, err := os.ReadFile(infoPath)
		if err != nil {
			continue
		}

		var c ContainerInfo
		if err := json.Unmarshal(data, &c); err != nil {
			continue
		}

		if c.Status == libcontainer.StatusRunning {
			if !utils.CheckProcessAlive(c.Pid) {
				c.Status = libcontainer.StatusStopped
				_ = SaveContainerInfo(&c)
			}
		}

		containers = append(containers, &c)
	}

	return containers, nil
}

// 宿主机上
func CreateOverlayDirs(containerID string) (*types.OverlayDirs, error) {
	baseDir := filepath.Join(containerDataDir, containerID, "overlay")
	mergedDir := filepath.Join(baseDir, "merged")
	upperDir := filepath.Join(baseDir, "upper")
	workDir := filepath.Join(baseDir, "work")

	os.RemoveAll(baseDir)

	for _, dir := range []string{mergedDir, upperDir, workDir} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return nil, fmt.Errorf("创建目录 %s 失败: %w", dir, err)
		}
	}

	return &types.OverlayDirs{
		Merged: mergedDir,
		Upper:  upperDir,
		Work:   workDir,
	}, nil
}

func getContainerInfoPath(containerID string) string {
	return filepath.Join(containerStoreDir, containerID+".json")
}

func SaveContainerInfo(info *ContainerInfo) (retErr error) {
	if err := os.MkdirAll(containerStoreDir, 0755); err != nil {
		return err
	}

	data, err := json.MarshalIndent(info, "", "  ")
	if err != nil {
		return err
	}

	infoPath := getContainerInfoPath(info.ID)
	tmpFile, err := os.CreateTemp(containerStoreDir, "container-")
	if err != nil {
		return err
	}
	tmpName := tmpFile.Name()

	defer func() {
		if retErr != nil {
			tmpFile.Close()
			os.Remove(tmpName)
		}
	}()

	if _, err := tmpFile.Write(data); err != nil {
		retErr = err
		return retErr
	}
	if err := tmpFile.Close(); err != nil {
		retErr = err
		return retErr
	}

	retErr = os.Rename(tmpName, infoPath)
	return retErr
}

func RemoveContainerInfo(containerID string) error {
	return os.Remove(getContainerInfoPath(containerID))
}

func LoadContainerInfoByID(containerID string) (*ContainerInfo, error) {
	infoPath := getContainerInfoPath(containerID)
	data, err := os.ReadFile(infoPath)
	if err == nil {
		var c ContainerInfo
		if err := json.Unmarshal(data, &c); err == nil {
			return &c, nil
		}
	}

	// 直接路径读取失败，按容器名遍历查找
	entries, err := os.ReadDir(containerStoreDir)
	if err != nil {
		return nil, fmt.Errorf("容器 %s 不存在", containerID)
	}

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}

		fullPath := filepath.Join(containerStoreDir, entry.Name())
		data, err := os.ReadFile(fullPath)
		if err != nil {
			continue
		}

		var c ContainerInfo
		if err := json.Unmarshal(data, &c); err != nil {
			continue
		}

		if c.Name == containerID {
			return &c, nil
		}
	}

	return nil, fmt.Errorf("容器 %s 不存在", containerID)
}

// CleanupContainerNetwork 通过重建 NetworkManager 并调用 Disconnect() 来清理网络资源
// 统一使用 NetworkManager.Disconnect() 作为网络清理的唯一实现，避免逻辑重复
func CleanupContainerNetwork(info *ContainerInfo) {
	if info.Network == "" && info.VethHost == "" {
		return
	}
	nm := network.NewManagerFromInfo(info.Network, info.PortMap, info.ContainerIP, info.VethHost)
	nm.Disconnect()
}

func CleanupOverlay(info *ContainerInfo) {
	if info.OverlayMerged == "" {
		return
	}

	exec.Command("umount", info.OverlayMerged).Run()

	containerDir := filepath.Join(containerDataDir, info.ID)
	os.RemoveAll(containerDir)
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
