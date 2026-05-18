# mini-docker — 从零理解 Docker 容器原理

> 一个用 Go 语言实现的迷你版 Docker，通过可运行的代码学习容器的底层原理。
> 容器的本质只有一句话：**容器 = Namespace（隔离）+ Cgroup（限制）+ RootFS（文件系统）**

---

## 目录

- [项目简介](#项目简介)
- [快速开始](#快速开始)
- [项目架构](#项目架构)
- [核心原理详解](#核心原理详解)
  - [1. Namespace — 进程隔离](#1-namespace--进程隔离)
  - [2. Cgroup — 资源限制](#2-cgroup--资源限制)
  - [3. RootFS — 文件系统隔离](#3-rootfs--文件系统隔离)
  - [4. 网络 — 容器通信](#4-网络--容器通信)
  - [5. 镜像 — 分层与分发](#5-镜像--分层与分发)
- [容器创建全流程](#容器创建全流程)
- [命令使用指南](#命令使用指南)
- [代码导读](#代码导读)
- [与真实 Docker 的差距](#与真实-docker-的差距)
- [学习路线建议](#学习路线建议)
- [常见问题](#常见问题)

---

## 项目简介

### 为什么写这个项目？

Docker 看起来像魔法，但它的底层原理并不复杂。这个项目剥离了 Docker 的工程复杂性，只保留最核心的容器技术，让你能直接看到：

- `docker run` 背后到底发生了什么
- 容器为什么能隔离进程、网络、文件系统
- `-m 100m` 是如何限制内存的
- 容器网络是怎么互通的

### 容器 vs 虚拟机

```
┌──────────────────────────────────────────────────────────────┐
│                      虚拟机 (VM)                              │
│  ┌──────────┐  ┌──────────┐  ┌──────────┐                   │
│  │  App A   │  │  App B   │  │  App C   │                   │
│  │ Bins/Libs│  │ Bins/Libs│  │ Bins/Libs│                   │
│  │ Guest OS │  │ Guest OS │  │ Guest OS │  ← 每个都有完整OS  │
│  └────┬─────┘  └────┬─────┘  └────┬─────┘                   │
│       └──────────────┼──────────────┘                        │
│                Hypervisor                                    │
│                Host OS                                       │
│                Infrastructure                                │
└──────────────────────────────────────────────────────────────┘

┌──────────────────────────────────────────────────────────────┐
│                      容器 (Container)                         │
│  ┌──────────┐  ┌──────────┐  ┌──────────┐                   │
│  │  App A   │  │  App B   │  │  App C   │                   │
│  │ Bins/Libs│  │ Bins/Libs│  │ Bins/Libs│  ← 只打包应用依赖  │
│  └────┬─────┘  └────┬─────┘  └────┬─────┘                   │
│       └──────────────┼──────────────┘                        │
│           Container Runtime (Namespace + Cgroup)             │
│                Host OS Kernel  ← 共享内核，无 Guest OS        │
│                Infrastructure                                 │
└──────────────────────────────────────────────────────────────┘
```

| 对比项 | Docker 容器 | 虚拟机 |
|--------|------------|--------|
| 隔离方式 | Namespace（进程级） | Hypervisor（硬件级） |
| 资源限制 | Cgroup | 虚拟硬件 |
| 内核 | 共享宿主机内核 | 独立内核 |
| 启动速度 | 毫秒级 | 分钟级 |
| 镜像大小 | MB 级 | GB 级 |
| 性能损耗 | 接近原生 | 5-10% |

**关键认知：容器本质上是"特殊的进程"，不是"轻量级虚拟机"。容器内的进程直接运行在宿主机内核上，只是被 Namespace 隔离了。**

---

## 快速开始

### 环境要求

- **操作系统**：Linux（推荐 Ubuntu 20.04+），或 WSL2
- **Go 版本**：1.22+
- **权限**：需要 root 权限运行（容器操作需要操作 namespace/cgroup/mount）

> ⚠️ 本项目使用 Linux 内核特性（Namespace、Cgroup、pivot_root 等），**必须在 Linux 环境下运行**。Windows/macOS 上只能编译，无法实际运行容器。

### 编译

```bash
# 克隆项目
cd mini-docker

# 编译（当前平台）
go build -o mini-docker .

# 交叉编译 Linux 版本（在 Windows/macOS 上）
GOOS=linux GOARCH=amd64 go build -o mini-docker .
```

### 运行第一个容器

```bash
# 1. 创建镜像（生成最小 rootfs 目录结构）
sudo ./mini-docker pull myos

# 2. 准备 busybox（提供基础命令：sh, ls, cat 等）
#    将 busybox 复制到镜像的 rootfs 中
sudo cp /bin/busybox /var/lib/mini-docker/images/myos/rootfs/bin/
sudo chroot /var/lib/mini-docker/images/myos/rootfs /bin/busybox --install -s /bin

# 3. 创建网络
sudo ./mini-docker network create mynet

# 4. 运行容器！
sudo ./mini-docker run -it myos /bin/sh

# 5. 在容器内验证隔离效果
hostname          # 显示 mini-docker（UTS Namespace 隔离）
ps aux            # 只能看到容器内进程（PID Namespace 隔离）
ip addr           # 独立的网络栈（Network Namespace 隔离）
ls /              # 独立的文件系统（Mount Namespace + RootFS）
```

### 资源限制示例

```bash
# 限制内存 100MB，CPU 份额 512
sudo ./mini-docker run -m 100m -c 512 -it myos /bin/sh

# 在容器内验证
cat /proc/meminfo   # 查看内存信息
dd if=/dev/zero bs=1M count=200  # 尝试分配 200MB 内存，会被 OOM Kill
```

---

## 项目架构

### 代码结构

```
mini-docker/
├── main.go                              # 入口：CLI 解析 + reexec 机制
│
├── container/                           # 容器核心模块
│   ├── doc.go                           # 📖 原理文档：Docker 核心架构
│   ├── container.go                     # 容器生命周期：run/stop/exec/ps/rm
│   ├── namespace_linux.go               # Namespace 隔离（Linux 实现）
│   ├── namespace_other.go               # 非 Linux 平台 stub
│   ├── rootfs_linux.go                  # RootFS 隔离：pivot_root + OverlayFS
│   └── rootfs_other.go                  # 非 Linux 平台 stub
│
├── cgroup/                              # Cgroup 资源限制模块
│   ├── doc.go                           # 📖 原理文档：Cgroup 机制
│   ├── cgroup_linux.go                  # Cgroup v1：内存/CPU/Freezer
│   └── cgroup_other.go                  # 非 Linux 平台 stub
│
├── image/                               # 镜像管理模块
│   ├── doc.go                           # 📖 原理文档：镜像分层
│   └── image.go                         # 镜像操作：pull/images
│
├── network/                             # 网络管理模块
│   ├── doc.go                           # 📖 原理文档：容器网络
│   ├── network_linux.go                 # 网络实现：Bridge + veth + NAT
│   └── network_other.go                 # 非 Linux 平台 stub
│
├── go.mod
└── go.sum
```

### Docker 架构对照

```
┌─────────────────────────────────────────────────────────────────┐
│                    Docker 真实架构                                │
│                                                                 │
│  docker CLI → dockerd → containerd → containerd-shim → runc    │
│       ↑                                                         │
│       └── 这一层层组件的最终目的，就是调用下面三个内核特性         │
│                                                                 │
│  ┌──────────┐  ┌──────────┐  ┌──────────┐                      │
│  │Namespace │  │  Cgroup  │  │  RootFS  │   ← Linux 内核特性    │
│  └──────────┘  └──────────┘  └──────────┘                      │
└─────────────────────────────────────────────────────────────────┘

┌─────────────────────────────────────────────────────────────────┐
│                    mini-docker 架构                               │
│                                                                 │
│  mini-docker CLI → main.go → container.Run()                   │
│                            ↓                                    │
│  ┌──────────┐  ┌──────────┐  ┌──────────┐                      │
│  │Namespace │  │  Cgroup  │  │  RootFS  │   ← 同样的内核特性    │
│  └──────────┘  └──────────┘  └──────────┘                      │
└─────────────────────────────────────────────────────────────────┘

区别：Docker 中间有 containerd/shim/runc 等多层抽象，
     mini-docker 直接调用内核接口，省略了工程化层。
     但底层技术完全相同！
```

---

## 核心原理详解

### 1. Namespace — 进程隔离

> 对应代码：`container/namespace_linux.go`

Namespace 是 Linux 内核提供的进程隔离机制。它让一个进程只能看到与自己相关的系统资源，产生"我拥有整个系统"的错觉。

#### 六种 Namespace

| Namespace | 隔离内容 | 系统调用参数 | 效果 |
|-----------|---------|-------------|------|
| **PID** | 进程 ID | `CLONE_NEWPID` | 容器内进程从 PID 1 开始，看不到宿主机进程 |
| **Mount** | 文件系统挂载点 | `CLONE_NEWNS` | 容器有独立的文件系统视图 |
| **UTS** | 主机名和域名 | `CLONE_NEWUTS` | 容器可以有自己的 hostname |
| **IPC** | 进程间通信 | `CLONE_NEWIPC` | 隔离信号量、消息队列、共享内存 |
| **Network** | 网络栈 | `CLONE_NEWNET` | 独立的 IP、端口、路由表 |
| **User** | 用户/用户组 ID | `CLONE_NEWUSER` | 容器内 root ≠ 宿主机 root |

#### 代码实现

```go
// container/namespace_linux.go

// 创建容器时，通过 Cloneflags 设置 Namespace
func NewNamespaceFlags() uintptr {
    return uintptr(
        unix.CLONE_NEWUTS |   // 主机名隔离
        unix.CLONE_NEWIPC |   // IPC 隔离
        unix.CLONE_NEWPID |   // PID 隔离
        unix.CLONE_NEWNS  |   // Mount 隔离
        unix.CLONE_NEWNET,    // 网络隔离
    )
}

// 设置到子进程的 SysProcAttr 中
func setCloneFlags(cmd *exec.Cmd, flags uintptr, tty bool) {
    cmd.SysProcAttr = &syscall.SysProcAttr{
        Cloneflags: flags,  // ← 这就是 Docker 创建容器的关键！
    }
}
```

#### 关键系统调用

```
┌──────────────────────────────────────────────────────────────┐
│  clone(flags=CLONE_NEWUTS|CLONE_NEWPID|...)                 │
│  → 创建新进程，同时创建新的 Namespace                        │
│  → 这就是 docker run 底层做的事                               │
├──────────────────────────────────────────────────────────────┤
│  setns(fd, 0)                                                │
│  → 将当前进程加入已有的 Namespace                             │
│  → 这就是 docker exec 底层做的事                              │
├──────────────────────────────────────────────────────────────┤
│  unshare(flags)                                              │
│  → 将当前进程移入新 Namespace（不创建新进程）                  │
│  → 这就是 docker pause 等操作可能用到的                       │
└──────────────────────────────────────────────────────────────┘
```

#### Namespace 的文件表示

每个 Namespace 在 `/proc/<pid>/ns/` 下都有对应的文件：

```
/proc/<pid>/ns/pid    → PID Namespace
/proc/<pid>/ns/mnt    → Mount Namespace
/proc/<pid>/ns/uts    → UTS Namespace
/proc/<pid>/ns/ipc    → IPC Namespace
/proc/<pid>/ns/net    → Network Namespace
/proc/<pid>/ns/user   → User Namespace
```

Docker 的 `docker exec` 就是通过打开这些文件，调用 `setns()` 将新进程加入容器的 Namespace。

#### 动手验证

```bash
# 运行一个容器
sudo ./mini-docker run -it myos /bin/sh

# 在另一个终端查看容器的 Namespace
sudo ls -la /proc/<容器PID>/ns/

# 对比宿主机进程的 Namespace
ls -la /proc/$$/ns/

# 你会发现它们的 Namespace 编号不同！
```

---

### 2. Cgroup — 资源限制

> 对应代码：`cgroup/cgroup_linux.go`

Namespace 解决了"能看到什么"的问题，Cgroup 解决了"能用多少"的问题。没有 Cgroup，一个容器可以耗尽宿主机所有资源。

#### Cgroup 核心概念

```
┌─────────────────────────────────────────────────────────────┐
│  Cgroup 层级树                                              │
  │
  ├── /sys/fs/cgroup/memory/mini-docker-xxx/                  │
  │   ├── cgroup.procs            ← 属于该组的进程 PID       │
  │   ├── memory.limit_in_bytes   ← 内存上限                 │
  │   └── memory.usage_in_bytes   ← 当前内存使用量           │
  │
  ├── /sys/fs/cgroup/cpu/mini-docker-xxx/                     │
  │   ├── cgroup.procs            ← 属于该组的进程 PID       │
  │   └── cpu.shares              ← CPU 份额权重             │
  │
  └── /sys/fs/cgroup/freezer/mini-docker-xxx/                 │
      ├── cgroup.procs            ← 属于该组的进程 PID       │
      └── freezer.state           ← FROZEN / THAWED          │
└─────────────────────────────────────────────────────────────┘
```

#### 内存限制

```go
// cgroup/cgroup_linux.go

func (c *CgroupManager) setMemoryLimit() error {
    // 1. 在 /sys/fs/cgroup/memory/ 下创建 cgroup 目录
    memoryCgroupPath := filepath.Join("/sys/fs/cgroup/memory", c.CgroupName)
    os.MkdirAll(memoryCgroupPath, 0755)

    // 2. 写入内存上限（字节）
    //    docker run -m 100m → 写入 104857600
    limitFile := filepath.Join(memoryCgroupPath, "memory.limit_in_bytes")
    os.WriteFile(limitFile, []byte("104857600"), 0644)

    return nil
}
```

**这就是 `docker run -m 100m` 的全部秘密！**

#### CPU 份额

```go
func (c *CgroupManager) setCpuShares() error {
    // cpu.shares 是相对权重，不是绝对限制
    // 默认值 1024，设为 512 表示获得一半的 CPU 时间
    sharesFile := filepath.Join(cpuCgroupPath, "cpu.shares")
    os.WriteFile(sharesFile, []byte("512"), 0644)
    return nil
}
```

**注意**：CPU shares 是相对值。两个 cgroup 的 shares 分别为 512 和 1024 时，它们按 1:2 分配 CPU。但如果只有一个在运行，它可以获得全部 CPU。

#### 冻结/恢复

```go
func (c *CgroupManager) Freeze() error {
    // 写入 "FROZEN" 暂停所有进程
    // 写入 "THAWED"  恢复所有进程
    // 这就是 docker pause/unpause 的实现！
    stateFile := filepath.Join(freezerCgroupPath, "freezer.state")
    os.WriteFile(stateFile, []byte("FROZEN"), 0644)
    return nil
}
```

#### Cgroup v1 vs v2

| 特性 | Cgroup v1 | Cgroup v2 |
|------|-----------|-----------|
| 目录结构 | 每个子系统独立挂载 | 统一层级 `/sys/fs/cgroup/` |
| 本项目支持 | ✅ 已实现 | ❌ 未实现 |
| Docker 默认 | 旧版本 | 新版本（20.10+） |
| 复杂度 | 灵活但复杂 | 简洁统一 |

#### 动手验证

```bash
# 运行一个限制内存的容器
sudo ./mini-docker run -m 100m -it myos /bin/sh

# 在另一个终端查看 cgroup 设置
sudo cat /sys/fs/cgroup/memory/mini-docker-*/memory.limit_in_bytes
# 输出: 104857600 (100MB)

# 在容器内尝试分配超过限制的内存
dd if=/dev/zero bs=1M count=200
# 进程会被 OOM Kill！
```

---

### 3. RootFS — 文件系统隔离

> 对应代码：`container/rootfs_linux.go`

RootFS 解决了"容器为什么看起来像独立操作系统"的问题。通过 Mount Namespace + `pivot_root`，容器进程的 `/` 指向一个独立的目录。

#### pivot_root vs chroot

```
┌──────────────────────────────────────────────────────────────┐
│  chroot (不安全)                                              │
│  - 仅改变进程对 "/" 的视角                                    │
│  - 不改变当前工作目录                                         │
│  - 可以通过 ".." 逃逸                                        │
│  - Docker 不使用 chroot                                      │
├──────────────────────────────────────────────────────────────┤
│  pivot_root (安全)                                            │
│  - 原子性地切换根目录                                         │
│  - 旧根被移动到新位置                                         │
│  - 无法通过路径逃逸                                           │
│  - Docker 默认使用 pivot_root                                │
└──────────────────────────────────────────────────────────────┘
```

#### 代码实现

```go
// container/rootfs_linux.go

func SetupRootFS(rootFSPath string) error {
    // 步骤 1: 将 / 重新挂载为 private，防止挂载传播到宿主机
    unix.Mount("", "/", "", unix.MS_PRIVATE|unix.MS_REC, "")

    // 步骤 2: 绑定挂载 rootfs 目录
    unix.Mount(rootFSPath, rootFSPath, "bind", unix.MS_BIND|unix.MS_REC, "")

    // 步骤 3: pivot_root 切换根目录
    //   newRoot = rootFSPath (新根)
    //   putOld  = rootFSPath/.pivot_root (旧根临时挂载点)
    unix.PivotRoot(rootFSPath, pivotDir)

    // 步骤 4: 切换工作目录到新根
    unix.Chdir("/")

    // 步骤 5: 卸载旧根
    unix.Unmount(putOldDir, unix.MNT_DETACH)

    // 步骤 6: 挂载 /proc（让 ps 等命令正常工作）
    unix.Mount("proc", "/proc", "proc", 0, "")

    // 步骤 7: 挂载 /tmp 为 tmpfs（基于内存的临时文件系统）
    unix.Mount("tmpfs", "/tmp", "tmpfs", unix.MS_NOSUID|unix.MS_NODEV, "size=64m")

    return nil
}
```

#### 为什么需要重新挂载 /proc？

```
┌──────────────────────────────────────────────────────────────┐
│  没有 /proc 的情况：                                          │
│  - ps aux 显示空白（无法读取进程信息）                        │
│  - top 无法工作                                              │
│  - /proc/cpuinfo、/proc/meminfo 不存在                       │
│                                                              │
│  直接使用宿主机 /proc 的情况：                                │
│  - ps aux 显示宿主机所有进程（PID Namespace 失效！）          │
│  - 容器可以看到宿主机的进程信息                               │
│                                                              │
│  正确做法：在新的 PID Namespace 中重新挂载 /proc              │
│  - ps aux 只显示容器内的进程                                 │
│  - /proc/1/cmdline 就是容器自己的 init 进程                  │
└──────────────────────────────────────────────────────────────┘
```

#### OverlayFS — Docker 镜像的分层原理

```
┌─────────────────────────────────────────────────────────────┐
│  Docker 镜像分层：                                           │
│                                                             │
│  ┌─────────────────┐  ← 可写层（容器运行时的修改写到这里）  │
│  ├─────────────────┤  ← 层3: RUN pip install flask         │
│  ├─────────────────┤  ← 层2: COPY app.py /app/             │
│  ├─────────────────┤  ← 层1: FROM python:3.9               │
│  └─────────────────┘                                        │
│                                                             │
│  OverlayFS 挂载命令：                                       │
│  mount -t overlay overlay \                                 │
│    -o lowerdir=/layer1:/layer2:/layer3, \                   │
│       upperdir=/container, \                                │
│       workdir=/work \                                       │
│    /merged                                                  │
│                                                             │
│  读取：从上到下查找，找到即返回                              │
│  修改：Copy-on-Write，先复制到 upper 层再修改               │
│  删除：在 upper 层创建 whiteout 标记文件                     │
└─────────────────────────────────────────────────────────────┘
```

本项目已实现 `MountOverlayFS()` 函数，但尚未集成到运行流程中。

---

### 4. 网络 — 容器通信

> 对应代码：`network/network_linux.go`

Docker 网络的核心：**Network Namespace + veth pair + Bridge + NAT**

#### 网络拓扑

```
┌──────────────────────────────────────────────────────────────┐
│  宿主机 Network Namespace                                    │
│  ┌──────────────────────────────────────────────────────┐   │
│  │  eth0 (192.168.1.100)                                │   │
│  │     │                                                │   │
│  │  docker0 / mini-mybridge (172.19.0.1) ← Bridge      │   │
│  │     ├── veth-xxx-h ← 容器A 的 veth 宿主机端          │   │
│  │     └── veth-yyy-h ← 容器B 的 veth 宿主机端          │   │
│  └──────────────────────────────────────────────────────┘   │
│                                                              │
│  ┌──────────────────────┐  ┌──────────────────────┐         │
│  │ 容器A Network NS     │  │ 容器B Network NS     │         │
│  │ eth0 (172.19.0.2)   │  │ eth0 (172.19.0.3)   │         │
│  │ veth-xxx-c          │  │ veth-yyy-c          │         │
│  └──────────────────────┘  └──────────────────────┘         │
└──────────────────────────────────────────────────────────────┘
```

#### veth pair — 虚拟网线

```
┌──────────────────────────────────────────────────────────────┐
│  veth pair 总是成对出现，像一根虚拟网线                       │
│                                                              │
│  veth-host ◄────────────────────► veth-container            │
│  (留在宿主机)                     (放入容器 Network NS)      │
│                                                              │
│  创建命令：                                                  │
│  ip link add veth-host type veth peer name veth-container   │
│                                                              │
│  数据从一端进，从另一端出                                    │
│  删除一端，另一端也会消失                                    │
└──────────────────────────────────────────────────────────────┘
```

#### Bridge — 虚拟交换机

```
┌──────────────────────────────────────────────────────────────┐
│  Bridge 连接多个网络设备，类似交换机                          │
│                                                              │
│  Docker 默认创建 docker0 网桥                                │
│  mini-docker 创建 mini-<name> 网桥                           │
│                                                              │
│  创建命令：                                                  │
│  ip link add mini-mybridge type bridge                      │
│  ip addr add 172.19.0.1/16 dev mini-mybridge               │
│  ip link set mini-mybridge up                                │
│                                                              │
│  将 veth 连接到网桥：                                        │
│  ip link set veth-host master mini-mybridge                 │
└──────────────────────────────────────────────────────────────┘
```

#### NAT — 容器访问外网

```
┌──────────────────────────────────────────────────────────────┐
│  容器访问外网的数据流：                                      │
│                                                              │
│  容器 → 容器.eth0 → veth → Bridge → NAT(iptables) → 宿主机.eth0 → 外网 │
│                                                              │
│  NAT 规则：                                                  │
│  iptables -t nat -A POSTROUTING \                           │
│    -s 172.19.0.0/16 ! -o mini-mybridge -j MASQUERADE        │
│                                                              │
│  将容器的私有 IP 转换为宿主机的公网 IP                       │
└──────────────────────────────────────────────────────────────┘
```

#### 代码实现：容器网络配置完整流程

```go
// network/network_linux.go

func (n *NetworkManager) Connect(pid int) error {
    // 1. 创建 veth pair
    ip link add veth-host type veth peer name veth-container

    // 2. 将一端放入容器的 Network Namespace
    ip link set veth-container netns <pid>

    // 3. 在容器内配置 IP
    nsenter -t <pid> -n ip addr add 172.19.0.x/16 dev veth-container

    // 4. 在容器内启用网络接口
    nsenter -t <pid> -n ip link set veth-container up
    nsenter -t <pid> -n ip link set lo up

    // 5. 将另一端连接到网桥
    ip link set veth-host master mini-mybridge
    ip link set veth-host up

    // 6. 设置默认路由（通过网桥网关）
    nsenter -t <pid> -n ip route add default via 172.19.0.1

    // 7. 配置 NAT（让容器可以访问外网）
    iptables -t nat -A POSTROUTING -s 172.19.0.0/16 ! -o mini-mybridge -j MASQUERADE
}
```

#### 动手验证

```bash
# 创建网络
sudo ./mini-docker network create mynet

# 查看网桥
ip addr show mini-mynet

# 运行容器并连接网络
sudo ./mini-docker run -n mynet -it myos /bin/sh

# 在容器内验证网络
ip addr          # 看到分配的 IP
ip route         # 看到默认路由
ping 172.19.0.1  # ping 网关
```

---

### 5. 镜像 — 分层与分发

> 对应代码：`image/image.go`

#### Docker 镜像的核心创新

```
┌──────────────────────────────────────────────────────────────┐
│  传统虚拟机镜像：一个完整的磁盘镜像（10GB+）                  │
│  → 体积大、传输慢、无法共享                                  │
│                                                              │
│  Docker 镜像：分层（Layer）+ 联合文件系统（UnionFS）         │
│  → 每层只存储差异（增量），可共享、可缓存                    │
└──────────────────────────────────────────────────────────────┘
```

#### Dockerfile 与镜像层

```dockerfile
FROM ubuntu:22.04       # → 基础层（约 77MB）
RUN apt-get update      # → 新增一层（更新后的包索引）
COPY app.py /app/       # → 新增一层（复制的文件）
RUN pip install flask   # → 新增一层（安装的依赖）
CMD ["python", "app.py"] # → 元数据（不增加层）
```

#### 镜像存储位置

```
/var/lib/docker/overlay2/
├── <layer-id-1>/
│   └── diff/          ← 该层的文件差异
├── <layer-id-2>/
│   └── diff/
└── <merged-id>/
    ├── merged/        ← 叠加后的完整文件系统
    ├── upper/         ← 容器可写层
    └── work/          ← OverlayFS 工作目录
```

#### 镜像拉取流程（真实 Docker）

```
┌──────────────────────────────────────────────────────────────┐
│  1. 客户端 → Registry: GET /v2/<name>/manifests/<tag>       │
│  2. Registry → 客户端: 返回 manifest（层列表 + SHA256）     │
│  3. 客户端逐层下载: GET /v2/<name>/blobs/<digest>           │
│  4. 每层验证 SHA256 校验和                                   │
│  5. 解压并存储到 overlay2 目录                               │
└──────────────────────────────────────────────────────────────┘
```

> ⚠️ 本项目的 `pull` 是简化版，只创建目录结构，不与 Registry 交互。

---

## 容器创建全流程

这是 mini-docker 运行一个容器时的完整执行流程，也是理解 Docker 原理最关键的部分：

```
用户执行: sudo ./mini-docker run -m 100m -n mynet -it myos /bin/sh
│
├── 1. main.go: 解析命令行参数
│   ├── 识别 -m 100m → MemoryLimit = "100m"
│   ├── 识别 -n mynet → Network = "mynet"
│   ├── 识别 -it → Tty = true
│   ├── 识别 myos → Image = "myos"
│   └── 识别 /bin/sh → Cmd = ["/bin/sh"]
│
├── 2. container.Run(): 创建容器
│   ├── 检查镜像 rootfs 是否存在
│   ├── 生成容器 ID 和名称
│   │
│   ├── 3. 创建子进程（关键步骤！）
│   │   ├── exec.Command("/proc/self/exe", "init", rootfsPath, "/bin/sh")
│   │   │   └── /proc/self/exe 是当前程序自身的路径
│   │   │       这是 Docker 的 "reexec" 模式：
│   │   │       重新执行自身，但通过参数区分是主进程还是容器进程
│   │   │
│   │   ├── setCloneFlags(cmd, CLONE_NEWUTS|CLONE_NEWIPC|CLONE_NEWPID|CLONE_NEWNS|CLONE_NEWNET)
│   │   │   └── 设置 Cloneflags，子进程创建时进入新 Namespace
│   │   │
│   │   └── cmd.Start()
│   │       └── 内核 fork 出子进程，同时创建新 Namespace
│   │
│   ├── 4. 父进程继续（子进程在后台初始化）
│   │   ├── CgroupManager.Apply(pid)
│   │   │   ├── 创建 /sys/fs/cgroup/memory/mini-docker-xxx/
│   │   │   ├── 写入 memory.limit_in_bytes = 104857600
│   │   │   ├── 创建 /sys/fs/cgroup/cpu/mini-docker-xxx/
│   │   │   ├── 写入 cpu.shares = 1024
│   │   │   └── 写入 cgroup.procs = <pid>
│   │   │
│   │   ├── NetworkManager.Connect(pid)
│   │   │   ├── ip link add veth-host type veth peer name veth-container
│   │   │   ├── ip link set veth-container netns <pid>
│   │   │   ├── nsenter ... ip addr add 172.19.0.x/16 dev veth-container
│   │   │   ├── ip link set veth-host master mini-mynet
│   │   │   ├── nsenter ... ip route add default via 172.19.0.1
│   │   │   └── iptables -t nat -A POSTROUTING ... -j MASQUERADE
│   │   │
│   │   └── saveContainerInfo() → 保存 JSON 到 /var/run/mini-docker/
│   │
│   └── 5. cmd.Wait()（前台模式等待子进程退出）
│
├── 6. 子进程执行（在新的 Namespace 中）
│   ├── main.go 检测到 os.Args[1] == "init"
│   ├── container.HandleInit()
│   │
│   ├── 7. InitProcess()
│   │   ├── SetupRootFS(rootfsPath)
│   │   │   ├── mount("", "/", "", MS_PRIVATE|MS_REC)  ← 防止挂载传播
│   │   │   ├── mount(rootfsPath, rootfsPath, "bind")  ← 绑定挂载
│   │   │   ├── pivot_root(rootfsPath, pivotDir)       ← 切换根目录！
│   │   │   ├── unmount(putOldDir, MNT_DETACH)         ← 卸载旧根
│   │   │   ├── mount("proc", "/proc", "proc")         ← 挂载 /proc
│   │   │   └── mount("tmpfs", "/tmp", "tmpfs")        ← 挂载 /tmp
│   │   │
│   │   ├── setHostname("mini-docker")  ← UTS Namespace 生效
│   │   │
│   │   └── syscall.Exec("/bin/sh", ...)  ← 替换进程映像
│   │       └── 此时进程已经在隔离环境中运行 /bin/sh
│   │
│   └── 8. 容器运行中！
│       ├── hostname → mini-docker
│       ├── ps aux → 只看到 /bin/sh (PID 1)
│       ├── ls / → 独立的文件系统
│       └── ip addr → 独立的网络栈
│
└── 9. 用户退出容器 (exit)
    ├── 子进程退出
    ├── 父进程 cmd.Wait() 返回
    ├── 更新容器状态为 "stopped"
    └── 清理 Cgroup
```

---

## 命令使用指南

### run — 创建并运行容器

```bash
sudo ./mini-docker run [选项] <镜像> <命令>

# 选项:
#   -it                交互式模式（分配终端）
#   -d                 后台运行模式
#   -m <内存限制>       内存限制（如 100m, 1g）
#   -c <CPU份额>        CPU 份额（如 512）
#   -n <网络名称>       加入指定网络
#   --name <容器名>     设置容器名称
#   -p <宿主端口>:<容器端口>  端口映射

# 示例:
sudo ./mini-docker run -it myos /bin/sh                    # 交互式运行
sudo ./mini-docker run -d myos /bin/sh                     # 后台运行
sudo ./mini-docker run -m 100m -c 512 -it myos /bin/sh    # 限制资源
sudo ./mini-docker run -n mynet -it myos /bin/sh           # 连接网络
sudo ./mini-docker run --name myapp -it myos /bin/sh       # 命名容器
```

### exec — 在容器内执行命令

```bash
sudo ./mini-docker exec <容器ID> <命令>

# 原理: 通过 setns() 加入容器的 Namespace
# 示例:
sudo ./mini-docker exec abc123 /bin/ls
```

### ps — 列出容器

```bash
sudo ./mini-docker ps

# 输出格式:
# 容器ID              名称            镜像            状态       创建时间
# 1715346800000       171534680000    myos            running    2024-05-15 10:00:00
```

### stop — 停止容器

```bash
sudo ./mini-docker stop <容器ID>

# 原理:
# 1. 发送 SIGTERM (信号15) → 优雅停止
# 2. 等待 2 秒
# 3. 如果进程未退出，发送 SIGKILL (信号9) → 强制杀死
```

### rm — 删除容器

```bash
sudo ./mini-docker rm <容器ID>

# 注意: 只能删除已停止的容器
```

### images — 列出镜像

```bash
sudo ./mini-docker images
```

### pull — 拉取镜像

```bash
sudo ./mini-docker pull <镜像名>

# 注意: 简化版，只创建最小 rootfs 目录结构
# 需要手动复制 busybox 等工具到 rootfs/bin/ 中
```

### network — 管理网络

```bash
sudo ./mini-docker network create <名称>    # 创建网络
sudo ./mini-docker network list              # 列出网络
sudo ./mini-docker network delete <名称>     # 删除网络
```

---

## 代码导读

建议按以下顺序阅读代码，循序渐进地理解容器原理：

### 第一阶段：理解入口和 reexec 机制

| 文件 | 重点 |
|------|------|
| `main.go:13-17` | `IsInitProcess()` 检测 — 理解 Docker 的 reexec 模式 |
| `main.go:78-175` | `runCommand()` — 理解参数如何传递到容器 |
| `container/container.go:266-268` | `IsInitProcess()` — 如何区分主进程和容器进程 |

**核心问题**：为什么不直接在子进程中执行用户命令？

> 因为需要在执行用户命令之前完成 RootFS 设置等初始化工作。Docker 的 runc 也是用同样的 reexec 模式。

### 第二阶段：理解 Namespace 隔离

| 文件 | 重点 |
|------|------|
| `container/namespace_linux.go:23-31` | `NewNamespaceFlags()` — 5 种 Namespace 标志 |
| `container/namespace_linux.go:63-71` | `setCloneFlags()` — 如何设置到子进程 |
| `container/namespace_linux.go:49-61` | `SetNamespace()` — docker exec 的底层实现 |
| `container/container.go:141-165` | `Exec()` — setns 加入已有 Namespace |

**动手实验**：修改 `NewNamespaceFlags()`，去掉某个 flag，观察容器内有什么变化。

### 第三阶段：理解 RootFS 文件系统隔离

| 文件 | 重点 |
|------|------|
| `container/rootfs_linux.go:14-41` | `SetupRootFS()` — 完整的文件系统隔离流程 |
| `container/rootfs_linux.go:44-63` | `pivotRoot()` — 根目录切换的核心 |
| `container/rootfs_linux.go:64-69` | `mountProc()` — 为什么需要重新挂载 /proc |
| `container/rootfs_linux.go:78-99` | `MountOverlayFS()` — 镜像分层的实现 |

**动手实验**：注释掉 `mountProc()`，在容器内执行 `ps aux` 看看会发生什么。

### 第四阶段：理解 Cgroup 资源限制

| 文件 | 重点 |
|------|------|
| `cgroup/cgroup_linux.go:25-45` | `Apply()` — Cgroup 配置入口 |
| `cgroup/cgroup_linux.go:47-67` | `setMemoryLimit()` — 内存限制实现 |
| `cgroup/cgroup_linux.go:69-85` | `setCpuShares()` — CPU 限制实现 |
| `cgroup/cgroup_linux.go:121-139` | `Freeze()/Unfreeze()` — docker pause 的实现 |

**动手实验**：运行一个限制内存 50m 的容器，在容器内尝试 `dd if=/dev/zero bs=1M count=100`。

### 第五阶段：理解网络隔离

| 文件 | 重点 |
|------|------|
| `network/network_linux.go:30-67` | `Create()` — 创建 Bridge 网桥 |
| `network/network_linux.go:119-188` | `Connect()` — 完整的容器网络配置流程 |
| `container/namespace_linux.go:23-31` | `CLONE_NEWNET` — Network Namespace 隔离 |

**动手实验**：创建两个连接同一网络的容器，互相 ping 看看是否通。

### 第六阶段：理解容器生命周期

| 文件 | 重点 |
|------|------|
| `container/container.go:54-123` | `Run()` — 容器创建全流程 |
| `container/container.go:125-139` | `InitProcess()` — 容器内初始化 |
| `container/container.go:167-191` | `Stop()` — SIGTERM → SIGKILL |
| `container/container.go:207-248` | `ListContainers()` — 容器状态管理 |

---

## 与真实 Docker 的差距

### 已实现的核心功能

| 功能 | mini-docker | Docker | 底层技术 |
|------|------------|--------|---------|
| Namespace 隔离 | ✅ 5种 | ✅ 6种（含User） | clone + Cloneflags |
| Cgroup 内存限制 | ✅ v1 | ✅ v1+v2 | memory.limit_in_bytes |
| Cgroup CPU 限制 | ✅ shares | ✅ shares+quota+cpuset | cpu.shares / cpu.cfs_quota_us |
| RootFS 隔离 | ✅ pivot_root | ✅ pivot_root | mount + pivot_root |
| Bridge 网络 | ✅ veth+bridge+NAT | ✅ | ip link / iptables |
| 容器生命周期 | ✅ run/stop/rm/ps | ✅ 完整 | signal + JSON 元数据 |
| docker exec | ✅ setns | ✅ | /proc/<pid>/ns/* |
| TTY 支持 | ✅ | ✅ | Setctty + Setsid |

### 未实现的重要功能

| 功能 | 重要程度 | 说明 |
|------|---------|------|
| **镜像分层（OverlayFS）** | 🔴 高 | `MountOverlayFS()` 已实现但未集成 |
| **Volume 数据卷** | 🔴 高 | 无 `-v` 挂载，容器删除数据丢失 |
| **端口映射** | 🔴 高 | `-p` 已解析但未实现 iptables DNAT |
| **镜像构建（Dockerfile）** | 🔴 高 | 无 build 命令 |
| **Registry 镜像拉取** | 🟡 中 | pull 只创建空目录 |
| **docker start** | 🟡 中 | 停止的容器无法重启 |
| **docker logs** | 🟡 中 | 无日志收集 |
| **环境变量 (-e)** | 🟡 中 | 直接透传宿主机环境变量 |
| **Cgroup v2** | 🟡 中 | 只支持 v1 |
| **网络模式** | 🟡 中 | 只有 bridge，缺 host/none/overlay |

### 未实现的安全功能

| 安全特性 | 重要程度 | 说明 |
|---------|---------|------|
| **User Namespace** | 🔴 高 | 容器内 root = 宿主机 root（最危险！） |
| **Linux Capabilities** | 🔴 高 | 未删除任何能力，容器拥有全部权限 |
| **Seccomp** | 🔴 高 | 未限制系统调用，可执行危险 syscall |
| **AppArmor/SELinux** | 🟡 中 | 无强制访问控制 |
| **/proc /sys 遮蔽** | 🟡 中 | 暴露内核敏感信息 |
| **PID 1 信号处理** | 🟡 中 | 无僵尸进程回收，无信号转发 |
| **只读根文件系统** | 🟢 低 | 无 `--read-only` 选项 |
| **镜像签名验证** | 🟢 低 | 无 Content Trust |

> ⚠️ **安全警告**：由于缺少 User Namespace、Capabilities 限制和 Seccomp，mini-docker 容器内的进程拥有宿主机的完整 root 权限。**绝对不要在生产环境使用，也不要运行不受信任的程序！**

---

## 学习路线建议

### 入门路线（1-2 天）

1. **阅读本文档**，建立对容器三大技术的整体认知
2. **编译并运行**第一个容器，验证隔离效果
3. **阅读 `container/namespace_linux.go`**，理解 Namespace 的代码实现
4. **动手实验**：修改 Namespace flags，观察容器行为变化

### 进阶路线（3-5 天）

5. **阅读 `container/rootfs_linux.go`**，理解 pivot_root 和 /proc 挂载
6. **阅读 `cgroup/cgroup_linux.go`**，理解资源限制的文件系统接口
7. **阅读 `network/network_linux.go`**，理解 veth pair + Bridge 的配置流程
8. **动手实验**：手动执行 `ip link`、`iptables` 命令，理解每一步的作用

### 深入路线（1-2 周）

9. **实现 OverlayFS 集成**：将 `MountOverlayFS()` 接入容器运行流程
10. **实现 Volume 挂载**：添加 `-v` 参数，在 pivot_root 之前 bind mount
11. **实现端口映射**：添加 iptables DNAT 规则
12. **实现 User Namespace**：添加 `CLONE_NEWUSER`，映射 UID/GID
13. **实现 Capabilities 限制**：使用 `unix.Prctl()` 删除不需要的能力

### 扩展阅读

- [Docker 官方文档](https://docs.docker.com/)
- [Linux Namespace 手册](https://man7.org/linux/man-pages/man7/namespaces.7.html)
- [Linux Cgroup 手册](https://man7.org/linux/man-pages/man7/cgroups.7.html)
- [runc 源码](https://github.com/opencontainers/runc) — Docker 底层的容器运行时
- [自己动手写 Docker](https://book.douban.com/subject/27070705/) — 中文经典教材

---

## 常见问题

### Q: 为什么必须在 Linux 上运行？

A: 容器技术依赖 Linux 内核的三大特性：Namespace、Cgroup、OverlayFS。这些都是 Linux 内核特有的功能，Windows 和 macOS 的内核不支持。WSL2 运行的是真正的 Linux 内核，所以可以使用。

### Q: 为什么需要 root 权限？

A: 创建 Namespace、操作 Cgroup、执行 pivot_root 都需要 root 权限。Docker 也是通过 docker daemon（root 进程）来执行这些操作的。

### Q: 容器内为什么看不到宿主机的进程？

A: 因为 PID Namespace 隔离了进程 ID 空间。在新的 PID Namespace 中，容器进程从 PID 1 开始编号，看不到宿主机 PID Namespace 中的进程。同时，我们重新挂载了 /proc，所以 `ps aux` 只显示容器内的进程。

### Q: 容器内为什么 hostname 是 mini-docker？

A: 因为 UTS Namespace 隔离了主机名。我们在 `InitProcess()` 中调用了 `setHostname("mini-docker")`，这只影响当前 UTS Namespace，不会改变宿主机的主机名。

### Q: `-m 100m` 是如何限制内存的？

A: 向 `/sys/fs/cgroup/memory/<cgroup>/memory.limit_in_bytes` 写入 104857600（100MB 的字节数）。当容器进程尝试使用超过限制的内存时，内核会触发 OOM Killer 杀死进程。

### Q: docker exec 是怎么实现的？

A: 通过 `setns()` 系统调用。打开目标容器的 `/proc/<pid>/ns/` 下的文件，调用 `setns()` 将当前进程加入容器的 Namespace，然后在新 Namespace 中执行命令。

### Q: /proc/self/exe 是什么？

A: 它是指向当前可执行文件自身的符号链接。我们通过它重新执行自己，但传入 `init` 参数，让程序知道自己是在容器内运行。这就是 Docker 的 "reexec" 模式。

### Q: 为什么 pivot_root 比 chroot 更安全？

A: chroot 只改变进程对 "/" 的视角，但不改变当前工作目录，可以通过相对路径 ".." 逃逸。pivot_root 原子性地切换根目录，旧根被移动到新位置后卸载，无法通过路径逃逸。

### Q: 容器和虚拟机的根本区别是什么？

A: 容器共享宿主机内核，是"特殊的进程"；虚拟机有独立内核，是"完整的计算机"。容器启动快、体积小，但安全性较弱（共享内核意味着内核漏洞会影响所有容器）。
