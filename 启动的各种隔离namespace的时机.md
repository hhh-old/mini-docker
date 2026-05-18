# 启动的各种隔离 Namespace 的时机

## 问题

在 mini-docker 中，到底是在执行哪个命令的时候启动的各种隔离 namespace？

## 答案

**在执行 `cmd.Start()` 时**（`container.go` 第 110 行），各种 namespace 隔离就被创建了。

## 具体执行顺序

```go
// container.go 第 93 行
cmd := exec.Command("/proc/self/exe", append([]string{"init"}, rootFSPath)...)

// 第 106-108 行：设置 namespace 标志
nsFlags := NewNamespaceFlags()  // 返回 CLONE_NEWUTS|CLONE_NEWIPC|CLONE_NEWPID|CLONE_NEWNS|CLONE_NEWNET
setCloneFlags(cmd, nsFlags, config.Tty)  // 把标志设置到 cmd.SysProcAttr.Cloneflags

// 第 110 行：启动子进程
if err := cmd.Start(); err != nil {  // ← 这里创建 namespace！
    ...
}
```

## 为什么是 `cmd.Start()` 时？

`cmd.Start()` 底层会执行以下系统调用序列：

```
1. fork()    ← 在这里，根据 Cloneflags 创建新的 namespace
2. exec()    ← 执行 /proc/self/exe（即 mini-docker 自身）
```

具体来说，Go 的 `exec.Cmd.Start()` 内部会调用 `fork()`，而 `fork()` 会检查 `SysProcAttr.Cloneflags`：

```go
// namespace_linux.go 第 69-78 行
func setCloneFlags(cmd *exec.Cmd, flags uintptr, tty bool) {
    cmd.SysProcAttr = &syscall.SysProcAttr{
        Cloneflags: flags,  // ← 这个标志会在 fork() 时生效
    }
    if tty {
        cmd.SysProcAttr.Setctty = true
        cmd.SysProcAttr.Setsid = true
    }
}
```

当 `fork()` 看到 `Cloneflags` 中包含 `CLONE_NEWUTS`、`CLONE_NEWIPC` 等标志时，会在新的 namespace 中创建子进程。

## Namespace 标志定义

```go
// namespace_linux.go 第 29-37 行
func NewNamespaceFlags() uintptr {
    return uintptr(
        unix.CLONE_NEWUTS |   // 隔离主机名
        unix.CLONE_NEWIPC |   // 隔离进程间通信
        unix.CLONE_NEWPID |   // 隔离进程 ID（容器内 PID 从 1 开始）
        unix.CLONE_NEWNS  |   // 隔离文件系统挂载点
        unix.CLONE_NEWNET,    // 隔离网络
    )
}
```

## 执行时间线

```
mini-docker run -it myos /bin/sh
│
├── 1. main() → runCommand() → container.Run()
│
├── 2. 创建 cmd 对象
│   cmd := exec.Command("/proc/self/exe", "init", rootFSPath, "/bin/sh")
│
├── 3. 设置 namespace 标志
│   cmd.SysProcAttr.Cloneflags = CLONE_NEWUTS|CLONE_NEWIPC|CLONE_NEWPID|CLONE_NEWNS|CLONE_NEWNET
│
├── 4. cmd.Start()  ← 在这里创建新的 namespace！
│   │
│   │   fork() 系统调用：
│   │   ├─ 检查 Cloneflags
│   │   ├─ 创建新的 UTS namespace
│   │   ├─ 创建新的 IPC namespace
│   │   ├─ 创建新的 PID namespace（容器内 PID 从 1 开始）
│   │   ├─ 创建新的 Mount namespace
│   │   └─ 创建新的 Network namespace
│   │
│   └── exec() 系统调用：
│       └─ 执行 /proc/self/exe（mini-docker 自身）
│
└── 5. 子进程在新的 namespace 中运行
    └── HandleInit() → InitProcess() → SetupRootFS() → syscall.Exec("/bin/sh")
```

## 各 Namespace 的作用

| Namespace | 标志 | 作用 |
|-----------|------|------|
| UTS | `CLONE_NEWUTS` | 隔离主机名，容器可以有自己的 hostname |
| IPC | `CLONE_NEWIPC` | 隔离进程间通信，容器内的共享内存、信号量等独立 |
| PID | `CLONE_NEWPID` | 隔离进程 ID，容器内 PID 从 1 开始 |
| Mount | `CLONE_NEWNS` | 隔离文件系统挂载点，容器有独立的挂载视图 |
| Network | `CLONE_NEWNET` | 隔离网络，容器有独立的网络栈 |

## 关键点总结

1. **namespace 创建时机**：`cmd.Start()` 时的 `fork()` 系统调用
2. **namespace 隔离生效**：子进程从 `fork()` 返回后就已经在新的 namespace 中了
3. **命令执行**：之后的 `exec()` 和 `syscall.Exec("/bin/sh")` 都是在新的 namespace 中执行
4. **设计模式**：使用 `/proc/self/exe` 技巧让子进程重新执行自身，通过不同的参数（`init` vs `run`）走不同分支

## 相关代码位置

- `container/container.go` 第 93 行：创建命令对象
- `container/container.go` 第 106-108 行：设置 namespace 标志
- `container/container.go` 第 110 行：`cmd.Start()` 启动子进程
- `container/namespace_linux.go` 第 29-37 行：定义 namespace 标志
- `container/namespace_linux.go` 第 69-78 行：设置 Cloneflags 的函数
