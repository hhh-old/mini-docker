# mini-docker 交互式容器 (-it) 全链路解析

## 1. AttachTask：Daemon 如何连接到容器的 TTY

### 作用

`d.service.AttachTask(containerID)` 的作用是：**通过 Unix 套接字连接到容器对应的 shim 进程，建立一条双向 I/O 流通道，使用户的终端可以与容器的 TTY 进行交互（输入/输出）。**

### 调用上下文

```go
// handler.go - runWithID 中
if req.Args["stream"] == "true" && conn != nil {
    shimConn, err := d.service.AttachTask(containerID)  // 先 attach
    d.service.StartTask(containerID)                     // 再 start
    go d.handleAttachStream(conn, shimConn, streamReady) // 启动双向转发
}
```

**先 attach 再 start**，确保容器启动后的第一行输出都不会丢失。

### AttachTask 内部流程

```go
// service.go
func (s *Service) AttachTask(containerID string) (net.Conn, error) {
    conn, err := connectShim(containerID)  // 1. 连接 shim 的 Unix 套接字
    req := types.ShimRequest{Type: "attach"}
    json.NewEncoder(conn).Encode(req)      // 2. 发送 attach 请求
    var resp types.ShimResponse
    json.NewDecoder(conn).Decode(&resp)    // 3. 读取响应
    return conn, nil                        // 4. 返回连接（后续用于双向 I/O）
}
```

`connectShim` 通过 `net.DialTimeout("unix", socketPath, 5s)` 连接到 `{runtimeDir}/{containerID}/shim.sock`。

### 返回值的用途

返回的 `shimConn`（`net.Conn`）是一条**持久化的双向流通道**：

- **下行**（容器 → 用户）：shim 把容器进程的 stdout/stderr 写入这个连接，daemon 通过 `handleAttachStream` 读取后转发给用户的 `conn`
- **上行**（用户 → 容器）：用户在终端输入的内容通过 `conn` → `shimConn` → shim → 容器进程的 stdin

---

## 2. Shim 进程如何处理 attach 请求

### Shim 的 PTY 创建（attach 的前提）

```go
// shim.go - run() 函数中
var containerPTY *pty.PTY
if isTTY {
    containerPTY, err = pty.Open()  // 创建 Master/Slave 对
}

// 将 Slave 传给 runtime create，然后关闭 shim 自己的 Slave 句柄
createArgs = append(createArgs, "--console", containerPTY.Name)
containerPTY.Slave.Close()  // 关键！确保容器退出时 Master 端能收到 EOF
containerPTY.Slave = nil
```

**为什么要关闭 Slave？** 这是 Linux PTY 的硬性规则：只有所有引用 Slave 的 FD 都关闭后，内核才会在 Master 端产生 EOF。如果不关，容器退出后 `io.Copy` 会永远阻塞。

### attach 请求的处理（核心）

```go
// shim.go - handleShimConn 中
case "attach":
    // ❶ 校验 PTY 是否存在
    if ctx.containerPTY == nil || ctx.containerPTY.Master == nil {
        json.NewEncoder(conn).Encode(types.ShimResponse{Success: false, Message: "容器未启用 TTY"})
        return
    }

    // ❷ 回复成功，告知 daemon 这是一个流式连接
    json.NewEncoder(conn).Encode(types.ShimResponse{Success: true, Stream: true})

    // ❸ 通知其他等待者：attach 已就绪
    ctx.attachOnce.Do(func() { close(ctx.attachReady) })

    // ❹ 启动双向 I/O 转发
    var once sync.Once
    done := make(chan struct{})

    go func() {
        defer once.Do(func() { close(done) })
        _, _ = io.Copy(ctx.containerPTY.Master, conn)  // 用户输入 → 容器
    }()
    go func() {
        defer once.Do(func() { close(done) })
        _, _ = io.Copy(conn, ctx.containerPTY.Master)  // 容器输出 → 用户
    }()

    // ❺ 等待结束条件
    select {
    case <-done:           // I/O 转发结束（任一方向断开）
    case <-ctx.exitReady:  // 容器进程退出
        time.Sleep(100 * time.Millisecond)
    case <-ctx.shutdownDone:  // shim 收到 shutdown 命令
    }
```

### 各步骤详解

| 步骤 | 含义 |
|------|------|
| ❶ 校验 PTY | attach 只对 TTY 模式的容器有意义，后台容器无法 attach |
| ❷ 回复 Stream=true | 告诉 daemon 这条连接后续作为流式通道使用，不再发 JSON |
| ❸ attachReady 信号 | 通知其他 goroutine attach 已就绪，确保容器启动后输出不丢失 |
| ❹ 双向 io.Copy | goroutine 1: `conn → Master`（用户输入→容器）；goroutine 2: `Master → conn`（容器输出→用户） |
| ❺ 等待结束 | I/O 断开、容器退出、shim 关闭，三种退出条件 |

### select 阻塞不会卡住 shim

`select` 阻塞的是 `handleShimConn` 这个 **goroutine**，不是 shim 进程本身。因为 `serveControlSocket` 用 `go handleShimConn(conn, ctx)` 启动新 goroutine 处理每个连接，shim 主循环不受影响，仍可接受 kill、state、resize 等其他请求。

---

## 3. io.Copy(ctx.containerPTY.Master, conn) 详解

### 含义

```go
io.Copy(dst=Master, src=conn)
```

从 `conn`（daemon 发来的 Unix 奟接字连接）读取数据，写入 `ctx.containerPTY.Master`（PTY 的 Master 端）。

### 数据流路径

```
用户键盘输入
    │
    ▼
  CLI 进程
    │
    ▼ (TCP/Unix)
  Daemon (handleAttachStream)
    │
    ▼ (Unix 奟接字 shim.sock)
  conn (net.Conn)          ←── io.Copy 的 src（读取端）
    │
    ▼
  PTY Master (*os.File)    ←── io.Copy 的 dst（写入端）
    │
    ▼ (内核 PTY 中转，自动完成)
  PTY Slave (/dev/pts/N)
    │
    ▼
  容器进程 stdin
```

### 会阻塞吗？

**会，而且是长时间阻塞。** 这是设计上的预期行为。

`io.Copy` 内部循环的主要阻塞点在 `src.Read(buf)`，即从 `conn` 读取数据。用户不输入时，`conn.Read` 一直阻塞，goroutine 挂起等待。

这和交互式终端的正常工作模式一致——大部分时间 Shell 都在 `read()` 上阻塞等待用户输入。

### 两个 io.Copy 的配合

```go
// goroutine 1: 用户输入 → 容器
go func() {
    _, _ = io.Copy(ctx.containerPTY.Master, conn)   // 阻塞等待用户输入
}()

// goroutine 2: 容器输出 → 用户
go func() {
    _, _ = io.Copy(conn, ctx.containerPTY.Master)   // 阻塞等待容器输出
}()
```

两个 goroutine **各自独立阻塞**，互不影响，实现**全双工**通信。

---

## 4. 每条 conn 是一个独立的底层 socket

### 代码证据

daemon 端每次调用 `connectShim()` 都建立一条全新的 Unix 域套接字连接：

```go
func connectShim(containerID string) (net.Conn, error) {
    conn, err := net.DialTimeout("unix", socketPath, 5*time.Second)  // 每次新建连接
    return conn, nil
}
```

### 层次关系

```
Go 类型层:    net.Conn (接口)
                 │
Go 实现层:    *net.UnixConn (具体类型，实现了 net.Conn 接口)
                 │
系统调用层:   socket() → 返回 fd (如 fd=5)
                 │
内核层:       Unix 域套接字 (AF_UNIX, SOCK_STREAM)
                 │
文件系统层:   /var/lib/mini-docker/shim/<containerID>/shim.sock
```

### 为什么不会数据混乱

每种 shim 请求都通过 `connectShim()` 建立独立的 Unix 奟套接字连接：

```
daemon                              shim
  │                                   │
  │─── conn1 (新建) ── attach ───────>│  goroutine A: 专门做 I/O 转发
  │─── conn2 (新建) ── kill ─────────>│  goroutine B: 发信号，处理完就关闭
  │─── conn3 (新建) ── resize ───────>│  goroutine C: 改窗口大小，处理完就关闭
  │─── conn4 (新建) ── state ────────>│  goroutine D: 查状态，处理完就关闭
```

内核为每个 socket fd 维护独立的读写缓冲区，不同请求走不同 socket，数据天然隔离。

### attach 连接的两阶段协议

```
阶段1: 请求/响应 (JSON 一问一答)
  daemon ──JSON──> shim    : {"type":"attach"}
  daemon <──JSON── shim    : {"success":true,"stream":true}

阶段2: 原始流式 I/O (纯二进制，不再有 JSON)
  daemon ──raw───> shim    : 用户键盘输入 (ls\n, cat foo\n, ...)
  daemon <──raw─── shim    : 容器终端输出 (文件列表, 命令结果, ...)
```

JSON Decoder 只读取 JSON 数据所需的字节数，不会多读。阶段 2 开始后，conn 中剩余的所有数据都是用户输入，不会再碰到任何 JSON 结构。

---

## 5. 用户输入如何从键盘到达 shim 的完整数据流

### 完整链路（4 段连接、3 个进程）

```
用户键盘
  │
  ▼
┌──────────┐         ┌──────────┐         ┌──────────┐         ┌──────────┐
│  os.Stdin │──(1)──>│ CLI conn │──(2)──>│daemonConn│──(3)──>│ shimConn │──(4)──> PTY Master → Slave → 容器
│ (终端fd)  │         │(Unix sock)│         │(Unix sock)│         │(Unix sock)│
└──────────┘         └──────────┘         └──────────┘         └──────────┘
  CLI 进程                                  Daemon 进程            Shim 进程
```

### 第 (1) 段：键盘 → os.Stdin → conn

```go
// main.go - runInteractive
oldState, err := term.MakeRaw(int(os.Stdin.Fd()))  // 终端切 Raw 模式
defer term.Restore(int(os.Stdin.Fd()), oldState)

go func() {
    _, _ = io.Copy(conn, os.Stdin)  // 从键盘读，写到 conn
}()
```

Raw 模式下，每个按键（包括 Ctrl+C、方向键、Tab）都作为原始字节流立刻被 `os.Stdin` 读到。

### 第 (2) 段：CLI conn → daemon 的 daemonConn

CLI 通过 `SendStream` 建立与 daemon 的 Unix 奟接字连接，发送 JSON 请求后**连接保持不关闭**，后续用于双向 I/O 转发。

daemon 端 `handleConnection` 检测到 `resp.Stream == true` 时，**不关闭 conn**。

### 第 (3) 段：daemonConn → shimConn（daemon 中继）

```go
// handler.go - relayStream
func relayStream(daemonConn, shimConn net.Conn, streamReady chan struct{}) {
    go func() {
        _, _ = io.Copy(shimConn, daemonConn)  // 用户输入 → shim
    }()
    go func() {
        _, _ = io.Copy(daemonConn, shimConn)  // 容器输出 → 用户
    }()
    <-done
}
```

**daemon 是纯中继**——不关心数据内容，只是用 `io.Copy` 原样转发字节。

### 第 (4) 段：shimConn → PTY Master → 容器

```go
// shim.go - attach 处理
go func() {
    _, _ = io.Copy(ctx.containerPTY.Master, conn)  // conn → Master
}()
```

从 conn 读到用户输入字节，写入 PTY Master。内核自动将 Master 端数据转发到 Slave 端，容器进程从 stdin（Slave）读到。

### 完整时序图

```
[用户]       [CLI]              [Daemon]             [Shim]           [容器]
  │            │                   │                    │                │
  │ ls\n       │                   │                    │                │
  │───────────>│                   │                    │                │
  │            │ io.Copy(conn,     │                    │                │
  │            │   os.Stdin)       │                    │                │
  │            │──────────────────>│                    │                │
  │            │                   │ io.Copy(shimConn,  │                │
  │            │                   │   daemonConn)      │                │
  │            │                   │───────────────────>│                │
  │            │                   │                    │ io.Copy(Master,│
  │            │                   │                    │   conn)        │
  │            │                   │                    │───────────────>│
  │            │                   │                    │   PTY Master   │
  │            │                   │                    │   ──内核──>     │
  │            │                   │                    │   PTY Slave    │
  │            │                   │                    │───────────────>│
  │            │                   │                    │    stdin       │
  │            │                   │                    │                │ Shell 执行 ls
  │            │                   │                    │                │
  │ <──────────│<──────────────────│<───────────────────│<───────────────│
  │  屏幕显示   │  (反向io.Copy链路)  │                    │  stdout→Slave  │
```

---

## 6. -it 模式的退出机制

### 场景一：用户在容器内输入 `exit` 或 `Ctrl+D`（容器进程主动退出）

触发源是**容器进程退出**，信号从 shim 向外传播：

#### 第 1 步：Shim 感知容器退出

```go
// shim.go
go func() {
    exitOnce.Do(func() {
        exitCode := waitForContainerExit(containerPID)  // Wait4 系统调用
        close(exitReady)   // 广播：容器退出了！
    })
}()
```

#### 第 2 步：Shim 的 attach goroutine 感知退出

attach 的 `select` 检测到 `exitReady`。同时，容器退出后 PTY Slave 端所有 FD 关闭，内核在 Master 端产生 EOF，两个 `io.Copy` goroutine 结束。`handleShimConn` 返回，`defer conn.Close()` 关闭与 daemon 的连接。

#### 第 3 步：Daemon 的 relayStream 感知连接断开

shim 关闭连接后，`io.Copy(daemonConn, shimConn)` 读到 EOF，`done` channel 关闭，`relayStream` 返回，`defer daemonConn.Close()` 关闭与 CLI 的连接。

#### 第 4 步：CLI 感知连接断开

```go
// main.go
<-stdoutDone   // io.Copy(os.Stdout, conn) 读到 EOF → 解除阻塞
conn.Close()
term.Restore()  // 终端恢复正常模式
```

#### 第 5 步：Daemon 异步清理容器资源

`WatchContainer` goroutine 轮询检测到容器退出 → `cleanupExitedContainer` → 清理网络/cgroup → `ShutdownShim` → shim 进程退出。

### 场景二：用户在宿主机终端按 `Ctrl+C`

**Raw 模式下 Ctrl+C 不会被终端驱动拦截**，而是作为原始字节 `0x03` 传给容器：

```
Ctrl+C → 0x03 字节 → CLI → daemon → shim → PTY Master → Slave → 容器
  → 容器 Shell 从 stdin 读到 0x03
  → 内核终端驱动检测到 INTR 字符
  → 向前台进程组发送 SIGINT
  → Shell 处理 SIGINT（通常中断当前子命令，Shell 本身不退出）
```

PTY Slave 被设置为容器的**控制终端**（`Setctty=true` + `Setsid=true`），所以内核会将 Ctrl+C 转为 SIGINT 发送给容器内的前台进程组。

### 退出对比表

| 操作 | 容器内发生什么 | PID=1 退出吗？ |
|------|---------------|----------------|
| 输入 `exit` | Shell 执行内置命令 `exit()`，主动退出 | ✅ 直接退出 |
| 输入 `Ctrl+D` | stdin EOF → Shell 检测到输入结束 → 自动退出 | ✅ 直接退出 |
| 输入 `Ctrl+C`（Shell 空闲） | 终端驱动发送 SIGINT → Shell 通常忽略 | ❌ 多数 Shell 不退出 |
| 输入 `Ctrl+C`（有子命令运行） | 终端驱动发送 SIGINT → 子命令被杀死 | ❌ Shell 继续，只有子命令死 |

---

## 7. 容器进程就是用户指定的 cmd

### execve 进程替换

```go
// container.go - HandleOCIInit
args := ociSpec.Process.Args          // 用户指定的命令，如 ["/bin/sh"]
execPath := args[0]                   // "/bin/sh"
syscallExec(execPath, args, env)      // execve 系统调用
```

`execve` 的效果：

```
execve 之前:  PID=1 → mini-docker init (Go 程序)
execve 之后:  PID=1 → /bin/sh          (进程还是那个进程，只是执行的程序变了)
```

PID 不变、FD 不变、namespace 不变，只是把进程的代码段、数据段、堆栈等全部替换成了用户指定程序的代码。

所以：
- `mini-docker run -it ubuntu /bin/sh` → 容器 PID=1 就是 `/bin/sh`
- `mini-docker run -it ubuntu /bin/bash` → 容器 PID=1 就是 `/bin/bash`
- `mini-docker run -it ubuntu python3` → 容器 PID=1 就是 `python3`

### exit 命令的本质

`exit` 不是什么"信号"或"外部杀进程"，而是**通过 stdin 传给容器进程的普通文本**。整条链路没有任何组件理解 `exit` 的含义——CLI 不知道、daemon 不知道、shim 不知道、PTY 也不知道。它们只是原封不动地搬运字节。**只有最终接收者 `/bin/sh` 知道这是它的退出命令。**

```
用户键盘输入 "exit\n"
    │
    ▼  (Raw 模式：原始字节流)
CLI:    io.Copy(conn, os.Stdin)       → "exit\n" 写入 conn
    ▼
Daemon: io.Copy(shimConn, daemonConn) → "exit\n" 原样转发
    ▼
Shim:   io.Copy(Master, conn)         → "exit\n" 写入 PTY Master
    ▼  (内核 PTY 中转)
PTY Slave (stdin)                     → /bin/sh 的 stdin 读到 "exit\n"
    ▼
/bin/sh 内部:
    1. readline() 返回 "exit\n"
    2. 解析器识别：这是内置命令 exit
    3. 执行 exit() → 进程终止
```

### 对比外部命令

| 命令类型 | 执行方式 | 进程变化 |
|---------|---------|---------|
| `ls` | fork + execve `/bin/ls` | Shell fork 出子进程(PID=N), Shell 等待子进程退出 |
| `sleep 100` | fork + execve `/bin/sleep` | Shell fork 出子进程(PID=N), Shell 等待 |
| **`exit`** | Shell 内部直接调用 `exit()` | **PID=1 本身终止**, 没有 fork |
| `Ctrl+D` | stdin EOF → Shell 检测到 → Shell 调用 `exit()` | **PID=1 本身终止**, 没有 fork |

### 类比：如果容器进程不是 Shell

假如用户执行 `mini-docker run -it ubuntu python3 -i`，此时 PID=1 是 `python3`。用户输入 `exit()`，Python 解释器从 stdin 读到 `exit()`，解释为 Python 的退出函数，Python 进程退出，容器退出。

如果用户输入 `exit`（不是 Python 语法），Python 会报 `NameError`，容器不会退出——**因为 `exit` 只是普通文本，只有接收它的程序才决定如何处理**。

---

## 8. PID=1 退出后的完整连锁反应

```
PID=1 (/bin/sh) 调用 exit() 终止
  │
  ├─ 所有 FD 关闭 → PTY Slave fd 关闭
  │
  ▼
内核: PTY Slave 最后一个引用关闭 → Master 端产生 EOF
  │
  ▼
shim: io.Copy(conn, Master) 读到 EOF → goroutine 结束
  │
shim: io.Copy(Master, conn) 写入失败 → goroutine 结束
  │
  ▼
shim: handleShimConn 返回 → defer conn.Close()
  │
  ▼
daemon: relayStream 读到 EOF → daemonConn.Close()
  │
  ▼
CLI: io.Copy(os.Stdout, conn) 读到 EOF → close(stdoutDone)
  │
  ▼
CLI: <-stdoutDone 解除阻塞 → term.Restore() → 回到宿主机 Shell
  │
  ▼
daemon: WatchContainer 检测退出 → cleanupExitedContainer → ShutdownShim
  │
  ▼
shim 进程退出
```

---

## 9. 架构总览

```
┌─────────┐    Unix Socket    ┌─────────┐    Unix Socket    ┌─────────┐    PTY    ┌─────────┐
│   CLI   │◄════════════════►│  Daemon  │◄════════════════►│   Shim  │◄════════►│  容器    │
│         │    (conn)        │         │    (shimConn)     │         │  Master/ │  进程   │
│ os.Stdin│                  │relayStream│                  │io.Copy  │  Slave   │ PID=1   │
│ os.Stdout│                 │ (中继)   │                  │(双向)   │          │(/bin/sh)│
└─────────┘                  └─────────┘                  └─────────┘          └─────────┘

数据流方向:
  用户输入: 键盘 → CLI → conn → daemonConn → shimConn → PTY Master → Slave → 容器stdin
  容器输出: 容器stdout → Slave → PTY Master → shimConn → daemonConn → conn → CLI → 屏幕
```
