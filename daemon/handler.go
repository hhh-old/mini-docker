package daemon

/*
=======================================================================
  Handler —— Daemon 端请求处理器
=======================================================================

  每个 CLI 命令对应一个 handler 方法。
  Handler 调用 container/cgroup/network 包完成实际操作。

  Docker 的处理链：
  CLI → dockerd API → containerd → shim → runc

  mini-docker 的简化链：
  CLI → Daemon handler → container/cgroup/network 包

=======================================================================
*/

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"mini-docker/cgroup"
	"mini-docker/container"
	"mini-docker/image"
	"mini-docker/network"
)

// handleRun 处理 run 请求
func (d *Daemon) handleRun(req Request) Response {
	imageName := req.Args["image"]
	cmdStr := req.Args["cmd"]
	if imageName == "" || cmdStr == "" {
		return Response{Success: false, Message: "需要指定镜像名和命令"}
	}

	tty := req.Args["tty"] == "true"
	detach := req.Args["detach"] == "true"

	// Daemon 无法处理交互模式：没有终端 I/O
	// 交互模式由 CLI 直接运行，不会到达这里
	if tty && !detach {
		return Response{Success: false, Message: "交互模式请直接运行，不要通过 Daemon"}
	}
	memory := req.Args["memory"]
	cpuShares := req.Args["cpu_shares"]
	netName := req.Args["network"]
	portMap := req.Args["port_map"]
	name := req.Args["name"]
	restartPolicy := req.Args["restart_policy"]
	if restartPolicy == "" {
		restartPolicy = "no"
	}

	// 解析 volumes 参数
	var volumes []string
	if volsStr := req.Args["volumes"]; volsStr != "" {
		volumes = strings.Split(volsStr, "|")
	}

	// 构造容器运行配置
	config := container.RunConfig{
		Tty:           tty,
		Detach:        detach,
		Memory:        memory,
		CpuShares:     cpuShares,
		Network:       netName,
		Name:          name,
		PortMap:       portMap,
		Image:         imageName,
		Cmd:           strings.Fields(cmdStr),
		RestartPolicy: restartPolicy,
		Volumes:       volumes,
	}

	// 创建 cgroup 管理器
	cg := &cgroup.CgroupManager{}
	if config.Memory != "" {
		cg.MemoryLimit = config.Memory
	}
	if config.CpuShares != "" {
		cg.CpuShares = config.CpuShares
	}

	// 创建网络管理器
	netMgr := &network.NetworkManager{}
	if config.Network != "" {
		netMgr.NetworkName = config.Network
	}
	if config.PortMap != "" {
		netMgr.PortMap = config.PortMap
	}

	// 运行容器
	err := container.Run(config, cg, netMgr)
	if err != nil {
		return Response{Success: false, Message: fmt.Sprintf("启动容器失败: %v", err)}
	}

	// 获取容器信息（Run 内部已保存）
	containers, _ := container.ListContainers()
	var containerInfo *container.ContainerInfo
	for _, c := range containers {
		if c.Image == imageName && (c.Status == "running") {
			containerInfo = c
			break
		}
	}

	if containerInfo != nil {
		// 注册到 Daemon 管理
		proc, _ := os.FindProcess(containerInfo.Pid)
		state := &ContainerState{
			ID:            containerInfo.ID,
			Pid:           containerInfo.Pid,
			Cmd:           proc,
			CgroupMgr:     cg,
			NetMgr:        netMgr,
			CreatedAt:     time.Now(),
			RestartPolicy: restartPolicy,
		}
		d.RegisterContainer(state)

		// 后台模式下监控容器退出
		if detach {
			go d.WatchContainer(state)
		}
	}

	return Response{
		Success: true,
		Message: fmt.Sprintf("容器启动成功"),
		Data:    containerInfo,
	}
}

// handleStop 处理 stop 请求
func (d *Daemon) handleStop(req Request) Response {
	containerID := req.Args["container_id"]
	if containerID == "" {
		return Response{Success: false, Message: "需要指定容器ID"}
	}

	err := container.Stop(containerID)
	if err != nil {
		return Response{Success: false, Message: fmt.Sprintf("停止容器失败: %v", err)}
	}

	// 从 Daemon 注销
	if state, ok := d.GetContainerState(containerID); ok {
		d.UnregisterContainer(state.ID)
	}

	d.eventBus.Publish(Event{
		Type:      "container_stop",
		Container: containerID,
		Time:      time.Now(),
		Message:   "容器被用户停止",
	})

	return Response{Success: true, Message: fmt.Sprintf("容器 %s 已停止", containerID)}
}

// handleStart 处理 start 请求（重启已停止的容器）
func (d *Daemon) handleStart(req Request) Response {
	containerID := req.Args["container_id"]
	if containerID == "" {
		return Response{Success: false, Message: "需要指定容器ID"}
	}

	err := container.Start(containerID)
	if err != nil {
		return Response{Success: false, Message: fmt.Sprintf("启动容器失败: %v", err)}
	}

	d.eventBus.Publish(Event{
		Type:      "container_start",
		Container: containerID,
		Time:      time.Now(),
		Message:   "容器被用户启动",
	})

	return Response{Success: true, Message: fmt.Sprintf("容器 %s 已启动", containerID)}
}

// handlePause 处理 pause 请求
func (d *Daemon) handlePause(req Request) Response {
	containerID := req.Args["container_id"]
	if containerID == "" {
		return Response{Success: false, Message: "需要指定容器ID"}
	}

	cgroupName, err := container.GetContainerCgroupName(containerID)
	if err != nil {
		return Response{Success: false, Message: fmt.Sprintf("查找容器失败: %v", err)}
	}

	cg := &cgroup.CgroupManager{CgroupName: cgroupName}
	err = container.Pause(containerID, cg)
	if err != nil {
		return Response{Success: false, Message: fmt.Sprintf("暂停容器失败: %v", err)}
	}

	d.eventBus.Publish(Event{
		Type:      "container_pause",
		Container: containerID,
		Time:      time.Now(),
	})

	return Response{Success: true, Message: fmt.Sprintf("容器 %s 已暂停", containerID)}
}

// handleUnpause 处理 unpause 请求
func (d *Daemon) handleUnpause(req Request) Response {
	containerID := req.Args["container_id"]
	if containerID == "" {
		return Response{Success: false, Message: "需要指定容器ID"}
	}

	cgroupName, err := container.GetContainerCgroupName(containerID)
	if err != nil {
		return Response{Success: false, Message: fmt.Sprintf("查找容器失败: %v", err)}
	}

	cg := &cgroup.CgroupManager{CgroupName: cgroupName}
	err = container.Unpause(containerID, cg)
	if err != nil {
		return Response{Success: false, Message: fmt.Sprintf("恢复容器失败: %v", err)}
	}

	d.eventBus.Publish(Event{
		Type:      "container_unpause",
		Container: containerID,
		Time:      time.Now(),
	})

	return Response{Success: true, Message: fmt.Sprintf("容器 %s 已恢复", containerID)}
}

// handleRm 处理 rm 请求
func (d *Daemon) handleRm(req Request) Response {
	containerID := req.Args["container_id"]
	if containerID == "" {
		return Response{Success: false, Message: "需要指定容器ID"}
	}

	err := container.Remove(containerID)
	if err != nil {
		return Response{Success: false, Message: fmt.Sprintf("删除容器失败: %v", err)}
	}

	d.eventBus.Publish(Event{
		Type:      "container_rm",
		Container: containerID,
		Time:      time.Now(),
	})

	return Response{Success: true, Message: fmt.Sprintf("容器 %s 已删除", containerID)}
}

// handlePs 处理 ps 请求
func (d *Daemon) handlePs(req Request) Response {
	containers, err := container.ListContainers()
	if err != nil {
		return Response{Success: false, Message: fmt.Sprintf("列出容器失败: %v", err)}
	}

	return Response{Success: true, Data: containers}
}

// handleExec 处理 exec 请求
func (d *Daemon) handleExec(req Request) Response {
	containerID := req.Args["container_id"]
	cmdStr := req.Args["cmd"]
	if containerID == "" || cmdStr == "" {
		return Response{Success: false, Message: "需要指定容器ID和命令"}
	}

	err := container.Exec(containerID, strings.Fields(cmdStr))
	if err != nil {
		return Response{Success: false, Message: fmt.Sprintf("执行命令失败: %v", err)}
	}

	return Response{Success: true}
}

// handleLogs 处理 logs 请求
func (d *Daemon) handleLogs(req Request) Response {
	containerID := req.Args["container_id"]
	if containerID == "" {
		return Response{Success: false, Message: "需要指定容器ID"}
	}

	logData, err := container.ReadContainerLogs(containerID)
	if err != nil {
		return Response{Success: false, Message: fmt.Sprintf("读取日志失败: %v", err)}
	}

	return Response{Success: true, Data: logData}
}

// handleEvents 处理 events 请求（特殊：需要长连接流式推送）
func (d *Daemon) handleEvents(req Request) Response {
	// events 请求使用单独的流式连接处理，不经过标准 request-response 模式
	archive := d.eventBus.GetArchive()
	var events []map[string]interface{}
	for _, e := range archive {
		events = append(events, map[string]interface{}{
			"type":      e.Type,
			"container": e.Container,
			"time":      e.Time.Format(time.RFC3339),
			"message":   e.Message,
		})
	}
	return Response{Success: true, Data: events}
}

// handleImages 处理 images 请求
func (d *Daemon) handleImages(req Request) Response {
	images, err := image.ListImages()
	if err != nil {
		return Response{Success: false, Message: fmt.Sprintf("列出镜像失败: %v", err)}
	}
	return Response{Success: true, Data: images}
}

// handlePull 处理 pull 请求
func (d *Daemon) handlePull(req Request) Response {
	imageName := req.Args["image"]
	if imageName == "" {
		return Response{Success: false, Message: "需要指定镜像名"}
	}

	err := image.Pull(imageName)
	if err != nil {
		return Response{Success: false, Message: fmt.Sprintf("拉取镜像失败: %v", err)}
	}

	return Response{Success: true, Message: fmt.Sprintf("镜像 %s 拉取成功", imageName)}
}

// handleRmi 处理 rmi 请求
func (d *Daemon) handleRmi(req Request) Response {
	imageName := req.Args["image"]
	if imageName == "" {
		return Response{Success: false, Message: "需要指定镜像名"}
	}

	err := image.RemoveImage(imageName)
	if err != nil {
		return Response{Success: false, Message: fmt.Sprintf("删除镜像失败: %v", err)}
	}

	return Response{Success: true, Message: fmt.Sprintf("镜像 %s 已删除", imageName)}
}

// handleNetworkCreate 处理 network create 请求
func (d *Daemon) handleNetworkCreate(req Request) Response {
	name := req.Args["name"]
	if name == "" {
		return Response{Success: false, Message: "需要指定网络名称"}
	}

	netMgr := &network.NetworkManager{}
	err := netMgr.Create(name)
	if err != nil {
		return Response{Success: false, Message: fmt.Sprintf("创建网络失败: %v", err)}
	}

	return Response{Success: true, Message: fmt.Sprintf("网络 %s 创建成功", name)}
}

// handleNetworkList 处理 network list 请求
func (d *Daemon) handleNetworkList(req Request) Response {
	netMgr := &network.NetworkManager{}
	nets, err := netMgr.List()
	if err != nil {
		return Response{Success: false, Message: fmt.Sprintf("列出网络失败: %v", err)}
	}
	return Response{Success: true, Data: nets}
}

// handleNetworkDelete 处理 network delete 请求
func (d *Daemon) handleNetworkDelete(req Request) Response {
	name := req.Args["name"]
	if name == "" {
		return Response{Success: false, Message: "需要指定网络名称"}
	}

	netMgr := &network.NetworkManager{}
	err := netMgr.Delete(name)
	if err != nil {
		return Response{Success: false, Message: fmt.Sprintf("删除网络失败: %v", err)}
	}

	return Response{Success: true, Message: fmt.Sprintf("网络 %s 已删除", name)}
}

// parsePortMap 解析端口映射字符串
func parsePortMap(portMap string) (hostPort int, containerPort int, err error) {
	parts := strings.Split(portMap, ":")
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("端口映射格式错误")
	}
	hostPort, err = strconv.Atoi(parts[0])
	if err != nil {
		return 0, 0, fmt.Errorf("宿主端口无效")
	}
	containerPort, err = strconv.Atoi(parts[1])
	if err != nil {
		return 0, 0, fmt.Errorf("容器端口无效")
	}
	return hostPort, containerPort, nil
}

// saveContainerLog 写入容器日志（对齐 Docker 的 json-log 格式）
func saveContainerLog(containerID string, logLine string, stream string) error {
	shortID := containerID
	if len(shortID) > 12 {
		shortID = shortID[:12]
	}

	logDir := filepath.Join("/var/lib/mini-docker/containers", shortID)
	if err := os.MkdirAll(logDir, 0755); err != nil {
		return err
	}

	logPath := filepath.Join(logDir, shortID+"-json.log")

	entry := map[string]string{
		"log":    logLine,
		"stream": stream,
		"time":   time.Now().Format(time.RFC3339Nano),
	}

	data, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	data = append(data, '\n')

	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer f.Close()

	_, err = f.Write(data)
	return err
}

// readContainerLog 读取容器日志
func readContainerLog(containerID string) ([]string, error) {
	shortID := containerID
	if len(shortID) > 12 {
		shortID = shortID[:12]
	}

	logPath := filepath.Join("/var/lib/mini-docker/containers", shortID, shortID+"-json.log")
	data, err := os.ReadFile(logPath)
	if err != nil {
		return nil, err
	}

	var lines []string
	for _, line := range strings.Split(string(data), "\n") {
		if line == "" {
			continue
		}
		var entry map[string]string
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			continue
		}
		lines = append(lines, entry["log"])
	}

	return lines, nil
}
