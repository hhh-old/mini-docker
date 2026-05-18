package main

import (
	"fmt"
	"os"

	"mini-docker/cgroup"
	"mini-docker/container"
	"mini-docker/image"
	"mini-docker/network"
)

func main() {
	if container.IsInitProcess() {
		//容器启动
		container.HandleInit()
		return
	}

	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	switch os.Args[1] {
	case "run":
		runCommand()
	case "exec":
		execCommand()
	case "ps":
		psCommand()
	case "stop":
		stopCommand()
	case "pause":
		pauseCommand()
	case "unpause":
		unpauseCommand()
	case "rm":
		rmCommand()
	case "images":
		imagesCommand()
	case "pull":
		pullCommand()
	case "rmi":
		rmiCommand()
	case "network":
		networkCommand()
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
	fmt.Println("  mini-docker run      [选项] <镜像> <命令>   创建并运行一个新容器")
	fmt.Println("  mini-docker exec     <容器ID> <命令>        在运行中的容器内执行命令")
	fmt.Println("  mini-docker ps                              列出所有容器")
	fmt.Println("  mini-docker stop     <容器ID>               停止一个运行中的容器")
	fmt.Println("  mini-docker pause    <容器ID>               暂停一个运行中的容器")
	fmt.Println("  mini-docker unpause  <容器ID>               恢复一个暂停的容器")
	fmt.Println("  mini-docker rm       <容器ID>               删除一个容器")
	fmt.Println("  mini-docker images                          列出所有镜像")
	fmt.Println("  mini-docker pull     <镜像名>               拉取一个镜像（自动构建 rootfs）")
	fmt.Println("  mini-docker rmi      <镜像名>               删除一个本地镜像")
	fmt.Println("  mini-docker network  <子命令>               管理容器网络")
	fmt.Println()
	fmt.Println("run 选项:")
	fmt.Println("  -it                           交互式模式（分配终端）")
	fmt.Println("  -d                            后台运行模式")
	fmt.Println("  -m <内存限制>                  内存限制（如 100m, 1g）")
	fmt.Println("  -c <CPU份额>                   CPU 份额（如 512）")
	fmt.Println("  -n <网络名称>                  加入指定网络")
	fmt.Println("  -p <宿主端口>:<容器端口>        端口映射（如 8080:80）")
	fmt.Println("  --name <容器名>                设置容器名称")
	fmt.Println()
	fmt.Println("network 子命令:")
	fmt.Println("  mini-docker network create <名称>         创建网络")
	fmt.Println("  mini-docker network list                   列出网络")
	fmt.Println("  mini-docker network delete <名称>          删除网络")
}

func runCommand() {
	args := os.Args[2:]
	if len(args) < 2 {
		fmt.Println("用法: mini-docker run [选项] <镜像> <命令>")
		os.Exit(1)
	}

	config := container.RunConfig{
		Tty:       false,
		Detach:    false,
		Memory:    "",
		CpuShares: "",
		Network:   "",
		Name:      "",
		PortMap:   "",
	}

	i := 0
	for i < len(args) {
		switch args[i] {
		case "-it":
			config.Tty = true
			i++
		case "-d":
			config.Detach = true
			i++
		case "-m":
			if i+1 >= len(args) {
				fmt.Println("错误: -m 需要指定内存限制")
				os.Exit(1)
			}
			config.Memory = args[i+1]
			i += 2
		case "-c":
			if i+1 >= len(args) {
				fmt.Println("错误: -c 需要指定 CPU 份额")
				os.Exit(1)
			}
			config.CpuShares = args[i+1]
			i += 2
		case "-n":
			if i+1 >= len(args) {
				fmt.Println("错误: -n 需要指定网络名称")
				os.Exit(1)
			}
			config.Network = args[i+1]
			i += 2
		case "-p":
			if i+1 >= len(args) {
				fmt.Println("错误: -p 需要指定端口映射（如 8080:80）")
				os.Exit(1)
			}
			config.PortMap = args[i+1]
			i += 2
		case "--name":
			if i+1 >= len(args) {
				fmt.Println("错误: --name 需要指定容器名称")
				os.Exit(1)
			}
			config.Name = args[i+1]
			i += 2
		default: //遇到非 flag 参数，跳出循环
			goto parseDone
		}
	}

parseDone:
	if i >= len(args) {
		fmt.Println("错误: 需要指定镜像名称")
		os.Exit(1)
	}
	//最后两个参数，一个是镜像名称，另一个是要在容器中执行的cmd命令
	config.Image = args[i]
	i++
	if i >= len(args) {
		fmt.Println("错误: 需要指定要执行的命令")
		os.Exit(1)
	}
	config.Cmd = args[i:]

	cg := &cgroup.CgroupManager{}
	if config.Memory != "" {
		cg.MemoryLimit = config.Memory
	}
	if config.CpuShares != "" {
		cg.CpuShares = config.CpuShares
	}

	netManager := &network.NetworkManager{}
	if config.Network != "" {
		netManager.NetworkName = config.Network
	}
	if config.PortMap != "" {
		netManager.PortMap = config.PortMap
	}

	err := container.Run(config, cg, netManager)
	if err != nil {
		fmt.Printf("运行容器失败: %v\n", err)
		os.Exit(1)
	}
}

func execCommand() {
	args := os.Args[2:]
	if len(args) < 2 {
		fmt.Println("用法: mini-docker exec <容器ID> <命令>")
		os.Exit(1)
	}
	containerID := args[0]
	cmd := args[1:]
	err := container.Exec(containerID, cmd)
	if err != nil {
		fmt.Printf("执行命令失败: %v\n", err)
		os.Exit(1)
	}
}

func psCommand() {
	containers, err := container.ListContainers()
	if err != nil {
		fmt.Printf("列出容器失败: %v\n", err)
		os.Exit(1)
	}
	if len(containers) == 0 {
		fmt.Println("没有运行中的容器")
		return
	}
	fmt.Printf("%-20s %-15s %-15s %-10s %-20s\n", "容器ID", "名称", "镜像", "状态", "创建时间")
	for _, c := range containers {
		fmt.Printf("%-20s %-15s %-15s %-10s %-20s\n",
			c.ID[:12], c.Name, c.Image, c.Status, c.CreatedAt)
	}
}

func stopCommand() {
	args := os.Args[2:]
	if len(args) < 1 {
		fmt.Println("用法: mini-docker stop <容器ID>")
		os.Exit(1)
	}
	err := container.Stop(args[0])
	if err != nil {
		fmt.Printf("停止容器失败: %v\n", err)
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

	cgroupName, err := container.GetContainerCgroupName(args[0])
	if err != nil {
		fmt.Printf("查找容器失败: %v\n", err)
		os.Exit(1)
	}

	cg := &cgroup.CgroupManager{CgroupName: cgroupName}
	err = container.Pause(args[0], cg)
	if err != nil {
		fmt.Printf("暂停容器失败: %v\n", err)
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

	cgroupName, err := container.GetContainerCgroupName(args[0])
	if err != nil {
		fmt.Printf("查找容器失败: %v\n", err)
		os.Exit(1)
	}

	cg := &cgroup.CgroupManager{CgroupName: cgroupName}
	err = container.Unpause(args[0], cg)
	if err != nil {
		fmt.Printf("恢复容器失败: %v\n", err)
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
	err := container.Remove(args[0])
	if err != nil {
		fmt.Printf("删除容器失败: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("容器 %s 已删除\n", args[0])
}

func imagesCommand() {
	images, err := image.ListImages()
	if err != nil {
		fmt.Printf("列出镜像失败: %v\n", err)
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
	err := image.Pull(args[0])
	if err != nil {
		fmt.Printf("拉取镜像失败: %v\n", err)
		os.Exit(1)
	}
}

func rmiCommand() {
	args := os.Args[2:]
	if len(args) < 1 {
		fmt.Println("用法: mini-docker rmi <镜像名>")
		os.Exit(1)
	}
	err := image.RemoveImage(args[0])
	if err != nil {
		fmt.Printf("删除镜像失败: %v\n", err)
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

	netManager := &network.NetworkManager{}
	switch args[0] {
	case "create":
		if len(args) < 2 {
			fmt.Println("用法: mini-docker network create <名称>")
			os.Exit(1)
		}
		err := netManager.Create(args[1])
		if err != nil {
			fmt.Printf("创建网络失败: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("网络 %s 创建成功\n", args[1])
	case "list":
		nets, err := netManager.List()
		if err != nil {
			fmt.Printf("列出网络失败: %v\n", err)
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
		err := netManager.Delete(args[1])
		if err != nil {
			fmt.Printf("删除网络失败: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("网络 %s 已删除\n", args[1])
	default:
		fmt.Printf("未知网络子命令: %s\n", args[0])
		os.Exit(1)
	}
}
