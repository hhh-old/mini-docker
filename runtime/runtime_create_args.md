# runtime create 参数说明

## 概述

`runtime create` 是 mini-docker 中创建容器环境的核心命令，对标 OCI 运行时规范中的 `runc create`。它负责创建容器的 namespace、rootfs 等隔离环境，但不启动用户进程。

## 调用方式

```bash
/proc/self/exe runtime create <containerID> --bundle <path> [选项]
```

由 shim 进程通过 `exec.Command("/proc/self/exe", createArgs...)` 调用。

## 参数列表

| 参数 | 必选 | 说明 |
|------|------|------|
| `containerID` | ✅ | 容器唯一标识符 |
| `--bundle <path>` | ✅ | OCI bundle 目录路径（包含 config.json 和 rootfs） |
| `--console <path>` | ❌ | PTY 设备路径（TTY 模式时使用） |
| `--stdout-fd <n>` | ❌ | stdout 管道的文件描述符（非 TTY 模式时使用） |
| `--stderr-fd <n>` | ❌ | stderr 管道的文件描述符（非 TTY 模式时使用） |

## 实际调用示例

### TTY 模式 (`-it`)

```bash
/proc/self/exe runtime create abc123 \
  --bundle /var/lib/mini-docker/runtime/abc123/bundle \
  --console /dev/pts/0
```

### 非 TTY 模式 (`-d`)

```bash
/proc/self/exe runtime create abc123 \
  --bundle /var/lib/mini-docker/runtime/abc123/bundle \
  --stdout-fd 3 \
  --stderr-fd 4
```

## 参数来源

参数由 shim 进程在 [shim.go:130-179](../shim/shim.go#L130-L179) 中构建：

```go
createArgs := []string{"runtime", "create", containerID, "--bundle", bundlePath}

if isTTY && containerPTY != nil {
    createArgs = append(createArgs, "--console", containerPTY.Name)
}

if !isTTY {
    createArgs = append(createArgs,
        "--stdout-fd", fmt.Sprintf("%d", 3),
        "--stderr-fd", fmt.Sprintf("%d", 4))
}
```

## 文件描述符传递机制

### TTY 模式

使用 PTY（伪终端）实现交互式 I/O：

```
shim 进程                        runtime create 子进程
    │                                  │
    └─ containerPTY.Master ◄──────► containerPTY.Slave
          (主设备)                        (从设备)
```

- shim 关闭 Slave 端引用，确保容器退出时 Master 端能收到 EOF
- runtime 子进程重新打开 Slave 设备并分配给容器 init 进程

### 非 TTY 模式

使用 os.Pipe 管道捕获容器输出：

```
shim 进程                        runtime create 子进程
    │                                  │
    ├─ stdoutPipeW (写端) ──────► ExtraFiles[0] → FD 3
    ├─ stderrPipeW (写端) ──────► ExtraFiles[1] → FD 4
    ├─ stdoutPipeR (读端) ← 自己保留，读取容器输出
    └─ stderrPipeR (读端) ← 自己保留，读取容器输出
```

`ExtraFiles` 的索引从 3 开始（0=stdin, 1=stdout, 2=stderr），所以 `--stdout-fd 3` 和 `--stderr-fd 4` 对应管道的写端。

### 数据流向

```
容器内进程 ──(输出)──> stdout (FD 1)
                       │ (Runtime 接线重定向)
                       ▼
                 stdoutPipeW (FD 3)
                       │
             (操作系统管道，数据穿过 Namespace 屏障)
                       │
                       ▼
                 stdoutPipeR (读端)
                       │
                writeLogStream() ──(序列化为JSON)──> container.log 文件
```

## Create 函数执行流程

详见 [runtime.go:34-88](runtime.go#L34-L88)：

```
1. 解析参数（containerID, bundlePath, consolePath, stdoutFd, stderrFd）
2. 加载 OCI Spec（config.json）
3. 转换为 libcontainer 配置
4. 创建 Container 实例
5. 创建 Process 对象（重定向 stdin/stdout/stderr）
6. 调用 container.Start(process)
7. 输出容器 PID
8. 退出
```

## 相关代码

| 文件 | 函数 | 说明 |
|------|------|------|
| [shim.go](../shim/shim.go) | `run()` | 构建 createArgs 并调用 runtime create |
| [runtime.go](runtime.go) | `Create()` | 解析参数并执行容器创建 |
| [linux_container.go](../libcontainer/linux_container.go) | `Start()` | fork init 进程，设置 namespace |
| [linux_init.go](../libcontainer/linux_init.go) | `InitProcess()` | 容器内 init 进程的初始化逻辑 |
