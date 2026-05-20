package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"mini-docker/builder"
	"mini-docker/cgroup"
	"mini-docker/container"
	"mini-docker/daemon"
	"mini-docker/network"
	"mini-docker/volume"
)

func main() {
	// 容器 init 进程入口（不变）
	if container.IsInitProcess() {
		container.HandleInit()
		return
	}

	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	switch os.Args[1] {
	case "daemon":
		daemonCommand()
	case "run":
		runCommand()
	case "exec":
		execCommand()
	case "ps":
		psCommand()
	case "stop":
		stopCommand()
	case "start":
		startCommand()
	case "pause":
		pauseCommand()
	case "unpause":
		unpauseCommand()
	case "rm":
		rmCommand()
	case "logs":
		logsCommand()
	case "events":
		eventsCommand()
	case "images":
		imagesCommand()
	case "pull":
		pullCommand()
	case "rmi":
		rmiCommand()
	case "network":
		networkCommand()
	case "volume":
		volumeCommand()
	case "build":
		buildCommand()
	case "help":
		printUsage()
	default:
		fmt.Printf("未知命令: %s\n", os.Args[1])
		printUsage()
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Println("mini-docker - 一个迷你版 Docker，用于学习容器原理")
	fmt.Println()
	fmt.Println("用法:")
	fmt.Println("  mini-docker daemon                          启动守护进程")
	fmt.Println("  mini-docker run      [选项] <镜像> <命令>   创建并运行一个新容器")
	fmt.Println("  mini-docker exec     <容器ID> <命令>        在运行中的容器内执行命令")
	fmt.Println("  mini-docker ps                              列出所有容器")
	fmt.Println("  mini-docker start    <容器ID>               启动一个已停止的容器")
	fmt.Println("  mini-docker stop     <容器ID>               停止一个运行中的容器")
	fmt.Println("  mini-docker pause    <容器ID>               暂停一个运行中的容器")
	fmt.Println("  mini-docker unpause  <容器ID>               恢复一个暂停的容器")
	fmt.Println("  mini-docker rm       <容器ID>               删除一个容器")
	fmt.Println("  mini-docker logs     <容器ID>               查看容器日志")
	fmt.Println("  mini-docker events                         实时监听容器事件")
	fmt.Println("  mini-docker images                          列出所有镜像")
	fmt.Println("  mini-docker pull     <镜像名>               拉取一个镜像")
	fmt.Println("  mini-docker rmi      <镜像名>               删除一个本地镜像")
	fmt.Println("  mini-docker network  <子命令>               管理容器网络")
	fmt.Println("  mini-docker volume   <子命令>               管理数据卷")
	fmt.Println("  mini-docker build    -t <镜像名> <上下文>       构建镜像")
	fmt.Println()
	fmt.Println("run 选项:")
	fmt.Println("  -it                           交互式模式（分配终端）")
	fmt.Println("  -d                            后台运行模式")
	fmt.Println("  -m <内存限制>                  内存限制（如 100m, 1g）")
	fmt.Println("  -c <CPU份额>                   CPU 份额（如 512）")
	fmt.Println("  -n <网络名称>                  加入指定网络")
	fmt.Println("  -p <宿主端口>:<容器端口>        端口映射（如 8080:80）")
	fmt.Println("  --name <容器名>                设置容器名称")
	fmt.Println("  --restart <策略>               重启策略 (no|always|on-failure)")
	fmt.Println("  -v <卷挂载>                     数据卷挂载 (如 /host:/container, myvol:/data)")
	fmt.Println()
	fmt.Println("network 子命令:")
	fmt.Println("  mini-docker network create <名称>         创建网络")
	fmt.Println("  mini-docker network list                   列出网络")
	fmt.Println("  mini-docker network delete <名称>          删除网络")
}

// daemonCommand 启动 Daemon 守护进程
func daemonCommand() {
	d := daemon.NewDaemon()
	if err := d.Start(); err != nil {
		fmt.Printf("启动 Daemon 失败: %v\n", err)
		os.Exit(1)
	}

	// 阻塞等待 Daemon 退出
	select {}
}

// sendRequest 通过 Daemon 执行请求
func sendRequest(req daemon.Request) (*daemon.Response, error) {
	client := daemon.NewClient()
	return client.Send(req)
}

func runCommand() {
	args := os.Args[2:]
	if len(args) < 2 {
		fmt.Println("用法: mini-docker run [选项] <镜像> <命令>")
		os.Exit(1)
	}

	reqArgs := map[string]string{
		"tty":    "false",
		"detach": "false",
	}

	i := 0
	for i < len(args) {
		switch args[i] {
		case "-it":
			reqArgs["tty"] = "true"
			i++
		case "-d":
			reqArgs["detach"] = "true"
			i++
		case "-m":
			if i+1 >= len(args) {
				fmt.Println("错误: -m 需要指定内存限制")
				os.Exit(1)
			}
			reqArgs["memory"] = args[i+1]
			i += 2
		case "-c":
			if i+1 >= len(args) {
				fmt.Println("错误: -c 需要指定 CPU 份额")
				os.Exit(1)
			}
			reqArgs["cpu_shares"] = args[i+1]
			i += 2
		case "-n":
			if i+1 >= len(args) {
				fmt.Println("错误: -n 需要指定网络名称")
				os.Exit(1)
			}
			reqArgs["network"] = args[i+1]
			i += 2
		case "-p":
			if i+1 >= len(args) {
				fmt.Println("错误: -p 需要指定端口映射（如 8080:80）")
				os.Exit(1)
			}
			reqArgs["port_map"] = args[i+1]
			i += 2
		case "--name":
			if i+1 >= len(args) {
				fmt.Println("错误: --name 需要指定容器名称")
				os.Exit(1)
			}
			reqArgs["name"] = args[i+1]
			i += 2
		case "--restart":
			if i+1 >= len(args) {
				fmt.Println("错误: --restart 需要指定策略 (no|always|on-failure)")
				os.Exit(1)
			}
			reqArgs["restart_policy"] = args[i+1]
			i += 2
		case "-v":
			if i+1 >= len(args) {
				fmt.Println("错误: -v 需要指定卷挂载 (如 /host:/container)")
				os.Exit(1)
			}
			// 支持多个 -v 参数，用逗号分隔存储
			if existing, ok := reqArgs["volumes"]; ok {
				reqArgs["volumes"] = existing + "|" + args[i+1]
			} else {
				reqArgs["volumes"] = args[i+1]
			}
			i += 2
		default:
			goto parseDone
		}
	}

parseDone:
	if i >= len(args) {
		fmt.Println("错误: 需要指定镜像名称")
		os.Exit(1)
	}
	reqArgs["image"] = args[i]
	i++
	if i >= len(args) {
		fmt.Println("错误: 需要指定要执行的命令")
		os.Exit(1)
	}
	reqArgs["cmd"] = strings.Join(args[i:], " ")

	// 交互模式（-it）：CLI 直接运行容器
	// 原因：交互容器需要终端 I/O 直连，Daemon 进程没有终端
	// 对齐 Docker：docker run -it 的 stdio 通过 Hijack 协议直连容器进程
	if reqArgs["tty"] == "true" && reqArgs["detach"] != "true" {
		config := container.RunConfig{
			Tty:           true,
			Memory:        reqArgs["memory"],
			CpuShares:     reqArgs["cpu_shares"],
			Network:       reqArgs["network"],
			Name:          reqArgs["name"],
			PortMap:       reqArgs["port_map"],
			Image:         reqArgs["image"],
			Cmd:           strings.Fields(reqArgs["cmd"]),
			RestartPolicy: reqArgs["restart_policy"],
			Volumes:       parseVolumeList(reqArgs["volumes"]),
		}
		if config.RestartPolicy == "" {
			config.RestartPolicy = "no"
		}

		cg := &cgroup.CgroupManager{}
		if config.Memory != "" {
			cg.MemoryLimit = config.Memory
		}
		if config.CpuShares != "" {
			cg.CpuShares = config.CpuShares
		}

		netMgr := &network.NetworkManager{}
		if config.Network != "" {
			netMgr.NetworkName = config.Network
		}
		if config.PortMap != "" {
			netMgr.PortMap = config.PortMap
		}

		if err := container.Run(config, cg, netMgr); err != nil {
			fmt.Printf("运行容器失败: %v\n", err)
			os.Exit(1)
		}
		return
	}

	// 后台模式（-d）：通过 Daemon 运行
	resp, err := sendRequest(daemon.Request{Type: "run", Args: reqArgs})
	if err != nil {
		fmt.Printf("错误: %v\n", err)
		os.Exit(1)
	}
	if !resp.Success {
		fmt.Printf("%s\n", resp.Message)
		os.Exit(1)
	}

	// 显示容器信息
	if resp.Data != nil {
		data, _ := json.MarshalIndent(resp.Data, "", "  ")
		fmt.Println(string(data))
	}
}

// parseVolumeList 解析 volume 参数列表（"|" 分隔）
func parseVolumeList(volsStr string) []string {
	if volsStr == "" {
		return nil
	}
	return strings.Split(volsStr, "|")
}

func execCommand() {
	args := os.Args[2:]
	if len(args) < 2 {
		fmt.Println("用法: mini-docker exec <容器ID> <命令>")
		os.Exit(1)
	}

	// exec 需要终端 I/O 直连容器进程，必须在 CLI 端直接运行
	// 原因：nsenter 启动的进程需要继承 CLI 的终端，Daemon 进程没有终端
	if err := container.Exec(args[0], args[1:]); err != nil {
		fmt.Printf("执行命令失败: %v\n", err)
		os.Exit(1)
	}
}

func psCommand() {
	resp, err := sendRequest(daemon.Request{Type: "ps", Args: map[string]string{}})
	if err != nil {
		fmt.Printf("错误: %v\n", err)
		os.Exit(1)
	}
	if !resp.Success {
		fmt.Printf("列出容器失败: %s\n", resp.Message)
		os.Exit(1)
	}

	// 解析容器列表
	data, _ := json.Marshal(resp.Data)
	var containers []*container.ContainerInfo
	if err := json.Unmarshal(data, &containers); err != nil {
		fmt.Printf("解析容器列表失败: %v\n", err)
		os.Exit(1)
	}

	if len(containers) == 0 {
		fmt.Println("没有容器")
		return
	}

	fmt.Printf("%-20s %-15s %-15s %-12s %-20s\n", "容器ID", "名称", "镜像", "状态", "创建时间")
	for _, c := range containers {
		fmt.Printf("%-20s %-15s %-15s %-12s %-20s\n",
			c.ID[:12], c.Name, c.Image, c.Status, c.CreatedAt)
	}
}

func startCommand() {
	args := os.Args[2:]
	if len(args) < 1 {
		fmt.Println("用法: mini-docker start <容器ID>")
		os.Exit(1)
	}

	resp, err := sendRequest(daemon.Request{
		Type: "start",
		Args: map[string]string{"container_id": args[0]},
	})
	if err != nil {
		fmt.Printf("错误: %v\n", err)
		os.Exit(1)
	}
	if !resp.Success {
		fmt.Printf("启动容器失败: %s\n", resp.Message)
		os.Exit(1)
	}
	fmt.Printf("容器 %s 已启动\n", args[0])
}

func stopCommand() {
	args := os.Args[2:]
	if len(args) < 1 {
		fmt.Println("用法: mini-docker stop <容器ID>")
		os.Exit(1)
	}

	resp, err := sendRequest(daemon.Request{
		Type: "stop",
		Args: map[string]string{"container_id": args[0]},
	})
	if err != nil {
		fmt.Printf("错误: %v\n", err)
		os.Exit(1)
	}
	if !resp.Success {
		fmt.Printf("停止容器失败: %s\n", resp.Message)
		os.Exit(1)
	}
	fmt.Printf("容器 %s 已停止\n", args[0])
}

func pauseCommand() {
	args := os.Args[2:]
	if len(args) < 1 {
		fmt.Println("用法: mini-docker pause <容器ID>")
		os.Exit(1)
	}

	resp, err := sendRequest(daemon.Request{
		Type: "pause",
		Args: map[string]string{"container_id": args[0]},
	})
	if err != nil {
		fmt.Printf("错误: %v\n", err)
		os.Exit(1)
	}
	if !resp.Success {
		fmt.Printf("暂停容器失败: %s\n", resp.Message)
		os.Exit(1)
	}
	fmt.Printf("容器 %s 已暂停\n", args[0])
}

func unpauseCommand() {
	args := os.Args[2:]
	if len(args) < 1 {
		fmt.Println("用法: mini-docker unpause <容器ID>")
		os.Exit(1)
	}

	resp, err := sendRequest(daemon.Request{
		Type: "unpause",
		Args: map[string]string{"container_id": args[0]},
	})
	if err != nil {
		fmt.Printf("错误: %v\n", err)
		os.Exit(1)
	}
	if !resp.Success {
		fmt.Printf("恢复容器失败: %s\n", resp.Message)
		os.Exit(1)
	}
	fmt.Printf("容器 %s 已恢复\n", args[0])
}

func rmCommand() {
	args := os.Args[2:]
	if len(args) < 1 {
		fmt.Println("用法: mini-docker rm <容器ID>")
		os.Exit(1)
	}

	resp, err := sendRequest(daemon.Request{
		Type: "rm",
		Args: map[string]string{"container_id": args[0]},
	})
	if err != nil {
		fmt.Printf("错误: %v\n", err)
		os.Exit(1)
	}
	if !resp.Success {
		fmt.Printf("删除容器失败: %s\n", resp.Message)
		os.Exit(1)
	}
	fmt.Printf("容器 %s 已删除\n", args[0])
}

func logsCommand() {
	args := os.Args[2:]
	if len(args) < 1 {
		fmt.Println("用法: mini-docker logs <容器ID>")
		os.Exit(1)
	}

	resp, err := sendRequest(daemon.Request{
		Type: "logs",
		Args: map[string]string{"container_id": args[0]},
	})
	if err != nil {
		fmt.Printf("错误: %v\n", err)
		os.Exit(1)
	}
	if !resp.Success {
		fmt.Printf("获取日志失败: %s\n", resp.Message)
		os.Exit(1)
	}

	// 打印日志
	if logData, ok := resp.Data.([]string); ok {
		for _, line := range logData {
			fmt.Print(line)
		}
	} else if resp.Data != nil {
		fmt.Printf("%v\n", resp.Data)
	}
}

func eventsCommand() {
	resp, err := sendRequest(daemon.Request{Type: "events", Args: map[string]string{}})
	if err != nil {
		fmt.Printf("错误: %v\n", err)
		os.Exit(1)
	}
	if !resp.Success {
		fmt.Printf("获取事件失败: %s\n", resp.Message)
		os.Exit(1)
	}

	// 打印事件
	if events, ok := resp.Data.([]interface{}); ok {
		for _, e := range events {
			data, _ := json.Marshal(e)
			fmt.Println(string(data))
		}
	}
}

func imagesCommand() {
	resp, err := sendRequest(daemon.Request{Type: "images", Args: map[string]string{}})
	if err != nil {
		fmt.Printf("错误: %v\n", err)
		os.Exit(1)
	}
	if !resp.Success {
		fmt.Printf("列出镜像失败: %s\n", resp.Message)
		os.Exit(1)
	}

	data, _ := json.Marshal(resp.Data)
	type ImageInfo struct {
		Name      string `json:"name"`
		Size      string `json:"size"`
		CreatedAt string `json:"created_at"`
	}
	var images []ImageInfo
	if err := json.Unmarshal(data, &images); err != nil {
		fmt.Printf("解析镜像列表失败: %v\n", err)
		os.Exit(1)
	}

	if len(images) == 0 {
		fmt.Println("没有本地镜像，使用 mini-docker pull <镜像名> 拉取镜像")
		return
	}

	fmt.Printf("%-30s %-15s %-20s\n", "镜像名", "大小", "创建时间")
	for _, img := range images {
		fmt.Printf("%-30s %-15s %-20s\n", img.Name, img.Size, img.CreatedAt)
	}
}

func pullCommand() {
	args := os.Args[2:]
	if len(args) < 1 {
		fmt.Println("用法: mini-docker pull <镜像名>")
		os.Exit(1)
	}

	resp, err := sendRequest(daemon.Request{
		Type: "pull",
		Args: map[string]string{"image": args[0]},
	})
	if err != nil {
		fmt.Printf("错误: %v\n", err)
		os.Exit(1)
	}
	if !resp.Success {
		fmt.Printf("拉取镜像失败: %s\n", resp.Message)
		os.Exit(1)
	}
	fmt.Println(resp.Message)
}

func rmiCommand() {
	args := os.Args[2:]
	if len(args) < 1 {
		fmt.Println("用法: mini-docker rmi <镜像名>")
		os.Exit(1)
	}

	resp, err := sendRequest(daemon.Request{
		Type: "rmi",
		Args: map[string]string{"image": args[0]},
	})
	if err != nil {
		fmt.Printf("错误: %v\n", err)
		os.Exit(1)
	}
	if !resp.Success {
		fmt.Printf("删除镜像失败: %s\n", resp.Message)
		os.Exit(1)
	}
	fmt.Printf("镜像 %s 已删除\n", args[0])
}

func networkCommand() {
	args := os.Args[2:]
	if len(args) < 1 {
		fmt.Println("用法: mini-docker network <create|list|delete> [名称]")
		os.Exit(1)
	}

	switch args[0] {
	case "create":
		if len(args) < 2 {
			fmt.Println("用法: mini-docker network create <名称>")
			os.Exit(1)
		}
		resp, err := sendRequest(daemon.Request{
			Type: "network_create",
			Args: map[string]string{"name": args[1]},
		})
		if err != nil {
			fmt.Printf("错误: %v\n", err)
			os.Exit(1)
		}
		if !resp.Success {
			fmt.Printf("创建网络失败: %s\n", resp.Message)
			os.Exit(1)
		}
		fmt.Printf("网络 %s 创建成功\n", args[1])
	case "list":
		resp, err := sendRequest(daemon.Request{Type: "network_list", Args: map[string]string{}})
		if err != nil {
			fmt.Printf("错误: %v\n", err)
			os.Exit(1)
		}
		if !resp.Success {
			fmt.Printf("列出网络失败: %s\n", resp.Message)
			os.Exit(1)
		}
		data, _ := json.Marshal(resp.Data)
		type NetInfo struct {
			Name      string   `json:"name"`
			Subnet    string   `json:"subnet"`
			Allocated []string `json:"allocated"`
		}
		var nets []NetInfo
		if err := json.Unmarshal(data, &nets); err != nil {
			fmt.Printf("解析网络列表失败: %v\n", err)
			os.Exit(1)
		}
		if len(nets) == 0 {
			fmt.Println("没有自定义网络")
			return
		}
		fmt.Printf("%-20s %-20s %-20s\n", "网络名称", "子网", "已分配IP数")
		for _, n := range nets {
			fmt.Printf("%-20s %-20s %-20d\n", n.Name, n.Subnet, len(n.Allocated))
		}
	case "delete":
		if len(args) < 2 {
			fmt.Println("用法: mini-docker network delete <名称>")
			os.Exit(1)
		}
		resp, err := sendRequest(daemon.Request{
			Type: "network_delete",
			Args: map[string]string{"name": args[1]},
		})
		if err != nil {
			fmt.Printf("错误: %v\n", err)
			os.Exit(1)
		}
		if !resp.Success {
			fmt.Printf("删除网络失败: %s\n", resp.Message)
			os.Exit(1)
		}
		fmt.Printf("网络 %s 已删除\n", args[1])
	default:
		fmt.Printf("未知网络子命令: %s\n", args[0])
		os.Exit(1)
	}
}

func volumeCommand() {
	args := os.Args[2:]
	if len(args) < 1 {
		fmt.Println("用法: mini-docker volume <create|list|rm|inspect> [名称]")
		os.Exit(1)
	}

	switch args[0] {
	case "create":
		if len(args) < 2 {
			fmt.Println("用法: mini-docker volume create <名称>")
			os.Exit(1)
		}
		info, err := volume.Create(args[1])
		if err != nil {
			fmt.Printf("创建卷失败: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("卷 %s 创建成功 (路径: %s)\n", info.Name, info.MountPath)
	case "list":
		volumes, err := volume.List()
		if err != nil {
			fmt.Printf("列出卷失败: %v\n", err)
			os.Exit(1)
		}
		if len(volumes) == 0 {
			fmt.Println("没有数据卷")
			return
		}
		fmt.Printf("%-20s %-20s %-40s %-20s\n", "卷名", "驱动", "挂载路径", "创建时间")
		for _, v := range volumes {
			fmt.Printf("%-20s %-20s %-40s %-20s\n", v.Name, v.Driver, v.MountPath, v.CreatedAt)
		}
	case "rm":
		if len(args) < 2 {
			fmt.Println("用法: mini-docker volume rm <名称>")
			os.Exit(1)
		}
		if err := volume.Remove(args[1]); err != nil {
			fmt.Printf("删除卷失败: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("卷 %s 已删除\n", args[1])
	case "inspect":
		if len(args) < 2 {
			fmt.Println("用法: mini-docker volume inspect <名称>")
			os.Exit(1)
		}
		info, err := volume.Inspect(args[1])
		if err != nil {
			fmt.Printf("查看卷失败: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("卷名:     %s\n", info.Name)
		fmt.Printf("驱动:     %s\n", info.Driver)
		fmt.Printf("挂载路径: %s\n", info.MountPath)
		fmt.Printf("创建时间: %s\n", info.CreatedAt)
	default:
		fmt.Printf("未知卷子命令: %s\n", args[0])
		os.Exit(1)
	}
}

func buildCommand() {
	args := os.Args[2:]
	if len(args) < 1 {
		fmt.Println("用法: mini-docker build -t <镜像名> <上下文目录>")
		os.Exit(1)
	}

	tag := ""
	contextDir := "."

	i := 0
	for i < len(args) {
		switch args[i] {
		case "-t":
			if i+1 >= len(args) {
				fmt.Println("错误: -t 需要指定镜像名")
				os.Exit(1)
			}
			tag = args[i+1]
			i += 2
		default:
			contextDir = args[i]
			i++
		}
	}

	if tag == "" {
		fmt.Println("错误: 需要指定镜像标签 (-t <name:tag>)")
		os.Exit(1)
	}

	config := builder.BuildConfig{
		ContextDir: contextDir,
		Tag:        tag,
	}

	result, err := builder.Build(config)
	if err != nil {
		fmt.Printf("构建失败: %v\n", err)
		os.Exit(1)
	}

	_ = result
}
