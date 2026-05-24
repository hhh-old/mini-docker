package daemon

/*
=======================================================================
  Handler —— Daemon 端请求处理器（对齐 Docker 的 dockerd handler）

  每个 CLI 命令对应一个 handler 方法。

  Docker 的处理链：
  CLI → dockerd API → containerd → shim → runc

  mini-docker 的对齐链路（-it 和 -d 统一）：
  CLI → Daemon handler → containerd → shim → runtime

  -it 模式（对齐 docker run -it）：
  CLI ←→ Daemon(流式) ←→ Shim(attach) ←→ pty master ←→ 容器进程(pty slave)

  -d 模式（对齐 docker run -d）：
  CLI → Daemon(请求/响应) → containerd → shim → runtime

=======================================================================
*/

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"mini-docker/cgroup"
	"mini-docker/constants"
	"mini-docker/container"
	"mini-docker/containerd"
	"mini-docker/image"
	"mini-docker/network"
	"mini-docker/types"
	"mini-docker/utils"
)

func mustMarshalJSON(v interface{}) string {
	data, _ := json.Marshal(v)
	return string(data)
}

func (d *Daemon) handleRun(req Request, conn net.Conn) Response {
	imageName := req.Args["image"]
	cmdStr := req.Args["cmd"]
	if imageName == "" || cmdStr == "" {
		return Response{Success: false, Message: "需要指定镜像名和命令"}
	}

	var cmd []string
	if cmdJSON := req.Args["cmd_json"]; cmdJSON != "" {
		if err := json.Unmarshal([]byte(cmdJSON), &cmd); err != nil {
			log.Printf("警告: cmd_json 解析失败 (%v)，回退到 cmd 字段", err)
			cmd = strings.Fields(cmdStr)
		}
	} else {
		cmd = strings.Fields(cmdStr)
	}

	tty := req.Args["tty"] == "true"

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

	var healthRetries int
	if hr := req.Args["health_retries"]; hr != "" {
		fmt.Sscanf(hr, "%d", &healthRetries)
	}

	rootFSPath := filepath.Join(constants.ImageStoreDir, imageName, "rootfs")
	if _, err := os.Stat(rootFSPath); os.IsNotExist(err) {
		return Response{Success: false, Message: fmt.Sprintf("镜像 %s 不存在，请先使用 mini-docker pull 拉取", imageName)}
	}

	containerID := generateDaemonContainerID()
	if name == "" {
		name = utils.FormatShortID(containerID)
	}

	overlay, err := container.CreateOverlayDirs(containerID)
	if err != nil {
		return Response{Success: false, Message: fmt.Sprintf("创建 OverlayFS 目录失败: %v", err)}
	}

	taskConfig := &containerd.TaskConfig{
		ID:            containerID,
		Image:         imageName,
		Hostname:      name,
		Cmd:           cmd,
		Tty:           tty,
		Memory:        memory,
		CPUShares:     cpuShares,
		Network:       netName,
		PortMap:       portMap,
		RestartPolicy: restartPolicy,
		Volumes:       volumes,
		RootFSPath:    rootFSPath,
		Overlay: &types.OverlayDirs{
			Merged: overlay.Merged,
			Upper:  overlay.Upper,
			Work:   overlay.Work,
		},
	}

	shimPID, err := d.service.CreateTask(taskConfig)
	if err != nil {
		container.CleanupOverlay(&container.ContainerInfo{
			ID:            containerID,
			OverlayMerged: overlay.Merged,
			OverlayUpper:  overlay.Upper,
			OverlayWork:   overlay.Work,
		})
		return Response{Success: false, Message: fmt.Sprintf("创建任务失败: %v", err)}
	}

	var containerPid int
	containerStatus := constants.StatusRunning
	if state, _ := d.service.GetTaskState(containerID); state != nil {
		containerPid = state.Pid
		if state.Status != "" {
			containerStatus = string(state.Status)
		}
	}

	var cgMgr container.CgroupManager
	if containerPid > 0 && (memory != "" || cpuShares != "") {
		cg := &cgroup.CgroupManager{CgroupName: constants.CgroupPrefix + utils.FormatShortID(containerID)}
		if memory != "" {
			cg.MemoryLimit = memory
		}
		if cpuShares != "" {
			cg.CPUShares = cpuShares
		}
		if err := cg.Apply(containerPid); err != nil {
			log.Printf("警告: 设置 cgroup 失败: %v\n", err)
		}
		cgMgr = cg
	}

	var netMgr container.NetworkManager
	if containerPid > 0 && netName != "" {
		nm := &network.NetworkManager{NetworkName: netName}
		if portMap != "" {
			nm.PortMap = portMap
		}
		if err := nm.Connect(containerPid); err != nil {
			log.Printf("警告: 设置网络失败: %v\n", err)
		} else {
			netMgr = nm
		}
	}

	containerInfo := &container.ContainerInfo{
		ID:             containerID,
		Name:           name,
		Image:          imageName,
		Cmd:            cmd,
		Pid:            containerPid,
		Status:         containerStatus,
		CreatedAt:      time.Now().Format(constants.TimeFormat),
		RootFS:         rootFSPath,
		OverlayMerged:  overlay.Merged,
		OverlayUpper:   overlay.Upper,
		OverlayWork:    overlay.Work,
		RestartPolicy:  restartPolicy,
		Volumes:        volumes,
		NetworkName:    netName,
		PortMap:        portMap,
		CgroupName:     constants.CgroupPrefix + utils.FormatShortID(containerID),
		Tty:            tty,
		HealthCmd:      req.Args["health_cmd"],
		HealthInterval: req.Args["health_interval"],
		HealthTimeout:  req.Args["health_timeout"],
		HealthRetries:  healthRetries,
		MemoryLimit:    memory,
		CPUShares:      cpuShares,
	}

	if netMgr != nil {
		containerInfo.VethHost = netMgr.GetVethHost()
		containerInfo.ContainerIP = netMgr.GetContainerIP()
	}

	if err := container.SaveContainerInfo(containerInfo); err != nil {
		if netMgr != nil {
			netMgr.Disconnect()
		}
		if cgMgr != nil {
			cgMgr.Destroy()
		}
		d.service.DeleteTask(containerID)
		container.CleanupOverlay(&container.ContainerInfo{
			ID:            containerID,
			OverlayMerged: overlay.Merged,
			OverlayUpper:  overlay.Upper,
			OverlayWork:   overlay.Work,
		})
		return Response{Success: false, Message: fmt.Sprintf("保存容器信息失败: %v", err)}
	}

	d.RegisterContainer(&ContainerState{
		ID:            containerID,
		Pid:           containerPid,
		ShimPID:       shimPID,
		CreatedAt:     time.Now(),
		RestartPolicy: restartPolicy,
		CgroupMgr:     cgMgr,
		NetMgr:        netMgr,
	})

	go d.WatchContainer(containerID)
	//	区分是否流式返回
	if req.Args["stream"] == "true" && conn != nil {
		shimConn, err := d.service.AttachTask(containerID)
		if err != nil {
			log.Printf("attach 到容器 %s 失败: %v\n", utils.FormatShortID(containerID), err)
		} else {
			streamReady := make(chan struct{})
			go d.handleAttachStream(conn, shimConn, streamReady)
			return Response{Success: true, Data: containerInfo, Stream: true, StreamReady: streamReady}
		}
	}

	return Response{
		Success: true,
		Message: fmt.Sprintf("容器启动成功"),
		Data:    containerInfo,
	}
}

// handleAttachStream 处理 -it 模式的流式 I/O 转发
// 对齐 Docker 的 attach 行为：CLI ←→ Daemon ←→ Shim ←→ pty ←→ 容器进程
func (d *Daemon) handleAttachStream(daemonConn net.Conn, shimConn net.Conn, streamReady chan struct{}) {
	defer daemonConn.Close()
	defer shimConn.Close()

	close(streamReady)

	var once sync.Once
	done := make(chan struct{})

	go func() {
		defer once.Do(func() { close(done) })
		_, _ = io.Copy(shimConn, daemonConn)
	}()
	go func() {
		defer once.Do(func() { close(done) })
		_, _ = io.Copy(daemonConn, shimConn)
	}()

	<-done
}

func (d *Daemon) handleStop(req Request) Response {
	containerID := req.Args["container_id"]
	if containerID == "" {
		return Response{Success: false, Message: "需要指定容器ID"}
	}

	if state, ok := d.GetContainerState(containerID); ok {
		state.mu.Lock()
		state.UserStopped = true
		state.mu.Unlock()
	}

	if err := utils.GracefulStopProcess(
		func(sig syscall.Signal) error { return d.service.KillTask(containerID, sig) },
		func() bool {
			state, _ := d.service.GetTaskState(containerID)
			return state != nil && (state.Status == constants.StatusRunning || state.Status == constants.StatusCreated)
		},
	); err != nil {
		return Response{Success: false, Message: fmt.Sprintf("停止容器失败: %v", err)}
	}

	var netMgr container.NetworkManager
	var cgMgr container.CgroupManager
	if state, ok := d.GetContainerState(containerID); ok {
		netMgr = state.NetMgr
		cgMgr = state.CgroupMgr
	}
	d.UnregisterContainer(containerID)

	d.service.DeleteTask(containerID)

	if netMgr != nil {
		_ = netMgr.Disconnect()
	}
	if cgMgr != nil {
		_ = cgMgr.Destroy()
	}

	info, err := container.LoadContainerInfoByID(containerID)
	if err == nil {
		info.Status = constants.StatusExited
		info.Pid = 0
		info.FinishedAt = utils.NowFormatted()
		container.SaveContainerInfo(info)
	}

	d.eventBus.Publish(Event{
		Type:      "container_stop",
		Container: containerID,
		Time:      time.Now(),
		Message:   "容器被用户停止",
	})

	return Response{Success: true, Message: fmt.Sprintf("容器 %s 已停止", containerID)}
}

func (d *Daemon) handleStart(req Request) Response {
	containerID := req.Args["container_id"]
	if containerID == "" {
		return Response{Success: false, Message: "需要指定容器ID"}
	}

	info, err := container.LoadContainerInfoByID(containerID)
	if err != nil {
		return Response{Success: false, Message: fmt.Sprintf("查找容器失败: %v", err)}
	}

	if info.Status != constants.StatusExited && info.Status != constants.StatusStopped && info.Status != constants.StatusCreated {
		return Response{Success: false, Message: fmt.Sprintf("容器状态为 %s，无法启动（仅 exited/stopped/created 可启动）", info.Status)}
	}

	imageName := info.Image
	cmd := info.Cmd
	restartPolicy := info.RestartPolicy

	d.service.DeleteTask(containerID)

	volumesStr := ""
	if len(info.Volumes) > 0 {
		volumesStr = strings.Join(info.Volumes, "|")
	}

	newReq := Request{
		Type: "run",
		Args: map[string]string{
			"image":           imageName,
			"cmd":             strings.Join(cmd, " "),
			"cmd_json":        mustMarshalJSON(cmd),
			"restart_policy":  restartPolicy,
			"detach":          "true",
			"name":            info.Name,
			"network":         info.NetworkName,
			"port_map":        info.PortMap,
			"volumes":         volumesStr,
			"health_cmd":      info.HealthCmd,
			"health_interval": info.HealthInterval,
			"health_timeout":  info.HealthTimeout,
		},
	}
	if info.HealthRetries > 0 {
		newReq.Args["health_retries"] = fmt.Sprintf("%d", info.HealthRetries)
	}

	backupInfo := *info

	resp := d.handleRun(newReq, nil)
	if !resp.Success {
		backupInfo.Status = constants.StatusExited
		container.SaveContainerInfo(&backupInfo)
		log.Printf("警告: 容器 %s 重启失败，已恢复容器信息\n", utils.FormatShortID(containerID))
		return resp
	}

	container.CleanupContainerNetwork(info)
	container.CleanupOverlay(info)
	container.CleanupCgroup(info.CgroupName)
	container.RemoveContainerInfo(containerID)

	return Response{Success: true, Message: fmt.Sprintf("容器 %s 已启动", info.Name), Data: resp.Data}
}

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

func (d *Daemon) handleRm(req Request) Response {
	containerID := req.Args["container_id"]
	if containerID == "" {
		return Response{Success: false, Message: "需要指定容器ID"}
	}

	state, _ := d.service.GetTaskState(containerID)
	if state != nil && (state.Status == constants.StatusRunning || state.Status == constants.StatusCreated || state.Status == constants.StatusPaused) {
		return Response{Success: false, Message: fmt.Sprintf("容器 %s 正在运行，请先停止容器", containerID)}
	}

	info, err := container.LoadContainerInfoByID(containerID)
	if err == nil && (info.Status == constants.StatusRunning || info.Status == constants.StatusPaused) && info.Pid > 0 {
		if utils.CheckProcessAlive(info.Pid) == nil {
			return Response{Success: false, Message: fmt.Sprintf("容器 %s 进程仍在运行 (PID: %d)，请先停止容器", containerID, info.Pid)}
		}
	}

	if cs, ok := d.GetContainerState(containerID); ok {
		cs.mu.Lock()
		cs.UserStopped = true
		cs.mu.Unlock()
	}
	d.UnregisterContainer(containerID)

	if err := d.service.DeleteTask(containerID); err != nil {
		return Response{Success: false, Message: fmt.Sprintf("删除容器失败: %v", err)}
	}

	if info != nil {
		container.CleanupContainerNetwork(info)
		container.CleanupOverlay(info)
		container.CleanupCgroup(info.CgroupName)
	}

	container.RemoveContainerInfo(containerID)

	d.eventBus.Publish(Event{
		Type:      "container_rm",
		Container: containerID,
		Time:      time.Now(),
	})

	return Response{Success: true, Message: fmt.Sprintf("容器 %s 已删除", containerID)}
}

func (d *Daemon) handlePs(req Request) Response {
	containers, err := container.ListContainers()
	if err != nil {
		return Response{Success: false, Message: fmt.Sprintf("列出容器失败: %v", err)}
	}

	return Response{Success: true, Data: containers}
}

func (d *Daemon) handleExec(req Request, conn net.Conn) Response {
	containerID := req.Args["container_id"]
	cmdStr := req.Args["cmd"]
	if containerID == "" || cmdStr == "" {
		return Response{Success: false, Message: "需要指定容器ID和命令"}
	}

	var cmd []string
	if cmdJSON := req.Args["cmd_json"]; cmdJSON != "" {
		if err := json.Unmarshal([]byte(cmdJSON), &cmd); err != nil {
			cmd = strings.Fields(cmdStr)
		}
	} else {
		cmd = strings.Fields(cmdStr)
	}

	tty := req.Args["tty"] == "true"

	shimConn, err := d.service.ExecTaskStream(containerID, cmd, tty)
	if err != nil {
		return Response{Success: false, Message: fmt.Sprintf("执行命令失败: %v", err)}
	}

	if !tty {
		shimConn.SetDeadline(time.Now().Add(30 * time.Second))
		output, _ := io.ReadAll(shimConn)
		shimConn.Close()
		result := strings.TrimRight(string(output), "\n")
		return Response{Success: true, Data: result}
	}

	streamReady := make(chan struct{})
	go d.handleExecStream(conn, shimConn, streamReady)
	return Response{Success: true, Stream: true, StreamReady: streamReady}
}

func (d *Daemon) handleExecStream(daemonConn net.Conn, shimConn net.Conn, streamReady chan struct{}) {
	defer daemonConn.Close()
	defer shimConn.Close()

	close(streamReady)

	var once sync.Once
	done := make(chan struct{})

	go func() {
		defer once.Do(func() { close(done) })
		_, _ = io.Copy(shimConn, daemonConn)
	}()
	go func() {
		defer once.Do(func() { close(done) })
		_, _ = io.Copy(daemonConn, shimConn)
	}()

	<-done
}

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

func (d *Daemon) handleEvents(req Request) Response {
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

func (d *Daemon) handleResize(req Request) Response {
	containerID := req.Args["container_id"]
	if containerID == "" {
		return Response{Success: false, Message: "需要指定容器ID"}
	}
	var rows, cols uint16
	fmt.Sscanf(req.Args["rows"], "%d", &rows)
	fmt.Sscanf(req.Args["cols"], "%d", &cols)
	if rows == 0 || cols == 0 {
		return Response{Success: false, Message: "无效的窗口大小"}
	}
	if err := d.service.ResizeTask(containerID, rows, cols); err != nil {
		return Response{Success: false, Message: fmt.Sprintf("调整窗口大小失败: %v", err)}
	}
	return Response{Success: true}
}

func (d *Daemon) handleImages(req Request) Response {
	images, err := image.ListImages()
	if err != nil {
		return Response{Success: false, Message: fmt.Sprintf("列出镜像失败: %v", err)}
	}
	return Response{Success: true, Data: images}
}

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

func (d *Daemon) handleNetworkList(req Request) Response {
	netMgr := &network.NetworkManager{}
	nets, err := netMgr.List()
	if err != nil {
		return Response{Success: false, Message: fmt.Sprintf("列出网络失败: %v", err)}
	}
	return Response{Success: true, Data: nets}
}

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

func generateDaemonContainerID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}
