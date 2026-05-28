package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"mini-docker/constants"
	"mini-docker/container"
	"mini-docker/daemon"
	"mini-docker/image"
	"mini-docker/network"
	"mini-docker/runtime"
	"mini-docker/shim"
	"mini-docker/volume"

	"golang.org/x/term"
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
	case "runtime":
		runtimeCommand()
	case "shim":
		shim.Run(os.Args[2:])
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
	fmt.Println("内部命令（对齐 Docker 分层架构）:")
	fmt.Println("  mini-docker runtime  <create|start|kill|delete|state>  OCI 运行时 (对标 runc)")
	fmt.Println("  mini-docker shim     <id> <bundle>                    容器 shim 进程 (对标 containerd-shim)")
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

// execSimpleCmd 发送请求并处理通用的错误/失败响应模式
// action: 操作名称（如"启动容器"、"停止容器"），用于格式化失败消息
// onSuccess: 请求成功后的回调，传入响应对象；可为 nil 表示只打印成功消息
// successMsg: 当 onSuccess 为 nil 时使用的成功消息模板（可用 %s 占位符）
// extraArgs: successMsg 的额外参数
func execSimpleCmd(req daemon.Request, action string, onSuccess func(*daemon.Response), successMsg string, extraArgs ...interface{}) {
	resp, err := sendRequest(req)
	if err != nil {
		fmt.Printf("错误: %v\n", err)
		os.Exit(1)
	}
	if !resp.Success {
		fmt.Printf("%s失败: %s\n", action, resp.Message)
		os.Exit(1)
	}
	if onSuccess != nil {
		onSuccess(resp)
	} else if successMsg != "" {
		fmt.Printf(successMsg+"\n", extraArgs...)
	}
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
parseLoop:
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
		case "-n", "--network":
			if i+1 >= len(args) {
				fmt.Println("错误: -n 需要指定网络名称")
				os.Exit(1)
			}
			reqArgs["network"] = args[i+1]
			i += 2
		case "--network=none", "--network=host":
			reqArgs["network"] = strings.TrimPrefix(args[i], "--network=")
			i++
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
				fmt.Println("错误: --restart 需要指定策略 (no|always|on-failure[:max])")
				os.Exit(1)
			}
			policy := args[i+1]
			if idx := strings.LastIndex(policy, ":"); idx != -1 {
				reqArgs["restart_policy"] = policy[:idx]
				reqArgs["max_restart_retries"] = policy[idx+1:]
			} else {
				reqArgs["restart_policy"] = policy
			}
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
			break parseLoop
		}
	}

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
	cmdJSON, _ := json.Marshal(args[i:])
	reqArgs["cmd_json"] = string(cmdJSON)

	if reqArgs["tty"] == "true" && reqArgs["detach"] != "true" {
		reqArgs["stream"] = "true"
		runInteractive(reqArgs)
		return
	}

	resp, err := sendRequest(daemon.Request{Type: "run", Args: reqArgs})
	if err != nil {
		fmt.Printf("错误: %v\n", err)
		os.Exit(1)
	}
	if !resp.Success {
		fmt.Printf("%s\n", resp.Message)
		os.Exit(1)
	}

	if resp.Data != nil {
		data, _ := json.MarshalIndent(resp.Data, "", "  ")
		fmt.Println(string(data))
	}
}

// runInteractive 通过 Daemon 以流式 I/O 运行交互式容器
// 对齐 Docker 的 docker run -it 架构：
// CLI ←→ Unix Socket(流式) ←→ Daemon ←→ Shim(attach) ←→ pty ←→ 容器进程
func runInteractive(reqArgs map[string]string) {
	client := daemon.NewClient()
	conn, resp, err := client.SendStream(daemon.Request{Type: "run", Args: reqArgs})
	if err != nil {
		fmt.Printf("错误: %v\n", err)
		os.Exit(1)
	}
	if resp == nil {
		fmt.Println("错误: 未收到响应")
		os.Exit(1)
	}
	if !resp.Success {
		fmt.Printf("%s\n", resp.Message)
		os.Exit(1)
	}
	if !resp.Stream { //!resp.Stream（表明 Daemon 并没有成功开启流式通道）
		if resp.Data != nil {
			data, _ := json.MarshalIndent(resp.Data, "", "  ")
			fmt.Println(string(data))
		}
		return
	}

	//宿主机终端切换为原始模式（Raw Mode）,为什么需要 Raw 模式（原始模式）？
	//默认模式（Cooked/Canonical Mode）：平常我们在 Linux 终端打字时，终端驱动是有“缓存”的（必须要按回车键，程序才能收到输入）。同时，像 Ctrl+C、Ctrl+Z 这样的组合键会被宿主机的终端驱动拦截，转换为 SIGINT 或 SIGTSTP 信号，直接杀死或挂起你的 mini-docker CLI 本身，而这些信号根本无法传给容器内部。
	//原始模式（Raw Mode）：调用 term.MakeRaw 后，宿主机的终端驱动将不再拦截任何控制字符，也不再进行行缓存。
	//你按下的每一个键（包括 Tab、方向键、Ctrl+C、Ctrl+D），都会作为最原始的字节流（Byte Stream），立刻被宿主机终端捕获。
	//Raw 模式下，你按的每一个键（包括 Ctrl+C、方向键、Tab）都作为 原始字节流 立刻被 os.Stdin 读到，不会被终端驱动拦截或缓存。
	oldState, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err == nil {
		//defer term.Restore 的保障作用：
		//如果宿主机终端一直处于 Raw 模式，一旦 mini-docker 退出，宿主机的命令行窗口就会陷入"按回车不换行"、"打字不显示"或者无法退出等异常卡死状态（因为终端属性被污染了）。
		//defer term.Restore 确保了无论 runInteractive 函数因为正常退出、异常报错还是程序 Panic 中断，都必定会将宿主机终端恢复为先前的正常状态（oldState）
		defer term.Restore(int(os.Stdin.Fd()), oldState)
	}

	var containerID string
	if resp.Data != nil {
		data, _ := json.Marshal(resp.Data)
		var info struct {
			ID string `json:"id"`
		}
		if json.Unmarshal(data, &info) == nil && info.ID != "" {
			containerID = info.ID
		}
	}

	if containerID != "" {
		ctx, cancel := context.WithCancel(context.Background())
		go handleSIGWINCH(ctx, containerID)
		defer cancel()
	}

	//构建双向 I/O 拷贝管道
	stdinDone := make(chan struct{})
	stdoutDone := make(chan struct{})
	//处定义了两个用于同步的 Go Channel（stdinDone、stdoutDone），并启动了两个后台协程（Goroutines）来实现全双工的实时拷贝：
	//输入流拷贝协程（Stdout ──> Conn）：
	//io.Copy(conn, os.Stdin)：这是一个死循环。它不断读取宿主机键盘输入（os.Stdin），一有数据就立刻写入到 Unix Socket 连接（conn）中，发送给 Daemon [1]。
	//输出流拷贝协程（Conn ──> Stdout）：
	//io.Copy(os.Stdout, conn)：这同样是一个死循环。它不断从 Unix Socket 连接（conn）中读取来自容器内部的屏幕输出，并将其写入到宿主机的控制台屏幕（os.Stdout）上 [1]。
	//这两条管道并行工作，就实现了用户在键盘上敲一个字母，容器里立刻做出响应并渲染到宿主机屏幕上的效果。
	go func() {
		defer close(stdinDone)
		_, _ = io.Copy(conn, os.Stdin)
	}()
	go func() {
		defer close(stdoutDone)
		_, _ = io.Copy(os.Stdout, conn)
	}()

	//阻塞等待退出：
	//CLI 进程在这里会被挂起。
	//什么时候 stdoutDone 会被关闭？ 当容器内的进程（如 /bin/sh）退出，或者容器因故死亡时，Daemon 端的 Shim 进程会关闭对应的 Socket 写入端。
	//一旦连接断开，CLI 端的 io.Copy(os.Stdout, conn) 读到 EOF（输入结束标记），该协程结束，并触发 defer close(stdoutDone)。
	//此时，<-stdoutDone 收到通道关闭的信号，解除阻塞，CLI 开始执行收尾工作。
	<-stdoutDone

	conn.Close() //主动调用 conn.Close() 彻底关闭客户端这一侧的连接
	//释放 Stdin 协程并防止死锁：
	//当 conn 被关闭后，仍在等待键盘输入的 io.Copy(conn, os.Stdin) 会因为连接关闭而发生写入错误，从而结束运行并触发 defer close(stdinDone)。
	//为什么需要 select 和 time.After(1 * time.Second)？
	//在某些极特殊情况下，宿主机的 os.Stdin（标准输入）可能正处于系统调用阻塞中（例如正卡在某个读取键盘输入的内核态调用中）。
	//为了防止 CLI 进程因为输入流无法彻底释放而陷入永久死锁（卡死在等待 <-stdinDone 上），这里设计了一个 1 秒超时机制。
	//如果在 1 秒内 stdinDone 成功关闭则皆大欢喜；如果超过 1 秒还没关闭，则强行超时退出。此时函数结束，外层的 term.Restore 被触发，CLI 进程正常销毁，终端重置，用户顺利返回到宿主机的 Shell 提示符下
	select {
	case <-stdinDone:
	case <-time.After(1 * time.Second):
	}
}

func execCommand() {
	args := os.Args[2:]
	if len(args) < 2 {
		fmt.Println("用法: mini-docker exec <容器ID> <命令>")
		os.Exit(1)
	}

	containerID := args[0]
	cmdArgs := args[1:]

	isTTY := isTerminal()

	reqArgs := map[string]string{
		"container_id": containerID,
		"cmd":          strings.Join(cmdArgs, " "),
		"tty":          fmt.Sprintf("%v", isTTY),
	}
	if cmdJSON, err := json.Marshal(cmdArgs); err == nil {
		reqArgs["cmd_json"] = string(cmdJSON)
	}

	if isTTY {
		client := daemon.NewClient()
		conn, resp, err := client.SendStream(daemon.Request{Type: "exec", Args: reqArgs})
		if err != nil {
			fmt.Printf("错误: %v\n", err)
			os.Exit(1)
		}
		if resp == nil || !resp.Success {
			msg := "未知错误"
			if resp != nil {
				msg = resp.Message
			}
			fmt.Printf("%s\n", msg)
			os.Exit(1)
		}
		if !resp.Stream {
			return
		}

		oldState, err := term.MakeRaw(int(os.Stdin.Fd()))
		if err == nil {
			defer term.Restore(int(os.Stdin.Fd()), oldState)
		}

		execContainerID := ""
		if resp.Data != nil {
			data, _ := json.Marshal(resp.Data)
			var info struct {
				ID string `json:"id"`
			}
			if json.Unmarshal(data, &info) == nil && info.ID != "" {
				execContainerID = info.ID
			}
		}

		if execContainerID != "" {
			ctx, cancel := context.WithCancel(context.Background())
			go handleSIGWINCH(ctx, execContainerID)
			defer cancel()
		}

		stdinDone := make(chan struct{})
		stdoutDone := make(chan struct{})

		go func() {
			defer close(stdinDone)
			_, _ = io.Copy(conn, os.Stdin)
		}()
		go func() {
			defer close(stdoutDone)
			_, _ = io.Copy(os.Stdout, conn)
		}()

		<-stdoutDone
		conn.Close()
		select {
		case <-stdinDone:
		case <-time.After(1 * time.Second):
		}
		return
	}

	resp, err := sendRequest(daemon.Request{Type: "exec", Args: reqArgs})
	if err != nil {
		fmt.Printf("错误: %v\n", err)
		os.Exit(1)
	}
	if !resp.Success {
		fmt.Printf("%s\n", resp.Message)
		os.Exit(1)
	}
	if resp.Data != nil {
		fmt.Printf("%v\n", resp.Data)
	}
}

func isTerminal() bool {
	fi, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

func handleSIGWINCH(ctx context.Context, containerID string) {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGWINCH)
	defer signal.Stop(sigCh)
	for {
		select {
		case <-ctx.Done():
			return
		case <-sigCh:
			sendResize(containerID)
		}
	}
}

func sendResize(containerID string) {
	cols, rows, err := term.GetSize(int(os.Stdin.Fd()))
	if err != nil {
		return
	}
	sendRequest(daemon.Request{
		Type: "resize",
		Args: map[string]string{
			"container_id": containerID,
			"rows":         fmt.Sprintf("%d", rows),
			"cols":         fmt.Sprintf("%d", cols),
		},
	})
}

func psCommand() {
	execSimpleCmd(daemon.Request{Type: "ps", Args: map[string]string{}},
		"列出容器", func(resp *daemon.Response) {
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
					c.ID, c.Name, c.Image, c.Status, c.CreatedAt)
			}
		}, "")
}

func startCommand() {
	args := os.Args[2:]
	if len(args) < 1 {
		fmt.Println("用法: mini-docker start <容器ID>")
		os.Exit(1)
	}
	execSimpleCmd(daemon.Request{Type: "start", Args: map[string]string{"container_id": args[0]}},
		"启动容器", nil, "容器 %s 已启动", args[0])
}

func stopCommand() {
	args := os.Args[2:]
	if len(args) < 1 {
		fmt.Println("用法: mini-docker stop <容器ID>")
		os.Exit(1)
	}
	execSimpleCmd(daemon.Request{Type: "stop", Args: map[string]string{"container_id": args[0]}},
		"停止容器", nil, "容器 %s 已停止", args[0])
}

func pauseCommand() {
	args := os.Args[2:]
	if len(args) < 1 {
		fmt.Println("用法: mini-docker pause <容器ID>")
		os.Exit(1)
	}
	execSimpleCmd(daemon.Request{Type: "pause", Args: map[string]string{"container_id": args[0]}},
		"暂停容器", nil, "容器 %s 已暂停", args[0])
}

func unpauseCommand() {
	args := os.Args[2:]
	if len(args) < 1 {
		fmt.Println("用法: mini-docker unpause <容器ID>")
		os.Exit(1)
	}
	execSimpleCmd(daemon.Request{Type: "unpause", Args: map[string]string{"container_id": args[0]}},
		"恢复容器", nil, "容器 %s 已恢复", args[0])
}

func rmCommand() {
	args := os.Args[2:]
	if len(args) < 1 {
		fmt.Println("用法: mini-docker rm <容器ID>")
		os.Exit(1)
	}
	execSimpleCmd(daemon.Request{Type: "rm", Args: map[string]string{"container_id": args[0]}},
		"删除容器", nil, "容器 %s 已删除", args[0])
}

func logsCommand() {
	args := os.Args[2:]
	if len(args) < 1 {
		fmt.Println("用法: mini-docker logs <容器ID>")
		os.Exit(1)
	}
	execSimpleCmd(daemon.Request{Type: "logs", Args: map[string]string{"container_id": args[0]}},
		"获取日志", func(resp *daemon.Response) {
			if resp.Data != nil {
				data, _ := json.Marshal(resp.Data)
				var logLines []string
				if err := json.Unmarshal(data, &logLines); err == nil {
					for _, line := range logLines {
						fmt.Print(line)
					}
				} else {
					fmt.Printf("%v\n", resp.Data)
				}
			}
		}, "")
}

func eventsCommand() {
	execSimpleCmd(daemon.Request{Type: "events", Args: map[string]string{}},
		"获取事件", func(resp *daemon.Response) {
			if resp.Data != nil {
				data, _ := json.Marshal(resp.Data)
				var events []map[string]interface{}
				if err := json.Unmarshal(data, &events); err == nil {
					for _, e := range events {
						edata, _ := json.Marshal(e)
						fmt.Println(string(edata))
					}
				}
			}
		}, "")
}

func imagesCommand() {
	execSimpleCmd(daemon.Request{Type: "images", Args: map[string]string{}},
		"列出镜像", func(resp *daemon.Response) {
			data, _ := json.Marshal(resp.Data)
			var images []image.ImageInfo
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
		}, "")
}

func pullCommand() {
	args := os.Args[2:]
	if len(args) < 1 {
		fmt.Println("用法: mini-docker pull <镜像名>")
		os.Exit(1)
	}

	client := daemon.NewClient().WithTimeout(constants.LongOperationTimeout)
	resp, err := client.Send(daemon.Request{
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
	execSimpleCmd(daemon.Request{Type: "rmi", Args: map[string]string{"image": args[0]}},
		"删除镜像", nil, "镜像 %s 已删除", args[0])
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
		execSimpleCmd(daemon.Request{Type: "network_create", Args: map[string]string{"name": args[1]}},
			"创建网络", nil, "网络 %s 创建成功", args[1])
	case "list":
		execSimpleCmd(daemon.Request{Type: "network_list", Args: map[string]string{}},
			"列出网络", func(resp *daemon.Response) {
				data, _ := json.Marshal(resp.Data)
				var nets []network.NetworkInfo
				if err := json.Unmarshal(data, &nets); err != nil {
					fmt.Printf("解析网络列表失败: %v\n", err)
					os.Exit(1)
				}
				if len(nets) == 0 {
					fmt.Println("没有网络")
					return
				}
				fmt.Printf("%-20s %-20s %-20s %-20s\n", "网络名称", "子网", "已分配IP数", "类型")
				for _, n := range nets {
					netType := "自定义"
					if network.IsDefaultNetwork(n.Name) {
						netType = "默认"
					}
					fmt.Printf("%-20s %-20s %-20d %-20s\n", n.Name, n.Subnet, len(n.Allocated), netType)
				}
			}, "")
	case "delete":
		if len(args) < 2 {
			fmt.Println("用法: mini-docker network delete <名称>")
			os.Exit(1)
		}
		execSimpleCmd(daemon.Request{Type: "network_delete", Args: map[string]string{"name": args[1]}},
			"删除网络", nil, "网络 %s 已删除", args[1])
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
		execSimpleCmd(daemon.Request{Type: "volume_create", Args: map[string]string{"name": args[1]}},
			"创建卷", nil, "卷 %s 创建成功", args[1])
	case "list":
		execSimpleCmd(daemon.Request{Type: "volume_list", Args: map[string]string{}},
			"列出卷", func(resp *daemon.Response) {
				volumes, ok := resp.Data.([]interface{})
				if !ok || len(volumes) == 0 {
					fmt.Println("没有数据卷")
					return
				}
				var volList []volume.VolumeInfo
				vdata, _ := json.Marshal(resp.Data)
				if json.Unmarshal(vdata, &volList) != nil || len(volList) == 0 {
					fmt.Println("没有数据卷")
					return
				}
				fmt.Printf("%-20s %-20s %-40s %-20s\n", "卷名", "驱动", "挂载路径", "创建时间")
				for _, v := range volList {
					fmt.Printf("%-20s %-20s %-40s %-20s\n", v.Name, v.Driver, v.MountPath, v.CreatedAt)
				}
			}, "")
	case "rm":
		if len(args) < 2 {
			fmt.Println("用法: mini-docker volume rm <名称>")
			os.Exit(1)
		}
		execSimpleCmd(daemon.Request{Type: "volume_rm", Args: map[string]string{"name": args[1]}},
			"删除卷", nil, "卷 %s 已删除", args[1])
	case "inspect":
		if len(args) < 2 {
			fmt.Println("用法: mini-docker volume inspect <名称>")
			os.Exit(1)
		}
		execSimpleCmd(daemon.Request{Type: "volume_inspect", Args: map[string]string{"name": args[1]}},
			"查看卷", func(resp *daemon.Response) {
				var vol volume.VolumeInfo
				vdata, _ := json.Marshal(resp.Data)
				if json.Unmarshal(vdata, &vol) == nil {
					fmt.Printf("卷名:     %s\n", vol.Name)
					fmt.Printf("驱动:     %s\n", vol.Driver)
					fmt.Printf("挂载路径: %s\n", vol.MountPath)
					fmt.Printf("创建时间: %s\n", vol.CreatedAt)
				}
			}, "")
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

	resp, err := daemon.NewClient().WithTimeout(constants.LongOperationTimeout).Send(daemon.Request{
		Type: "build",
		Args: map[string]string{
			"dockerfile": filepath.Join(contextDir, "Dockerfile"),
			"context":    contextDir,
			"tag":        tag,
		},
	})
	if err != nil {
		fmt.Printf("构建失败: %v\n", err)
		os.Exit(1)
	}
	if !resp.Success {
		fmt.Printf("构建失败: %s\n", resp.Message)
		os.Exit(1)
	}

	if result, ok := resp.Data.(map[string]interface{}); ok {
		if imageID, ok := result["image_id"].(string); ok && imageID != "" {
			fmt.Printf("构建成功: %s\n", imageID)
		}
	}
}

func runtimeCommand() {
	args := os.Args[2:]
	if len(args) < 1 {
		fmt.Println("用法: mini-docker runtime <create|start|kill|delete|state> [参数]")
		os.Exit(1)
	}

	switch args[0] {
	case "create":
		runtime.Create(args[1:])
	case "start":
		runtime.Start(args[1:])
	case "kill":
		runtime.Kill(args[1:])
	case "delete":
		runtime.Delete(args[1:])
	case "state":
		runtime.State(args[1:])
	default:
		fmt.Printf("未知 runtime 子命令: %s\n", args[0])
		os.Exit(1)
	}
}
