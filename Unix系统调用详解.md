# Unix 系统调用详解

本文档详细解释 mini-docker 项目中用到的各种 Unix 系统调用命令的参数及其作用。

## 1. 文件系统相关系统调用

### 1.1 unix.Mount - 挂载文件系统

**函数签名：**
```go
func Mount(source string, target string, fstype string, flags uintptr, data string) error
```

**参数说明：**
- `source`：要挂载的设备或文件系统（如 "overlay"、"proc"、"tmpfs"）
- `target`：挂载点目录（如 "/proc"、"/tmp"）
- `fstype`：文件系统类型（如 "overlay"、"proc"、"tmpfs"、"bind"）
- `flags`：挂载标志，可以组合使用
- `data`：文件系统特定的选项字符串

**常用标志：**
```go
unix.MS_BIND       // 绑定挂载（将一个目录挂载到另一个位置）
unix.MS_REC        // 递归挂载（包括所有子挂载点）
unix.MS_PRIVATE    // 私有挂载（挂载事件不传播）
unix.MS_NOSUID     // 禁止 setuid/setgid 位生效
unix.MS_NODEV      // 禁止访问设备文件
```

**项目中的使用：**

```go
// 1. 重新挂载根目录为私有（防止挂载事件传播到宿主机）
unix.Mount("", "/", "", unix.MS_PRIVATE|unix.MS_REC, "")

// 2. 挂载 OverlayFS（联合文件系统）
options := "lowerdir=<镜像路径>,upperdir=<可写层>,workdir=<工作目录>"
unix.Mount("overlay", mergedDir, "overlay", 0, options)

// 3. 绑定挂载 rootfs
unix.Mount(rootFSPath, rootFSPath, "bind", unix.MS_BIND|unix.MS_REC, "")

// 4. 挂载 /proc 文件系统
unix.Mount("proc", "/proc", "proc", 0, "")

// 5. 挂载 /tmp 为 tmpfs
unix.Mount("tmpfs", "/tmp", "tmpfs", unix.MS_NOSUID|unix.MS_NODEV, "size=64m")
```

**各标志的含义：**
- `MS_BIND`：创建绑定挂载，使两个目录指向同一文件系统
- `MS_REC`：递归应用挂载操作到所有子挂载点
- `MS_PRIVATE`：设置为私有挂载，挂载事件不会传播到其他命名空间
- `MS_NOSUID`：禁止在该文件系统上执行 setuid/setgid 程序
- `MS_NODEV`：禁止访问该文件系统上的设备文件

---

### 1.2 unix.Unmount - 卸载文件系统

**函数签名：**
```go
func Unmount(target string, flags int) error
```

**参数说明：**
- `target`：要卸载的挂载点
- `flags`：卸载标志

**常用标志：**
```go
unix.MNT_DETACH  // 懒卸载（立即从文件系统层级中分离，但实际卸载延迟到没有进程使用时）
```

**项目中的使用：**
```go
// 卸载旧的根目录（在 pivot_root 之后）
unix.Unmount(putOldDir, unix.MNT_DETACH)
```

**MNT_DETACH 的作用：**
- 立即将挂载点从文件系统层级中分离
- 如果有进程仍在使用该挂载点，不会立即卸载
- 当所有进程停止使用后，自动完成卸载
- 避免因忙碌而导致卸载失败

---

### 1.3 unix.PivotRoot - 切换根目录

**函数签名：**
```go
func PivotRoot(newRoot string, putOld string) error
```

**参数说明：**
- `newRoot`：新的根目录路径
- `putOld`：旧根目录的存放位置（相对于新根目录）

**项目中的使用：**
```go
// 切换根目录到容器的 rootfs
pivotDir := filepath.Join(targetPath, ".pivot_root")
os.MkdirAll(pivotDir, 0755)
unix.PivotRoot(targetPath, pivotDir)

// 切换到新根目录
unix.Chdir("/")

// 卸载并删除旧根目录
putOldDir := "/", filepath.Base(pivotDir)
unix.Unmount(putOldDir, unix.MNT_DETACH)
os.RemoveAll(putOldDir)
```

**工作原理：**
1. 将当前根目录移动到 `newRoot/putOld`
2. 将 `newRoot` 设为新的根目录
3. 需要后续调用 `chdir("/")` 切换到新根
4. 最后卸载并删除 `putOld` 目录

---

### 1.4 unix.Chdir - 切换工作目录

**函数签名：**
```go
func Chdir(path string) error
```

**参数说明：**
- `path`：要切换到的目录路径

**项目中的使用：**
```go
// 在 pivot_root 后切换到新的根目录
unix.Chdir("/")
```

**作用：**
- 改变当前进程的工作目录
- 在 pivot_root 后必须调用，确保进程在新的根目录中

---

## 2. 命名空间相关系统调用

### 2.1 克隆标志（CLONE_*）

**函数签名：**
```go
const (
    CLONE_NEWUTS  = unix.CLONE_NEWUTS
    CLONE_NEWIPC  = unix.CLONE_NEWIPC
    CLONE_NEWPID  = unix.CLONE_NEWPID
    CLONE_NEWNS   = unix.CLONE_NEWNS
    CLONE_NEWNET  = unix.CLONE_NEWNET
    CLONE_NEWUSER = unix.CLONE_NEWUSER
)
```

**各标志的作用：**

| 标志 | 作用 | 隔离内容 |
|------|------|----------|
| `CLONE_NEWUTS` | 隔离主机名 | 容器可以有自己的 hostname |
| `CLONE_NEWIPC` | 隔离进程间通信 | 共享内存、信号量、消息队列独立 |
| `CLONE_NEWPID` | 隔离进程 ID | 容器内 PID 从 1 开始 |
| `CLONE_NEWNS` | 隔离挂载点 | 容器有独立的文件系统视图 |
| `CLONE_NEWNET` | 隔离网络 | 容器有独立的网络栈（网卡、IP、路由） |
| `CLONE_NEWUSER` | 隔离用户 | 容器可以有独立的用户/组 ID 映射 |

**项目中的使用：**
```go
// 创建包含所有隔离标志的组合
func NewNamespaceFlags() uintptr {
    return uintptr(
        unix.CLONE_NEWUTS |
        unix.CLONE_NEWIPC |
        unix.CLONE_NEWPID |
        unix.CLONE_NEWNS |
        unix.CLONE_NEWNET,
    )
}

// 在创建子进程时应用这些标志
cmd.SysProcAttr = &syscall.SysProcAttr{
    Cloneflags: flags,  // fork() 时创建新的 namespace
}
```

---

### 2.2 unix.Open - 打开文件

**函数签名：**
```go
func Open(path string, mode int, perm uint32) (fd int, err error)
```

**参数说明：**
- `path`：文件路径
- `mode`：打开模式（只读、只写、读写等）
- `perm`：文件权限（当创建新文件时）

**常用模式：**
```go
unix.O_RDONLY  // 只读模式
unix.O_WRONLY  // 只写模式
unix.O_RDWR   // 读写模式
unix.O_CREAT   // 如果文件不存在则创建
```

**项目中的使用：**
```go
// 打开 namespace 文件（只读）
fd, err := unix.Open(nsPath, unix.O_RDONLY, 0)
```

---

### 2.3 unix.Close - 关闭文件描述符

**函数签名：**
```go
func Close(fd int) error
```

**参数说明：**
- `fd`：要关闭的文件描述符

**项目中的使用：**
```go
// 关闭 namespace 文件描述符
defer unix.Close(fd)
```

---

### 2.4 unix.Setns - 加入命名空间

**函数签名：**
```go
func Setns(fd int, nstype int) error
```

**参数说明：**
- `fd`：指向 namespace 的文件描述符（通常从 `/proc/<pid>/ns/<ns>` 打开）
- `nstype`：namespace 类型（0 表示任意类型）

**项目中的使用：**
```go
// 加入指定的 namespace
func SetNamespace(nsPath string) error {
    // 打开 namespace 文件
    fd, err := unix.Open(nsPath, unix.O_RDONLY, 0)
    if err != nil {
        return fmt.Errorf("打开 namespace 文件失败 %s: %w", nsPath, err)
    }
    defer unix.Close(fd)

    // 加入 namespace
    if err := unix.Setns(fd, 0); err != nil {
        return fmt.Errorf("setns 失败: %w", err)
    }

    return nil
}
```

**工作原理：**
1. 从 `/proc/<pid>/ns/<namespace>` 打开 namespace 文件
2. 获取指向该 namespace 的文件描述符
3. 调用 `setns()` 将当前进程加入该 namespace
4. 常用于 `docker exec` 进入已运行容器的场景

---

## 3. 进程控制相关系统调用

### 3.1 unix.Sethostname - 设置主机名

**函数签名：**
```go
func Sethostname(p []byte) error
```

**参数说明：**
- `p`：主机名的字节数组

**项目中的使用：**
```go
// 设置容器主机名
func setHostname(name string) error {
    return unix.Sethostname([]byte(name))
}

// 在 InitProcess 中调用
setHostname("mini-docker")
```

**作用：**
- 设置当前进程所在 UTS namespace 的主机名
- 需要 `CLONE_NEWUTS` 标志才能隔离主机名

---

### 3.2 unix.Kill - 发送信号

**函数签名：**
```go
func Kill(pid int, sig syscall.Signal) error
```

**参数说明：**
- `pid`：目标进程 ID
- `sig`：要发送的信号

**常用信号：**
```go
syscall.SIGTERM  // 15 - 终止信号（优雅停止）
syscall.SIGKILL  // 9 - 强制终止信号（无法捕获）
syscall.Signal(0) // 0 - 检查进程是否存在（不发送信号）
```

**项目中的使用：**
```go
// 发送 SIGTERM 优雅停止容器
func sendSignal(pid int, sig int) error {
    return unix.Kill(pid, syscall.Signal(sig))
}

// 检查进程是否存活
func checkProcessAlive(pid int) error {
    process, err := os.FindProcess(pid)
    if err != nil {
        return err
    }
    return process.Signal(syscall.Signal(0))
}

// 停止容器流程
func Stop(containerID string) error {
    // 1. 发送 SIGTERM
    sendSignal(containerInfo.Pid, 15)
    time.Sleep(2 * time.Second)

    // 2. 检查是否还在运行
    if err := checkProcessAlive(containerInfo.Pid); err == nil {
        // 3. 如果还在运行，发送 SIGKILL 强制终止
        sendSignal(containerInfo.Pid, 9)
    }
    return nil
}
```

**信号 0 的特殊用途：**
- 不实际发送信号
- 用于检查进程是否存在
- 如果进程存在，返回 nil
- 如果进程不存在，返回错误

---

## 4. 系统调用在容器创建流程中的应用

### 4.1 完整流程示例

```go
func SetupRootFS(rootFSPath string, overlay *OverlayDirs) error {
    // 1. 重新挂载根目录为私有
    unix.Mount("", "/", "", unix.MS_PRIVATE|unix.MS_REC, "")

    // 2. 挂载 OverlayFS
    unix.Mount("overlay", mergedDir, "overlay", 0, options)

    // 3. 创建 pivot_root 目录
    pivotDir := filepath.Join(targetPath, ".pivot_root")
    os.MkdirAll(pivotDir, 0755)

    // 4. 切换根目录
    unix.PivotRoot(targetPath, pivotDir)
    unix.Chdir("/")

    // 5. 卸载旧根目录
    putOldDir := filepath.Join("/", filepath.Base(pivotDir))
    unix.Unmount(putOldDir, unix.MNT_DETACH)
    os.RemoveAll(putOldDir)

    // 6. 挂载 /proc
    unix.Mount("proc", "/proc", "proc", 0, "")

    // 7. 挂载 /tmp
    unix.Mount("tmpfs", "/tmp", "tmpfs", unix.MS_NOSUID|unix.MS_NODEV, "size=64m")

    return nil
}
```

### 4.2 流程图

```
容器创建流程
│
├── 1. 创建子进程（带 namespace 标志）
│   └── fork() + CLONE_NEW* 标志 → 新的 namespace
│
├── 2. 设置根文件系统
│   ├── Mount("", "/", "", MS_PRIVATE|MS_REC)  // 私有化根挂载点
│   ├── Mount("overlay", merged, "overlay")     // 挂载联合文件系统
│   ├── PivotRoot(newRoot, pivotDir)            // 切换根目录
│   ├── Chdir("/")                              // 切换工作目录
│   └── Unmount(putOld, MNT_DETACH)            // 卸载旧根
│
├── 3. 挂载必要的文件系统
│   ├── Mount("proc", "/proc", "proc")          // 进程信息
│   └── Mount("tmpfs", "/tmp", "tmpfs")         // 临时文件
│
├── 4. 设置系统参数
│   └── Sethostname("mini-docker")              // 设置主机名
│
└── 5. 执行用户命令
    └── Exec("/bin/sh")                         // 替换当前进程
```

---

## 5. 关键概念总结

### 5.1 挂载传播类型
- **MS_PRIVATE**：私有挂载，挂载事件不传播
- **MS_SHARED**：共享挂载，挂载事件双向传播
- **MS_SLAVE**：从属挂载，单向接收主挂载的事件

### 5.2 Namespace 隔离
- **UTS**：主机名隔离
- **IPC**：进程间通信隔离
- **PID**：进程 ID 隔离
- **Mount**：文件系统挂载点隔离
- **Network**：网络栈隔离
- **User**：用户/组 ID 隔离

### 5.3 信号处理
- **SIGTERM (15)**：优雅终止，进程可以捕获并清理
- **SIGKILL (9)**：强制终止，进程无法捕获
- **Signal(0)**：检查进程是否存在，不发送信号

### 5.4 文件系统操作
- **Bind Mount**：将一个目录挂载到另一个位置
- **OverlayFS**：联合文件系统，实现写时复制
- **PivotRoot**：切换根目录，实现文件系统隔离
