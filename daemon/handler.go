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
	"strings"
	"sync"
	"syscall"
	"time"

	"mini-docker/builder"
	"mini-docker/constants"
	"mini-docker/containerd"
	"mini-docker/containerd/content"
	"mini-docker/containerd/diff"
	"mini-docker/containerd/diff/walking"
	containerdEvents "mini-docker/containerd/events"
	"mini-docker/containerd/images"
	"mini-docker/containerd/metadata"
	"mini-docker/containerd/snapshots"
	"mini-docker/containerstore"
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
	if info.Tty {
		req.Args["tty"] = "true"
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
	if imageName == "" {
		return Response{Success: false, Message: "需要指定镜像名"}
	}

	var cmd []string
	if cmdStr != "" {
		if cmdJSON := req.Args["cmd_json"]; cmdJSON != "" {
			if err := json.Unmarshal([]byte(cmdJSON), &cmd); err != nil {
				log.Printf("警告: cmd_json 解析失败 (%v)，回退到 cmd 字段", err)
				cmd = strings.Fields(cmdStr)
			}
		} else {
			cmd = strings.Fields(cmdStr)
		}
	}

	// 对齐 Docker: 如果未指定命令，使用镜像 Config.Cmd 作为默认命令
	// 这正是 Dockerfile 中 CMD 指令存在的意义：docker run myimage 无需手动指定命令
	if len(cmd) == 0 {
		imageInfo, err := d.service.InspectImage(imageName)
		if err == nil && imageInfo != nil && len(imageInfo.Config.Cmd) > 0 {
			cmd = imageInfo.Config.Cmd
			log.Printf("  使用镜像默认命令: %s\n", strings.Join(cmd, " "))
		} else {
			return Response{Success: false, Message: "需要指定要执行的命令（镜像未定义 CMD）"}
		}
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

	// 解析镜像引用，获取最顶层快照 ID 作为 PrepareSnapshot 的 parent
	topLayerSnapshotID, err := d.service.ResolveImage(imageName)
	if err != nil {
		return Response{Success: false, Message: fmt.Sprintf("镜像 %s 不存在，请先使用 mini-docker pull 拉取: %v", imageName, err)}
	}
	if topLayerSnapshotID == "" {
		return Response{Success: false, Message: fmt.Sprintf("镜像 %s 的 snapshot ID 为空", imageName)}
	}

	containerID := existingID
	if containerID == "" {
		containerID = utils.GenerateContainerID()
	}
	if name == "" {
		name = containerID
	}

	if existingContainers, err := d.service.ListContainers(); err == nil {
		for _, c := range existingContainers {
			if c.Name == name {
				return Response{Success: false, Message: fmt.Sprintf("容器名 %s 已被使用", name)}
			}
		}
	}

	// 创建容器可写层快照（对齐 containerd: 通过 Snapshotter.Prepare 注册到 boltdb）
	// parent 为镜像最顶层的 TopLayerSnapshotID（cacheID），这样 Snapshotter 可以沿 parent 链递归构建多层 lowerdir
	overlay, err := d.service.PrepareSnapshot(containerID, topLayerSnapshotID)
	if err != nil {
		return Response{Success: false, Message: fmt.Sprintf("创建容器快照失败: %v", err)}
	}

	// 对齐 Docker: 不指定 --network 时自动连接默认 bridge 网络
	if netName == "" {
		netName = network.DefaultNetworkName
	}

	// 先构建 ContainerInfo（持久化的真相来源）
	// 运行时字段（Pid、网络信息等）由 RuntimeState 管理，不写入 ContainerInfo
	containerInfo := &containerstore.ContainerInfo{
		ID:                containerID,
		Name:              name,
		Image:             imageName,
		Cmd:               cmd,
		CreatedAt:         time.Now().Format(constants.TimeFormat),
		RootFS:            overlay.Merged,
		RestartPolicy:     restartPolicy,
		MaxRestartRetries: maxRestartRetries,
		Volumes:           volumes,
		Network:           netName,
		PortMap:           portMap,
		Tty:               tty,
		HealthCmd:         req.Args["health_cmd"],
		HealthInterval:    req.Args["health_interval"],
		HealthTimeout:     req.Args["health_timeout"],
		HealthRetries:     healthRetries,
		Memory:            memory,
		CPUShares:         cpuShares,
		Runtime:           "runc",
	}

	// 容器静态元数据只持久化一次，且必须在 CreateTask 之前
	// 对齐真实 containerd: Container 元数据先落盘，Task.Create 再通过 containerID 查询
	if err := d.service.CreateContainer(containerInfo); err != nil {
		d.cleanupContainerResources(containerID, &ContainerLive{Info: containerInfo}, CleanupOptions{CleanupOverlay: true})
		return Response{Success: false, Message: fmt.Sprintf("保存容器信息失败: %v", err)}
	}

	shimPID, err := d.service.CreateTask(containerID, constants.CgroupPrefix+containerID)
	if err != nil {
		d.cleanupContainerResources(containerID, &ContainerLive{Info: containerInfo}, CleanupOptions{RemoveInfo: true, CleanupOverlay: true})
		return Response{Success: false, Message: fmt.Sprintf("创建任务失败: %v", err)}
	}

	// 等待容器创建完成（对齐 Docker: runc create 返回后才设置网络）
	containerPid, err := d.service.WaitForCreate(containerID, 15*time.Second)
	if err != nil {
		d.cleanupContainerResources(containerID, &ContainerLive{Info: containerInfo}, CleanupOptions{DeleteTask: true, RemoveInfo: true, CleanupOverlay: true})
		return Response{Success: false, Message: fmt.Sprintf("等待容器创建失败: %v", err)}
	}
	// 在 create 和 start 之间设置网络（对齐 Docker: runc create → 设置网络 → runc start）
	// 为什么在 Daemon 中设置网络是正确的?
	//		原因 							说明
	//网络是宿主机资源 	veth pair、bridge、iptables 规则都属于宿主机，不属于容器 namespace
	//需要 root 权限 		网络配置需要特权操作，daemon 通常以 root 运行
	//需要全局视点 		IP 分配、端口映射需要跨容器协调，daemon 有全局状态
	//Docker 的设计 		libnetwork 运行在 dockerd 中，containerd 只负责生命周期

	// Docker 的行为: docker run busybox → 自动连接到 bridge 网络（docker0 网桥）
	// 指定 --network=none 时跳过网络设置（创建独立 netns 但不连接任何网络）
	// 指定 --network=host 时跳过网络设置（共享宿主机网络栈，spec 中已不含 network namespace）
	effectiveNetName := netName
	if effectiveNetName == "none" || effectiveNetName == "host" {
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

	var vethHost, containerIP string
	if netMgr != nil {
		vethHost = netMgr.GetVethHost()
		containerIP = netMgr.GetContainerIP()
	}

	// 运行时状态写入 ContainerRuntimeState（Daemon 自己维护，不放在 ContainerInfo 中）
	runtimeState := &containerstore.ContainerRuntimeState{
		VethHost:      vethHost,
		ContainerIP:   containerIP,
		OverlayMerged: overlay.Merged,
		CgroupName:    constants.CgroupPrefix + containerID,
	}

	// TaskState 仅内存，对齐 containerd: shim 是 source of truth
	taskState := &metadata.TaskState{
		ContainerID: containerID,
		PID:         containerPid,
		ShimPID:     shimPID,
		Status:      containerstore.StatusCreated,
	}

	// 运行时状态持久化到 Daemon 自己的 state 文件
	if err := containerstore.SaveRuntimeState(containerID, runtimeState); err != nil {
		log.Printf("警告: 保存容器 %s 运行时状态失败: %v\n", containerID, err)
	}

	d.RegisterContainer(&ContainerLive{
		Info:      containerInfo,
		Runtime:   runtimeState,
		TaskState: taskState,
		NetMgr:    netMgr,
	})

	go d.WatchContainer(containerID)

	// 对于 -it 模式，先 attach 再 start（确保容器启动后的第一行输出都不会丢失）
	if req.Args["stream"] == "true" && conn != nil {
		shimConn, err := d.service.AttachTask(containerID)
		if err != nil {
			d.UnregisterContainer(containerID)
			d.cleanupContainerResources(containerID, &ContainerLive{Info: containerInfo, Runtime: runtimeState, NetMgr: netMgr}, CleanupOptions{DeleteTask: true, CleanupOverlay: true, RemoveInfo: true})
			return Response{Success: false, Message: fmt.Sprintf("attach 到容器失败: %v", err)}
		}
		// attach 已建立，现在发送 start 信号
		if err := d.service.StartTask(containerID); err != nil {
			log.Printf("启动容器 %s 失败: %v\n", containerID, err)
			shimConn.Close()
			d.UnregisterContainer(containerID)
			d.cleanupContainerResources(containerID, &ContainerLive{Info: containerInfo, Runtime: runtimeState, NetMgr: netMgr}, CleanupOptions{DeleteTask: true, CleanupOverlay: true, RemoveInfo: true})
			return Response{Success: false, Message: fmt.Sprintf("启动容器失败: %v", err)}
		}
		taskState.Status = containerstore.StatusRunning

		// 启动健康检查（stream 模式也需要）
		if containerInfo.HealthCmd != "" {
			go d.runHealthCheckLoop(containerID)
		}

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
	taskState.Status = containerstore.StatusRunning

	// task start 事件已由 containerd task.Service 发布，Daemon 不再重复发布

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

	if _, err := d.service.GetContainer(containerID); err != nil {
		return Response{Success: false, Message: fmt.Sprintf("容器 %s 不存在", containerID)}
	}

	// 获取运行时状态（PID 用于进程存活检查，从 Daemon 本地状态获取）
	var pid int
	if live, ok := d.GetContainerLive(containerID); ok && live.TaskState != nil {
		pid = live.TaskState.PID
	}

	killed, err := utils.GracefulStopProcess(
		func(sig syscall.Signal) error { return d.service.KillTask(containerID, sig) },
		func() bool {
			return utils.CheckProcessAlive(pid)
		},
	)
	if err != nil {
		return Response{Success: false, Message: fmt.Sprintf("停止容器失败: %v", err)}
	}

	exitCode := 0
	if killed {
		exitCode = 128 + int(syscall.SIGKILL)
	}

	// 更新运行时状态（Daemon 自己维护，不放在 ContainerInfo 中）
	var state *ContainerLive
	var runtimeState *containerstore.ContainerRuntimeState
	if s, ok := d.GetContainerLive(containerID); ok {
		state = s
		runtimeState = state.Runtime
	} else {
		runtimeState, _ = containerstore.LoadRuntimeState(containerID)
	}
	if runtimeState == nil {
		runtimeState = &containerstore.ContainerRuntimeState{}
	}
	runtimeState.LastExitCode = exitCode
	runtimeState.LastExitedAt = utils.NowFormatted()
	runtimeState.UserStopped = true
	if err := containerstore.SaveRuntimeState(containerID, runtimeState); err != nil {
		log.Printf("警告: 保存容器 %s 运行时状态失败: %v\n", containerID, err)
	}
	if state != nil {
		state.mu.Lock()
		state.Runtime = runtimeState
		state.mu.Unlock()
	}

	d.UnregisterContainer(containerID)

	d.cleanupContainerResources(containerID, state, CleanupOptions{DeleteTask: true})

	// task delete 事件已由 containerd task.Service 发布，Daemon 不再重复发布

	return Response{Success: true, Message: fmt.Sprintf("容器 %s 已停止", containerID)}
}

func (d *Daemon) handleStart(req Request) Response {
	containerID := req.Args["container_id"]
	if containerID == "" {
		return Response{Success: false, Message: "需要指定容器ID"}
	}

	info, err := d.service.GetContainer(containerID)
	if err != nil {
		return Response{Success: false, Message: fmt.Sprintf("查找容器失败: %v", err)}
	}

	// 检查容器运行时状态（从 shim 获取实时状态）
	taskState, _ := d.service.GetTaskState(containerID)
	status := ""
	if taskState != nil {
		status = taskState.Status
	}
	if status != containerstore.StatusStopped && status != containerstore.StatusCreated {
		return Response{Success: false, Message: fmt.Sprintf("容器状态为 %s，无法启动（仅 stopped/created 可启动）", status)}
	}

	savedInfo := *info

	savedRuntimeState, _ := containerstore.LoadRuntimeState(containerID)
	savedCgroupName := constants.CgroupPrefix + containerID
	if savedRuntimeState != nil && savedRuntimeState.CgroupName != "" {
		savedCgroupName = savedRuntimeState.CgroupName
	}

	newReq := buildRunRequest(info)

	d.cleanupContainerResources(containerID, nil, CleanupOptions{DeleteTask: true, CleanupOverlay: true, RemoveInfo: true})

	resp := d.runWithID(newReq, nil, containerID)
	if resp.Success {
		return resp
	}

	log.Printf("警告: 容器 %s 启动失败，恢复旧容器信息\n", containerID)
	// 恢复静态元数据
	if err := d.service.CreateContainer(&savedInfo); err != nil {
		log.Printf("警告: 恢复容器 %s 静态元数据失败: %v\n", containerID, err)
	}
	// 恢复运行时状态，并重置用户手动停止标记
	if savedRuntimeState == nil {
		savedRuntimeState = &containerstore.ContainerRuntimeState{}
	}
	savedRuntimeState.UserStopped = false
	if err := containerstore.SaveRuntimeState(containerID, savedRuntimeState); err != nil {
		log.Printf("警告: 恢复容器 %s 运行时状态失败: %v\n", containerID, err)
	}
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

	if live, ok := d.GetContainerLive(containerID); ok && live.TaskState != nil {
		live.TaskState.Status = containerstore.StatusPaused
	}

	// task paused 事件已由 containerd task.Service 发布，Daemon 不再重复发布

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

	if live, ok := d.GetContainerLive(containerID); ok && live.TaskState != nil {
		live.TaskState.Status = containerstore.StatusRunning
	}

	// task resumed 事件已由 containerd task.Service 发布，Daemon 不再重复发布

	return Response{Success: true, Message: fmt.Sprintf("容器 %s 已恢复", containerID)}
}

func (d *Daemon) handleRm(req Request) Response {
	containerID := req.Args["container_id"]
	if containerID == "" {
		return Response{Success: false, Message: "需要指定容器ID"}
	}

	// 检查运行时状态（从 shim 获取实时状态）
	state, _ := d.service.GetTaskState(containerID)
	if state != nil && (state.Status == containerstore.StatusRunning || state.Status == containerstore.StatusCreated || state.Status == containerstore.StatusPaused) {
		// 二次验证：检查进程是否真的存活
		if state.PID > 0 && utils.CheckProcessAlive(state.PID) {
			return Response{Success: false, Message: fmt.Sprintf("容器 %s 进程仍在运行 (PID: %d)，请先停止容器", containerID, state.PID)}
		}
		return Response{Success: false, Message: fmt.Sprintf("容器 %s 正在运行，请先停止容器", containerID)}
	}

	if cs, ok := d.GetContainerLive(containerID); ok && cs.Runtime != nil {
		cs.mu.Lock()
		cs.Runtime.UserStopped = true
		cs.mu.Unlock()
	}
	d.UnregisterContainer(containerID)

	d.cleanupContainerResources(containerID, nil, CleanupOptions{DeleteTask: true, CleanupOverlay: true, RemoveInfo: true})

	// container delete 事件已由 containerd containers.Service 发布，Daemon 不再重复发布

	return Response{Success: true, Message: fmt.Sprintf("容器 %s 已删除", containerID)}
}

func (d *Daemon) handlePs(req Request) Response {
	containers, err := d.service.ListContainers()
	if err != nil {
		return Response{Success: false, Message: fmt.Sprintf("列出容器失败: %v", err)}
	}

	// 为每个容器附加运行时状态（优先 Daemon 本地状态，再从 shim 获取实时状态）
	type containerWithStatus struct {
		*containerstore.ContainerInfo
		Status string `json:"status"`
	}
	var result []containerWithStatus
	for _, c := range containers {
		var status string
		if live, ok := d.GetContainerLive(c.ID); ok && live.TaskState != nil {
			status = live.TaskState.Status
		} else if ts, err := d.service.GetTaskState(c.ID); err == nil && ts != nil {
			status = ts.Status
		} else {
			if runtimeState, _ := containerstore.LoadRuntimeState(c.ID); runtimeState != nil && runtimeState.LastExitedAt != "" {
				status = containerstore.StatusStopped
			}
		}
		result = append(result, containerWithStatus{
			ContainerInfo: c,
			Status:        status,
		})
	}

	return Response{Success: true, Data: result}
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

func (d *Daemon) handleEvents(req Request, conn net.Conn) Response {
	stream := req.Args["stream"] != "false"

	var since, until time.Time
	if v := req.Args["since"]; v != "" {
		since, _ = time.Parse(time.RFC3339, v)
	}
	if v := req.Args["until"]; v != "" {
		until, _ = time.Parse(time.RFC3339, v)
	}

	if !stream {
		archive, err := d.service.GetEventArchive(since, until)
		if err != nil {
			return Response{Success: false, Message: fmt.Sprintf("获取事件归档失败: %v", err)}
		}
		var out []map[string]interface{}
		for _, e := range archive {
			out = append(out, eventToMap(e))
		}
		return Response{Success: true, Data: out}
	}

	// 流式模式：建立到 containerd 的事件订阅，转发给 CLI。
	filters := parseEventFilters(req.Args["filters"])
	subConn, err := d.service.SubscribeEvents(filters...)
	if err != nil {
		return Response{Success: false, Message: fmt.Sprintf("订阅事件失败: %v", err)}
	}

	// 先发送握手响应，然后异步转发事件流。
	go func() {
		defer subConn.Close()
		defer conn.Close()

		// 可选：先推送历史归档
		if !since.IsZero() || !until.IsZero() {
			if archive, err := d.service.GetEventArchive(since, until); err == nil {
				for _, e := range archive {
					if err := json.NewEncoder(conn).Encode(eventToMap(e)); err != nil {
						return
					}
				}
			}
		}

		decoder := json.NewDecoder(subConn)
		encoder := json.NewEncoder(conn)
		for {
			select {
			case <-d.shutdown:
				return
			default:
			}

			var ev containerdEvents.Envelope
			if err := decoder.Decode(&ev); err != nil {
				return
			}
			if err := encoder.Encode(eventToMap(&ev)); err != nil {
				return
			}
		}
	}()

	return Response{Stream: true}
}

// parseEventFilters 解析事件过滤参数。
func parseEventFilters(raw string) []string {
	if raw == "" {
		return nil
	}
	var filters []string
	if err := json.Unmarshal([]byte(raw), &filters); err == nil {
		return filters
	}
	return []string{raw}
}

// eventToMap 把事件信封转换为 CLI 可读的 map 格式。
func eventToMap(ev *containerdEvents.Envelope) map[string]interface{} {
	return map[string]interface{}{
		"topic":     ev.Topic,
		"namespace": ev.Namespace,
		"time":      ev.Timestamp.Format(time.RFC3339),
		"event":     ev.Event,
	}
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
	images, err := d.service.ListImages()
	if err != nil {
		return Response{Success: false, Message: fmt.Sprintf("列出镜像失败: %v", err)}
	}
	return Response{Success: true, Data: images}
}

func (d *Daemon) handlePull(req Request, conn net.Conn) Response {
	imageName := req.Args["image"]
	if imageName == "" {
		// 流式模式下，错误也需要通过连接发送
		d.sendProgress(conn, ProgressFrameData{
			Type:    ResultFrame,
			Status:  StatusError,
			Success: false,
			Message: "需要指定镜像名",
		})
		return Response{}
	}

	// 定义从 containerd 接收进度并转发给 CLI 连接的回调
	// 所有帧（包括 result 帧）都通过此回调转发给 CLI
	progressFn := func(frame containerd.ProgressFrameData) {
		// 将进度帧转发为 Daemon 侧的 ProgressFrameData
		// Type/Status 跨包转换：string-based enum 互转
		daemonFrame := ProgressFrameData{
			Type:    FrameType(frame.Type),
			Status:  ProgressFrameStatus(frame.Status),
			Message: frame.Message,
		}
		// result 帧需要传递 Success 标志和 Data
		if frame.Type == containerd.ResultFrame {
			daemonFrame.Success = frame.Status != images.StatusError
			daemonFrame.Data = frame.Data
		}
		d.sendProgress(conn, daemonFrame)
	}

	_, err := d.service.PullImage(imageName, progressFn)
	if err != nil {
		// PullImage 内部出错（比如连接 containerd 失败），发送错误结果帧给 CLI
		d.sendProgress(conn, ProgressFrameData{
			Type:    ResultFrame,
			Status:  StatusError,
			Success: false,
			Message: fmt.Sprintf("拉取镜像失败: %v", err),
		})
	}

	return Response{}
}

func (d *Daemon) handleRmi(req Request) Response {
	imageName := req.Args["image"]
	if imageName == "" {
		return Response{Success: false, Message: "需要指定镜像名"}
	}

	err := d.service.RemoveImage(imageName)
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

// getContentStore 获取 Content Store 实例
// 对齐 containerd: Daemon 通过 RPC 代理访问 Content Store，不再直接创建本地实例
func (d *Daemon) getContentStore() content.Store {
	return d.service.ContentStore()
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
		Service: &containerdBuildService{
			client:       d.service,
			contentStore: d.getContentStore(),
		}, //containerdBuildService 是桥接层，把 builder 的抽象接口映射到 containerd 的具体实现
	}

	result, err := builder.Build(config)
	if err != nil {
		return Response{Success: false, Message: fmt.Sprintf("构建镜像失败: %v", err)}
	}
	return Response{Success: true, Data: result}
}

// handleCommit 将容器的可写层提交为新镜像（对齐 docker commit）
// 对齐 containerd: diff.Differ + Snapshotter.Commit
func (d *Daemon) handleCommit(req Request) Response {
	containerID := req.Args["container_id"]
	imageName := req.Args["image_name"]
	tag := req.Args["tag"]
	if tag == "" {
		tag = "latest"
	}

	if containerID == "" || imageName == "" {
		return Response{Success: false, Message: "需要指定容器ID和镜像名"}
	}

	// 检查容器是否存在
	_, err := d.service.GetContainer(containerID)
	if err != nil {
		return Response{Success: false, Message: fmt.Sprintf("容器 %s 不存在", containerID)}
	}

	// 检查容器运行时状态（优先 Daemon 本地状态，再从 shim 获取实时状态）
	var status string
	if live, ok := d.GetContainerLive(containerID); ok && live.TaskState != nil {
		status = live.TaskState.Status
	} else if ts, err := d.service.GetTaskState(containerID); err == nil && ts != nil {
		status = ts.Status
	}
	if status != containerstore.StatusRunning && status != containerstore.StatusStopped {
		return Response{Success: false, Message: fmt.Sprintf("容器 %s 状态为 %s，无法提交", containerID, status)}
	}

	img, err := d.service.CommitContainer(containerID, imageName, tag)
	if err != nil {
		return Response{Success: false, Message: fmt.Sprintf("提交容器失败: %v", err)}
	}

	return Response{Success: true, Data: img, Message: fmt.Sprintf("容器 %s 已提交为镜像 %s:%s", containerID, imageName, tag)}
}

// containerdBuildService 把 builder.BuildService 桥接到 containerd.Client
// 让 builder 不直接依赖 containerd 进程通信细节
type containerdBuildService struct {
	client       *containerd.Client
	contentStore content.Store
	differ       diff.Differ // 对齐 containerd: 可插拔的层差异计算器，由插件系统注入
}

func (s *containerdBuildService) ResolveImage(imageRef string) (string, error) {
	return s.client.ResolveImage(imageRef)
}

func (s *containerdBuildService) RegisterImage(info *metadata.Image) error {
	return s.client.RegisterImage(info)
}

func (s *containerdBuildService) Snapshotter() snapshots.Snapshotter {
	return s.client.Snapshotter()
}

func (s *containerdBuildService) ContentStore() content.Store {
	return s.contentStore
}

// Differ 返回层差异计算器接口
// 对齐 containerd: diff 服务可插拔，构建器通过此接口计算层差异
func (s *containerdBuildService) Differ() diff.Differ {
	if s.differ != nil {
		return s.differ
	}
	// 降级：如果未注入 Differ，创建默认实例
	return walking.NewLayerDiffer()
}
