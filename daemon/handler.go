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
	"bufio"
	"bytes"
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

	"mini-docker/builder"
	"mini-docker/constants"
	"mini-docker/containerstore"
	"mini-docker/image"
	"mini-docker/libcontainer"
	"mini-docker/libcontainer/cgroups"
	"mini-docker/network"
	"mini-docker/utils"
	"mini-docker/volume"
)

func mustMarshalJSON(v interface{}) string {
	data, _ := json.Marshal(v)
	return string(data)
}

func getBoolArg(args map[string]string, key string) bool {
	return args[key] == "true"
}

func buildRunRequest(info *containerstore.ContainerInfo) Request {
	volumesStr := ""
	if len(info.Volumes) > 0 {
		volumesStr = strings.Join(info.Volumes, "|")
	}

	req := Request{
		Type: "run",
		Args: map[string]string{
			"image":           info.Image,
			"cmd":             strings.Join(info.Cmd, " "),
			"cmd_json":        mustMarshalJSON(info.Cmd),
			"restart_policy":  info.RestartPolicy,
			"detach":          "true",
			"name":            info.Name,
			"network":         info.Network,
			"port_map":        info.PortMap,
			"volumes":         volumesStr,
			"memory":          info.Memory,
			"cpu_shares":      info.CPUShares,
			"health_cmd":      info.HealthCmd,
			"health_interval": info.HealthInterval,
			"health_timeout":  info.HealthTimeout,
		},
	}
	if info.HealthRetries > 0 {
		req.Args["health_retries"] = fmt.Sprintf("%d", info.HealthRetries)
	}
	if info.MaxRestartRetries > 0 {
		req.Args["max_restart_retries"] = fmt.Sprintf("%d", info.MaxRestartRetries)
	}
	return req
}

func relayStream(daemonConn, shimConn net.Conn, streamReady chan struct{}) {
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

func (d *Daemon) handleRun(req Request, conn net.Conn) Response {
	return d.runWithID(req, conn, "")
}

// 容器进程同步：
// ┌─────────────┐          ┌─────────────┐          ┌─────────────┐
// │   Daemon    │          │    Shim     │          │  容器进程    │
// └─────────────┘          └─────────────┘          └─────────────┘
//
//	│                        │                        │
//	│ 1. CreateTask          │                        │
//	│ ──────────────────────→│                        │
//	│                        │                        │
//	│                        │ 2. runtime create      │
//	│                        │ ──────────────────────→│
//	│                        │                        │
//	│                        │ 3. 写入 created 文件    │
//	│                        │    (PID)               │
//	│                        │ ──────┐                │
//	│                        │       │                │
//	│ 4. WaitForCreate       │       │                │
//	│    (轮询 created 文件)  │       │                │
//	│ ──────────────────────→│←──────┘                │
//	│                        │                        │
//	│    返回 PID             │                        │
//	│ ←──────────────────────│                        │
//	│                        │                        │
//	│ 5. 设置网络 (Connect)   │                        │
//	│    ┌─────────────┐     │                        │
//	│    │ veth pair   │     │                        │
//	│    │ IP 分配     │     │                        │
//	│    │ 路由配置    │     │                        │
//	│    └─────────────┘     │                        │
//	│                        │                        │
//	│                        │ ◄─── shim 阻塞在       │
//	│                        │      <-startReady      │
//	│                        │                        │
//	│ 6. StartTask           │                        │
//	│    (发送 "start" 请求)  │                        │
//	│ ──────────────────────→│                        │
//	│                        │                        │
//	│                        │ 7. close(startReady)   │
//	│                        │ ──────┐                │
//	│                        │       │                │
//	│                        │ 8. runtime start       │
//	│                        │ ──────────────────────→│
//	│                        │                        │
//	│                        │                        │ 容器进程启动
func (d *Daemon) runWithID(req Request, conn net.Conn, existingID string) Response {
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

	tty := getBoolArg(req.Args, "tty")

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

	var maxRestartRetries int
	if mrr := req.Args["max_restart_retries"]; mrr != "" {
		fmt.Sscanf(mrr, "%d", &maxRestartRetries)
	}

	rootFSPath := filepath.Join(constants.ImageStoreDir, imageName, "rootfs")
	if _, err := os.Stat(rootFSPath); os.IsNotExist(err) {
		return Response{Success: false, Message: fmt.Sprintf("镜像 %s 不存在，请先使用 mini-docker pull 拉取", imageName)}
	}

	containerID := existingID
	if containerID == "" {
		containerID = utils.GenerateContainerID()
	}
	if name == "" {
		name = containerID
	}

	if existingContainers, err := containerstore.ListContainers(); err == nil {
		for _, c := range existingContainers {
			if c.Name == name && (c.Status == libcontainer.StatusRunning || c.Status == libcontainer.StatusCreated || c.Status == libcontainer.StatusPaused) {
				return Response{Success: false, Message: fmt.Sprintf("容器名 %s 已被使用", name)}
			}
		}
	}

	overlay, err := containerstore.CreateOverlayDirs(containerID)
	if err != nil {
		return Response{Success: false, Message: fmt.Sprintf("创建 OverlayFS 目录失败: %v", err)}
	}

	// 先构建 ContainerInfo（持久化的真相来源）
	// 运行时字段（Pid、网络信息）在后续步骤中补全
	containerInfo := &containerstore.ContainerInfo{
		ID:                containerID,
		Name:              name,
		Image:             imageName,
		Cmd:               cmd,
		Status:            libcontainer.StatusCreated,
		CreatedAt:         time.Now().Format(constants.TimeFormat),
		RootFS:            rootFSPath,
		OverlayMerged:     overlay.Merged,
		OverlayUpper:      overlay.Upper,
		OverlayWork:       overlay.Work,
		RestartPolicy:     restartPolicy,
		MaxRestartRetries: maxRestartRetries,
		Volumes:           volumes,
		Network:           netName,
		PortMap:           portMap,
		CgroupName:        constants.CgroupPrefix + containerID,
		Tty:               tty,
		HealthCmd:         req.Args["health_cmd"],
		HealthInterval:    req.Args["health_interval"],
		HealthTimeout:     req.Args["health_timeout"],
		HealthRetries:     healthRetries,
		Memory:            memory,
		CPUShares:         cpuShares,
	}

	shimPID, err := d.service.CreateTask(containerInfo)
	if err != nil {
		d.cleanupContainerResources(containerID, &ContainerLive{Info: containerInfo}, CleanupOptions{CleanupOverlay: true})
		return Response{Success: false, Message: fmt.Sprintf("创建任务失败: %v", err)}
	}

	// 等待容器创建完成（对齐 Docker: runc create 返回后才设置网络）
	containerPid, err := d.service.WaitForCreate(containerID, 15*time.Second)
	if err != nil {
		d.cleanupContainerResources(containerID, &ContainerLive{Info: containerInfo}, CleanupOptions{DeleteTask: true, CleanupOverlay: true})
		return Response{Success: false, Message: fmt.Sprintf("等待容器创建失败: %v", err)}
	}

	// cgroup 已由 libcontainer 在 runtime create 阶段通过 OCI spec 应用
	// 对齐 Docker: cgroup 在 runc create 时生效，容器进程从第一行代码就受 cgroup 约束

	// 在 create 和 start 之间设置网络（对齐 Docker: runc create → 设置网络 → runc start）
	// 为什么在 Daemon 中设置网络是正确的?
	//		原因 							说明
	//网络是宿主机资源 	veth pair、bridge、iptables 规则都属于宿主机，不属于容器 namespace
	//需要 root 权限 		网络配置需要特权操作，daemon 通常以 root 运行
	//需要全局视点 		IP 分配、端口映射需要跨容器协调，daemon 有全局状态
	//Docker 的设计 		libnetwork 运行在 dockerd 中，containerd 只负责生命周期

	// 对齐 Docker: 不指定 --network 时自动连接默认 bridge 网络
	// Docker 的行为: docker run busybox → 自动连接到 bridge 网络（docker0 网桥）
	// 指定 --network=none 时跳过网络设置（创建独立 netns 但不连接任何网络）
	// 指定 --network=host 时跳过网络设置（共享宿主机网络栈，spec 中已不含 network namespace）
	effectiveNetName := netName
	if effectiveNetName == "" {
		effectiveNetName = network.DefaultNetworkName
	} else if effectiveNetName == "none" || effectiveNetName == "host" {
		effectiveNetName = ""
	}

	var netMgr network.Manager
	if containerPid > 0 && effectiveNetName != "" {
		nm := &network.NetworkManager{NetworkName: effectiveNetName}
		if portMap != "" {
			nm.PortMap = portMap
		}
		if err := nm.Connect(containerPid); err != nil {
			log.Printf("警告: 设置网络失败: %v\n", err)
		} else {
			netMgr = nm
		}
	}

	// 对齐 Docker: --network=none 时启用容器内的 loopback 接口
	// Docker 行为: --network=none 创建独立 netns，但只启用 lo 接口，不创建 veth pair
	// 之前跳过 Connect() 导致 lo 处于 DOWN 状态，容器内本地 socket 通信（如某些数据库）会失败
	if containerPid > 0 && netName == "none" {
		network.EnableLoopback(containerPid)
	}

	// 补全运行时字段
	containerInfo.Pid = containerPid
	// 对齐 Docker: Network 字段保存用户指定的网络模式（host/none/bridge/自定义），
	// 而非 effectiveNetName（内部使用的实际网络名），
	// 这样容器重启时可以正确恢复网络模式
	containerInfo.Network = netName
	if netName == "" {
		containerInfo.Network = network.DefaultNetworkName
	}
	if netMgr != nil {
		containerInfo.VethHost = netMgr.GetVethHost()
		containerInfo.ContainerIP = netMgr.GetContainerIP()
	}

	if err := containerstore.SaveContainerInfo(containerInfo); err != nil {
		d.cleanupContainerResources(containerID, &ContainerLive{Info: containerInfo, NetMgr: netMgr}, CleanupOptions{DeleteTask: true, CleanupOverlay: true})
		return Response{Success: false, Message: fmt.Sprintf("保存容器信息失败: %v", err)}
	}

	d.RegisterContainer(&ContainerLive{
		Info:    containerInfo,
		ShimPID: shimPID,
		NetMgr:  netMgr,
	})

	go d.WatchContainer(containerID)

	// 对于 -it 模式，先 attach 再 start（确保容器启动后的第一行输出都不会丢失）
	if req.Args["stream"] == "true" && conn != nil {
		shimConn, err := d.service.AttachTask(containerID)
		if err != nil {
			d.UnregisterContainer(containerID)
			d.cleanupContainerResources(containerID, &ContainerLive{Info: containerInfo, NetMgr: netMgr}, CleanupOptions{DeleteTask: true, CleanupOverlay: true, RemoveInfo: true})
			return Response{Success: false, Message: fmt.Sprintf("attach 到容器失败: %v", err)}
		}
		// attach 已建立，现在发送 start 信号
		if err := d.service.StartTask(containerID); err != nil {
			log.Printf("启动容器 %s 失败: %v\n", containerID, err)
			shimConn.Close()
			return Response{Success: false, Message: fmt.Sprintf("启动容器失败: %v", err)}
		}
		containerInfo.Status = libcontainer.StatusRunning
		containerstore.SaveContainerInfo(containerInfo)
		streamReady := make(chan struct{})
		go relayStream(conn, shimConn, streamReady)
		return Response{Success: true, Data: containerInfo, Stream: true, StreamReady: streamReady}
	}

	// 非 -it 模式：直接发送 start 信号
	if err := d.service.StartTask(containerID); err != nil {
		d.UnregisterContainer(containerID)
		d.cleanupContainerResources(containerID, &ContainerLive{Info: containerInfo, NetMgr: netMgr}, CleanupOptions{DeleteTask: true, CleanupOverlay: true, RemoveInfo: true})
		return Response{Success: false, Message: fmt.Sprintf("启动容器失败: %v", err)}
	}
	containerInfo.Status = libcontainer.StatusRunning
	containerstore.SaveContainerInfo(containerInfo)

	d.eventBus.Publish(Event{
		Type:      "container_start",
		Container: containerID,
		Time:      time.Now(),
	})

	// 启动健康检查（对齐 Docker: Daemon 周期执行 HEALTHCHECK）
	if containerInfo.HealthCmd != "" {
		go d.runHealthCheckLoop(containerID)
	}

	return Response{
		Success: true,
		Message: fmt.Sprintf("容器启动成功"),
		Data:    containerInfo,
	}
}

func (d *Daemon) handleStop(req Request) Response {
	containerID := req.Args["container_id"]
	if containerID == "" {
		return Response{Success: false, Message: "需要指定容器ID"}
	}

	info, err := containerstore.LoadContainerInfoByID(containerID)
	if err != nil {
		return Response{Success: false, Message: fmt.Sprintf("容器 %s 不存在", containerID)}
	}

	if state, ok := d.GetContainerLive(containerID); ok {
		state.mu.Lock()
		state.UserStopped = true
		state.mu.Unlock()
	}

	killed, err := utils.GracefulStopProcess(
		func(sig syscall.Signal) error { return d.service.KillTask(containerID, sig) },
		func() bool {
			return utils.CheckProcessAlive(info.Pid)
		},
	)
	if err != nil {
		return Response{Success: false, Message: fmt.Sprintf("停止容器失败: %v", err)}
	}

	exitCode := 0
	if killed {
		exitCode = 128 + int(syscall.SIGKILL)
	}
	info.Status = libcontainer.StatusStopped
	info.ExitCode = exitCode
	info.Pid = 0
	info.FinishedAt = utils.NowFormatted()
	containerstore.SaveContainerInfo(info)

	var state *ContainerLive
	if s, ok := d.GetContainerLive(containerID); ok {
		state = s
	}
	d.UnregisterContainer(containerID)

	d.cleanupContainerResources(containerID, state, CleanupOptions{DeleteTask: true})

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

	info, err := containerstore.LoadContainerInfoByID(containerID)
	if err != nil {
		return Response{Success: false, Message: fmt.Sprintf("查找容器失败: %v", err)}
	}

	if info.Status != libcontainer.StatusStopped && info.Status != libcontainer.StatusCreated {
		return Response{Success: false, Message: fmt.Sprintf("容器状态为 %s，无法启动（仅 stopped/created 可启动）", info.Status)}
	}

	savedInfo := *info
	savedOverlayMerged := info.OverlayMerged
	savedOverlayUpper := info.OverlayUpper
	savedOverlayWork := info.OverlayWork
	savedCgroupName := info.CgroupName

	newReq := buildRunRequest(info)

	d.cleanupContainerResources(containerID, nil, CleanupOptions{DeleteTask: true, CleanupOverlay: true, RemoveInfo: true})

	resp := d.runWithID(newReq, nil, containerID)
	if resp.Success {
		return resp
	}

	log.Printf("警告: 容器 %s 启动失败，恢复旧容器信息\n", containerID)
	savedInfo.Status = libcontainer.StatusStopped
	savedInfo.OverlayMerged = savedOverlayMerged
	savedInfo.OverlayUpper = savedOverlayUpper
	savedInfo.OverlayWork = savedOverlayWork
	savedInfo.CgroupName = savedCgroupName
	containerstore.SaveContainerInfo(&savedInfo)
	containerstore.CreateOverlayDirs(containerID)
	if savedCgroupName != "" {
		cgroups.RemoveCgroup(savedCgroupName)
	}
	return resp
}

func (d *Daemon) handlePause(req Request) Response {
	containerID := req.Args["container_id"]
	if containerID == "" {
		return Response{Success: false, Message: "需要指定容器ID"}
	}

	if err := d.service.PauseTask(containerID); err != nil {
		return Response{Success: false, Message: fmt.Sprintf("暂停容器失败: %v", err)}
	}

	if info, err := containerstore.LoadContainerInfoByID(containerID); err == nil {
		info.Status = libcontainer.StatusPaused
		containerstore.SaveContainerInfo(info)
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

	if info, err := containerstore.LoadContainerInfoByID(containerID); err == nil {
		info.Status = libcontainer.StatusRunning
		containerstore.SaveContainerInfo(info)
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
	if state != nil && (state.Status == libcontainer.StatusRunning || state.Status == libcontainer.StatusCreated || state.Status == libcontainer.StatusPaused) {
		return Response{Success: false, Message: fmt.Sprintf("容器 %s 正在运行，请先停止容器", containerID)}
	}

	info, err := containerstore.LoadContainerInfoByID(containerID)
	if err == nil && (info.Status == libcontainer.StatusRunning || info.Status == libcontainer.StatusPaused) && info.Pid > 0 {
		if utils.CheckProcessAlive(info.Pid) {
			return Response{Success: false, Message: fmt.Sprintf("容器 %s 进程仍在运行 (PID: %d)，请先停止容器", containerID, info.Pid)}
		}
	}

	if cs, ok := d.GetContainerLive(containerID); ok {
		cs.mu.Lock()
		cs.UserStopped = true
		cs.mu.Unlock()
	}
	d.UnregisterContainer(containerID)

	d.cleanupContainerResources(containerID, nil, CleanupOptions{DeleteTask: true, CleanupOverlay: true, RemoveInfo: true})

	d.eventBus.Publish(Event{
		Type:      "container_rm",
		Container: containerID,
		Time:      time.Now(),
	})

	return Response{Success: true, Message: fmt.Sprintf("容器 %s 已删除", containerID)}
}

func (d *Daemon) handlePs(req Request) Response {
	containers, err := containerstore.ListContainers()
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

	tty := getBoolArg(req.Args, "tty")

	shimConn, err := d.service.ExecTaskStream(containerID, cmd, tty)
	if err != nil {
		return Response{Success: false, Message: fmt.Sprintf("执行命令失败: %v", err)}
	}

	if !tty {
		shimConn.SetDeadline(time.Now().Add(5 * time.Minute))
		var buf bytes.Buffer
		scanner := bufio.NewScanner(shimConn)
		for scanner.Scan() {
			buf.WriteString(scanner.Text())
			buf.WriteByte('\n')
		}
		shimConn.Close()
		result := strings.TrimRight(buf.String(), "\n")
		return Response{Success: true, Data: result}
	}

	streamReady := make(chan struct{})
	go relayStream(conn, shimConn, streamReady)
	return Response{Success: true, Stream: true, StreamReady: streamReady}
}

func (d *Daemon) handleLogs(req Request) Response {
	containerID := req.Args["container_id"]
	if containerID == "" {
		return Response{Success: false, Message: "需要指定容器ID"}
	}

	logData, err := containerstore.ReadContainerLogs(containerID)
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

	err := network.CreateNetwork(name)
	if err != nil {
		return Response{Success: false, Message: fmt.Sprintf("创建网络失败: %v", err)}
	}

	return Response{Success: true, Message: fmt.Sprintf("网络 %s 创建成功", name)}
}

func (d *Daemon) handleNetworkList(req Request) Response {
	nets, err := network.ListNetworks()
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

	err := network.DeleteNetwork(name)
	if err != nil {
		return Response{Success: false, Message: fmt.Sprintf("删除网络失败: %v", err)}
	}

	return Response{Success: true, Message: fmt.Sprintf("网络 %s 已删除", name)}
}

func (d *Daemon) handleVolumeCreate(req Request) Response {
	name := req.Args["name"]
	if name == "" {
		return Response{Success: false, Message: "需要指定卷名"}
	}

	vol, err := volume.Create(name)
	if err != nil {
		return Response{Success: false, Message: fmt.Sprintf("创建卷失败: %v", err)}
	}

	return Response{Success: true, Data: vol}
}

func (d *Daemon) handleVolumeList(req Request) Response {
	vols, err := volume.List()
	if err != nil {
		return Response{Success: false, Message: fmt.Sprintf("列出卷失败: %v", err)}
	}
	return Response{Success: true, Data: vols}
}

func (d *Daemon) handleVolumeRm(req Request) Response {
	name := req.Args["name"]
	if name == "" {
		return Response{Success: false, Message: "需要指定卷名"}
	}

	if err := volume.Remove(name); err != nil {
		return Response{Success: false, Message: fmt.Sprintf("删除卷失败: %v", err)}
	}

	return Response{Success: true, Message: fmt.Sprintf("卷 %s 已删除", name)}
}

func (d *Daemon) handleVolumeInspect(req Request) Response {
	name := req.Args["name"]
	if name == "" {
		return Response{Success: false, Message: "需要指定卷名"}
	}

	vol, err := volume.Inspect(name)
	if err != nil {
		return Response{Success: false, Message: fmt.Sprintf("查询卷失败: %v", err)}
	}

	return Response{Success: true, Data: vol}
}

func (d *Daemon) handleBuild(req Request) Response {
	dockerfilePath := req.Args["dockerfile"]
	contextDir := req.Args["context"]
	tag := req.Args["tag"]
	if dockerfilePath == "" && contextDir == "" {
		return Response{Success: false, Message: "需要指定 Dockerfile 路径或构建上下文"}
	}

	config := builder.BuildConfig{
		DockerfilePath: dockerfilePath,
		ContextDir:     contextDir,
		Tag:            tag,
	}

	result, err := builder.Build(config)
	if err != nil {
		return Response{Success: false, Message: fmt.Sprintf("构建镜像失败: %v", err)}
	}

	return Response{Success: true, Data: result}
}
