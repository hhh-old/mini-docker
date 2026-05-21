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
	"syscall"
	"time"

	"mini-docker/cgroup"
	"mini-docker/container"
	"mini-docker/containerd"
	"mini-docker/image"
	"mini-docker/network"
)

// handleRun 处理 run 请求（创建并运行容器）
func (d *Daemon) handleRun(req Request) Response {
	imageName := req.Args["image"]
	cmdStr := req.Args["cmd"]
	if imageName == "" || cmdStr == "" {
		return Response{Success: false, Message: "需要指定镜像名和命令"}
	}

	tty := req.Args["tty"] == "true"
	detach := req.Args["detach"] == "true"

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

	var volumes []string
	if volsStr := req.Args["volumes"]; volsStr != "" {
		volumes = strings.Split(volsStr, "|")
	}

	rootFSPath := filepath.Join("/var/lib/mini-docker/images", imageName, "rootfs")
	if _, err := os.Stat(rootFSPath); os.IsNotExist(err) {
		return Response{Success: false, Message: fmt.Sprintf("镜像 %s 不存在，请先使用 mini-docker pull 拉取", imageName)}
	}

	containerID := fmt.Sprintf("%d", time.Now().UnixNano())
	if name == "" {
		name = containerID[:12]
	}

	overlay, err := container.CreateOverlayDirs(containerID)
	if err != nil {
		return Response{Success: false, Message: fmt.Sprintf("创建 OverlayFS 目录失败: %v", err)}
	}

	taskConfig := &containerd.TaskConfig{
		ID:            containerID,
		Image:         imageName,
		Hostname:      name,
		Cmd:           strings.Fields(cmdStr),
		Tty:           tty,
		Memory:        memory,
		CpuShares:     cpuShares,
		Network:       netName,
		PortMap:       portMap,
		RestartPolicy: restartPolicy,
		Volumes:       volumes,
		RootFSPath:    rootFSPath,
		Overlay: &containerd.OverlayConfig{
			Merged: overlay.Merged,
			Upper:  overlay.Upper,
			Work:   overlay.Work,
		},
	}

	shimPID, err := d.service.CreateTask(taskConfig)
	if err != nil {
		return Response{Success: false, Message: fmt.Sprintf("创建任务失败: %v", err)}
	}

	state, _ := d.service.GetTaskState(containerID)
	if state != nil {
		if memory != "" || cpuShares != "" {
			cg := &cgroup.CgroupManager{CgroupName: "mini-docker-" + containerID[:12]}
			if memory != "" {
				cg.MemoryLimit = memory
			}
			if cpuShares != "" {
				cg.CpuShares = cpuShares
			}
			cg.Apply(state.Pid)
		}

		if netName != "" {
			netMgr := &network.NetworkManager{NetworkName: netName}
			if portMap != "" {
				netMgr.PortMap = portMap
			}
			netMgr.Connect(state.Pid)
		}
	}

	containerInfo := &container.ContainerInfo{
		ID:            containerID,
		Name:          name,
		Image:         imageName,
		Cmd:           strings.Fields(cmdStr),
		Status:        "running",
		CreatedAt:     time.Now().Format("2006-01-02 15:04:05"),
		RootFS:        rootFSPath,
		OverlayMerged: overlay.Merged,
		OverlayUpper:  overlay.Upper,
		OverlayWork:   overlay.Work,
		RestartPolicy: restartPolicy,
		Volumes:       volumes,
	}
	if state != nil {
		containerInfo.Pid = state.Pid
	}
	if netName != "" {
		containerInfo.NetworkName = netName
	}
	if portMap != "" {
		containerInfo.PortMap = portMap
	}

	if err := container.SaveContainerInfo(containerInfo); err != nil {
		return Response{Success: false, Message: fmt.Sprintf("保存容器信息失败: %v", err)}
	}

	state2 := &ContainerState{
		ID:            containerID,
		ShimPID:       shimPID,
		CreatedAt:     time.Now(),
		RestartPolicy: restartPolicy,
	}
	if state != nil {
		state2.Pid = state.Pid
	}
	d.RegisterContainer(state2)

	go d.WatchContainer(containerID)

	d.eventBus.Publish(Event{
		Type:      "container_start",
		Container: containerID,
		Time:      time.Now(),
		Message:   fmt.Sprintf("容器已启动 (shim PID: %d)", shimPID),
	})

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

	if err := d.service.KillTask(containerID, syscall.SIGTERM); err != nil {
		return Response{Success: false, Message: fmt.Sprintf("停止容器失败: %v", err)}
	}

	time.Sleep(2 * time.Second)

	state, _ := d.service.GetTaskState(containerID)
	if state != nil && (state.Status == "running" || state.Status == "created") {
		d.service.KillTask(containerID, syscall.SIGKILL)
	}

	if state2, ok := d.GetContainerState(containerID); ok {
		d.UnregisterContainer(state2.ID)
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

	imageName := containerd.GetContainerImage(containerID)
	cmd := containerd.GetContainerCmd(containerID)
	restartPolicy := containerd.GetContainerRestartPolicy(containerID)

	d.service.DeleteTask(containerID)

	newReq := Request{
		Type: "run",
		Args: map[string]string{
			"image":          imageName,
			"cmd":            strings.Join(cmd, " "),
			"restart_policy": restartPolicy,
			"detach":         "true",
		},
	}
	resp := d.handleRun(newReq)
	if !resp.Success {
		return resp
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

	if err := d.service.PauseTask(containerID); err != nil {
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

	if err := d.service.ResumeTask(containerID); err != nil {
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

	state, _ := d.service.GetTaskState(containerID)
	if state != nil && (state.Status == "running" || state.Status == "created") {
		return Response{Success: false, Message: fmt.Sprintf("容器 %s 正在运行，请先停止容器", containerID)}
	}

	if err := d.service.DeleteTask(containerID); err != nil {
		return Response{Success: false, Message: fmt.Sprintf("删除容器失败: %v", err)}
	}

	container.RemoveContainerInfo(containerID)

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

	if err := d.service.ExecTask(containerID, strings.Fields(cmdStr)); err != nil {
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
