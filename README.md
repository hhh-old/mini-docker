# mini-docker — 从零理解容器运行时原理

> 一个用 Go 语言实现的迷你版容器运行时，对齐 Docker / containerd / runc 的分层架构，通过可运行的代码学习容器底层原理。
>
> 容器的本质只有一句话：**容器 = Namespace（隔离）+ Cgroup（限制）+ RootFS（文件系统）**
>
> 运行时的本质：**CLI ↔ Daemon ↔ containerd ↔ shim ↔ runtime ↔ libcontainer → 内核**

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
  - [5. 镜像 — Content Store + Snapshotter + BoltDB](#5-镜像--content-store--snapshotter--boltdb)
  - [6. 多进程分层 — Daemon / containerd / shim / runtime](#6-多进程分层--daemon--containerd--shim--runtime)
  - [7. GC — 基于 Lease 的垃圾回收](#7-gc--基于-lease-的垃圾回收)
- [容器创建全流程](#容器创建全流程)
- [命令使用指南](#命令使用指南)
- [代码导读](#代码导读)
- [与真实 Docker 的差距](#与真实-docker-的差距)
- [学习路线建议](#学习路线建议)
- [常见问题](#常见问题)

---

## 项目简介

### 为什么写这个项目？

Docker 看起来像魔法，但它的底层原理并不复杂。这个项目在第一版"单文件实现 Namespace + Cgroup + RootFS"的基础上，进一步对齐真实 Docker 的多进程分层架构（dockerd / containerd / shim / runc），让你能直接看到：

- 一次 `docker run` 请求是如何跨越多个进程边界最终调用到内核的
- containerd 的 content-addressable 存储模型是怎么组织的（BoltDB 元数据 + Content Store + Snapshotter）
- shim 进程为什么必须存在，它解决了什么问题
- runc / libcontainer 是如何消费 OCI Spec 启动容器的
- `docker build` / `docker pull` 在底层走了哪些 HTTP 调用、生成哪些层

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
- **Go 版本**：1.23+
- **权限**：需要 root 权限运行（操作 namespace/cgroup/mount/iptables）
- **依赖工具**：`iptables`（建议 `iptables-legacy`）、`iproute2`（`ip` 命令）、`busybox`（用于本地构建的镜像）

> ⚠️ 本项目使用 Linux 内核特性（Namespace、Cgroup、pivot_root、OverlayFS），**必须在 Linux 环境下运行**。Windows / macOS 上只能编译，无法实际运行容器。

### 编译

```bash
# 克隆项目
cd mini-docker

# 编译当前平台
go build -o mini-docker .

# 交叉编译 Linux 版本（在 Windows/macOS 上）
GOOS=linux GOARCH=amd64 go build -o mini-docker .
```

### 第一次跑通

```bash
# 1. 启动 Daemon（自动拉起 containerd 子进程）
sudo ./mini-docker daemon &

# 2. 拉取一个公开镜像（直接走 Docker Hub）
sudo ./mini-docker pull alpine:3.18

# 3. 创建一个网络
sudo ./mini-docker network create mynet

# 4. 启动一个交互式容器
sudo ./mini-docker run -it --name myalpine -n mynet alpine:3.18 /bin/sh

# 5. 在容器内验证隔离
hostname            # 显示 mini-docker（UTS Namespace）
ps aux              # 只看到 /bin/sh（PID Namespace）
ip addr             # 独立网络栈（Network Namespace）
ls /                # 独立文件系统（Mount Namespace + OverlayFS）
exit                # 退出容器

# 6. 列出 / 启动 / 停止 / 删除
sudo ./mini-docker ps -a
sudo ./mini-docker start myalpine
sudo ./mini-docker stop myalpine
sudo ./mini-docker rm myalpine
```

### 资源限制 + 端口映射 + 卷挂载 + 重启策略

```bash
# 限制内存 100MB、CPU 512 份额、命名容器 myapp
sudo ./mini-docker run -d --name myapp -m 100m -c 512 alpine:3.18 sleep 3600

# 端口映射：把宿主机 8080 映射到容器 80
sudo ./mini-docker run -d --name web -p 8080:80 nginx:alpine

# 卷挂载：bind mount 主机目录 + 命名卷
sudo ./mini-docker volume create mydata
sudo ./mini-docker run -d --name app \
    -v /opt/config:/etc/app:ro \
    -v mydata:/var/lib/app \
    myimage:1.0

# 重启策略
sudo ./mini-docker run -d --restart always --name resilient alpine:3.18 sleep 3600
```

### 用 Dockerfile 构建镜像

```bash
# 在项目根目录准备 Dockerfile
cat > Dockerfile <<EOF
FROM alpine:3.18
RUN apk add --no-cache curl
COPY hello.sh /usr/local/bin/hello.sh
RUN chmod +x /usr/local/bin/hello.sh
CMD ["/usr/local/bin/hello.sh"]
EOF

# 构建
sudo ./mini-docker build -t myhello:1.0 .
```

### 动手验证 Namespace

```bash
# 启动一个容器
sudo ./mini-docker run -it alpine:3.18 /bin/sh

# 在另一个终端查看容器的 Namespace
sudo ls -la /proc/<容器PID>/ns/

# 对比宿主机进程的 Namespace
ls -la /proc/$$/ns/

# 它们的 Namespace inode 不同 —— 隔离生效
```

---

## 项目架构

### 进程分层（对齐 Docker）

```
┌─────────────────────────────────────────────────────────────────┐
│                    Docker 真实架构                                │
│                                                                 │
│  docker CLI → dockerd → containerd → containerd-shim → runc     │
│       ↑             ↑         ↑              ↑              ↑   │
│      Unix         gRPC      gRPC         TTRPC        OCI Spec  │
│     Socket                                              + 内核   │
└─────────────────────────────────────────────────────────────────┘

┌─────────────────────────────────────────────────────────────────┐
│                    mini-docker 架构                              │
│                                                                 │
│  mini-docker  →  mini-docker   →  mini-docker  →  mini-docker   │
│    CLI           daemon           containerd       shim         │
│     │              │                  │              │          │
│     │   Unix Socket│    Unix Socket   │     /proc/   │  /proc/  │
│     │  (JSON+Stream) (JSON+Stream)   │  self/exe    │  self/exe│
│     │                                │              │  "init"  │
│     │                                ▼              ▼          │
│     │                          mini-docker   mini-docker       │
│     │                          runtime        containerinit    │
│     │                          (runc)        (libcontainer)    │
│     │                                            │              │
│     └────────────────────────────────────────────┴→ Linux Kernel│
│                                          Namespace/Cgroup/Mount│
└─────────────────────────────────────────────────────────────────┘

核心特征：
- 每一层都通过 Unix Socket 通信
- 每一层都通过 /proc/self/exe 启动下一层（"reexec" 模式）
- CLI / Daemon / containerd / shim / runtime 是 5 个独立进程，互不依赖
```

### 代码结构

```
mini-docker/
├── main.go                              # CLI 入口 + reexec 入口（init / runtime / shim 子命令）
│
├── constants/                           # 全局常量（路径、超时、格式化）
│   └── constants.go
│
├── types/                               # 跨包共享类型
│   └── types.go
│
├── utils/                               # 通用工具函数
│
├── daemon/                              # 对标 dockerd
│   ├── daemon.go                        # 守护进程主循环：监听 /var/run/mini-docker/mini-docker.sock
│   ├── handler.go                       # 请求处理器：run / ps / stop / start / pull / build ...
│   ├── client.go                        # CLI → Daemon 的 Unix Socket 客户端
│   ├── protocol.go                      # CLI ↔ Daemon 的 Request/Response/ProgressFrame
│   └── events.go                        # 容器事件总线
│
├── containerd/                          # 对标 containerd（独立进程）
│   ├── server_linux.go                  # containerd 主进程：监听 /var/run/mini-docker/containerd.sock
│   ├── client_linux.go                  # Daemon → containerd 的客户端
│   ├── api.go / api_linux.go            # Daemon ↔ containerd 的 Request/Response
│   ├── routes.go                        # 路由键常量（ReqCreateTask 等）
│   ├── handler_task_linux.go            # Task 生命周期：Create/Start/Kill/Delete/Attach/Exec
│   ├── handler_image_linux.go           # 镜像管理：Pull/Remove/List
│   ├── handler_snapshot_linux.go        # 快照管理：Prepare/Remove/RegisterCommitted
│   ├── handler_gc_linux.go              # 垃圾回收
│   ├── shim_manager_linux.go            # Shim 进程的创建/通信/销毁
│   ├── content/                         # 对标 containerd content store
│   │   ├── store.go                     # blob 存储接口
│   │   ├── filesys.go                   # 文件系统实现：/var/lib/mini-docker/content/sha256/<digest>
│   │   ├── content_linux.go / _other.go # 平台相关辅助
│   │   └── contentStore详解.md
│   ├── metadata/                        # 对标 containerd metadata（boltdb）
│   │   ├── db.go                        # boltdb 封装
│   │   ├── images.go                    # 镜像元数据读写
│   │   ├── layers.go                    # 层元数据
│   │   ├── snapshots.go                 # 快照元数据
│   │   ├── tags.go                      # 标签映射
│   │   └── leases.go                    # 租约
│   ├── snapshots/                       # 对标 containerd snapshots.Snapshotter
│   │   ├── snapshotter.go               # 可插拔接口
│   │   └── overlay/                     # OverlayFS 实现
│   │       ├── overlay.go
│   │       └── overlay_unpack_linux.go  # tar.gz 解压 + 写时复制
│   ├── images/                          # 对标 containerd images service
│   │   ├── service.go                   # Pull/Remove/List/Resolve 入口
│   │   ├── image.go                     # pullFromRegistry / pullLocal
│   │   ├── registry.go                  # Docker Registry V2 HTTP 客户端
│   │   ├── oci_linux.go                 # OCI 镜像层解压
│   │   ├── busybox_linux.go             # 本地构建 busybox rootfs
│   │   ├── dev_linux.go                 # 创建设备节点
│   │   ├── progress.go                  # 拉取进度回调
│   │   └── 关于镜像每层是怎么来的.md
│   └── gc/                              # 垃圾回收（对标 containerd GC）
│       ├── gc.go                        # 三色标记清除
│       ├── lease.go                     # Lease 管理
│       └── adapter.go                   # Content / Snapshot 适配器
│
├── shim/                                # 对标 containerd-shim
│   ├── shim.go                          # Shim 进程主入口
│   ├── handlers.go                      # Shim 控制协议（create/start/attach/...）
│   ├── runtime.go                       # 与 runtime 子进程的桥接
│   ├── context.go                       # 容器运行时上下文（PTY / FIFO / I/O）
│   └── shim说明.md
│
├── runtime/                             # 对标 runc
│   ├── runtime.go                       # OCI runtime 入口（create/start/kill/state/...）
│   └── runtime_other.go                 # 非 Linux 桩
│
├── libcontainer/                        # 对标 runc/libcontainer
│   ├── container.go                     # Container 接口
│   ├── factory.go                       # 工厂：创建 LinuxContainer
│   ├── linux_container.go               # 容器生命周期主流程
│   ├── configs/                         # 容器配置（OCI runtime-spec + 扩展）
│   │   ├── config.go                    # Config / Namespace / MountSpec / RootFS
│   │   ├── capabilities.go              # Linux capabilities
│   │   ├── resources.go                 # Cgroup 资源限制
│   │   └── seccomp.go                   # seccomp 规则
│   ├── cgroups/                         # Cgroup 抽象
│   │   ├── cgroup.go                    # 统一接口
│   │   ├── fs_linux.go                  # cgroup v1
│   │   └── fs2_linux.go                 # cgroup v2（统一层级）
│   ├── containerinit/                   # 容器 init 进程（在隔离环境内执行）
│   │   ├── init.go                      # 入口：解析 OCI Spec / 打开 FIFO / pivot_root
│   │   ├── rootfs_linux.go              # OverlayFS 挂载 + pivot_root + 挂 /proc + /tmp
│   │   └── capability_linux.go          # 删除 capability
│   ├── hooks.go                         # OCI runtime hooks
│   └── factory_linux.go                 # init 进程创建
│
├── containerinit/                       # （旧版，保留兼容）容器 init 入口
│   └── init.go
│
├── containerstore/                      # 容器元数据存储
│   ├── container.go                     # ContainerInfo 结构 + 落盘 /var/run/mini-docker/<id>.json
│   └── health.go                        # 容器健康检查
│
├── builder/                             # 对标 docker build
│   └── builder.go                       # Dockerfile 解析 + OverlayFS 增量构建
│
├── network/                             # 容器网络
│   ├── network_linux.go                 # Bridge + veth pair + iptables NAT
│   └── network_other.go
│
├── volume/                              # 数据卷
│   └── volume.go                        # Bind Mount + 命名卷
│
├── pty/                                 # PTY（伪终端）
│   ├── pty_linux.go                     # openpty / forkpty
│   └── pty_other.go
│
├── spec/                                # OCI runtime-spec
│   └── spec.go                          # config.json 加载/生成
│
├── tests/                               # 集成测试脚本
│
├── go.mod / go.sum
└── README.md
```

### 存储布局（对齐 containerd）

```
/var/lib/mini-docker/
├── metadata.db                         # BoltDB：镜像/层/标签/快照/租约/content 元数据
├── content/sha256/                     # Content Store：manifest/config/layer 的原始 blob
│   └── <hex>                           # 文件名 = digest 的 hex 部分
├── snapshots/overlay/                  # OverlayFS Snapshotter：解压后的层快照
│   └── <cache-id>/
│       └── diff/                       # 该层的文件内容
├── containers/                         # 容器可写层 / OCI bundle
├── networks/                           # 网络配置（JSON）
├── volumes/                            # 命名卷
│   └── <name>/
│       ├── _data/                      # 实际数据
│       └── metadata.json
├── runtime/                            # runtime 临时数据
└── build/                              # 构建器临时 overlay mount

/var/run/mini-docker/
├── mini-docker.sock                    # CLI ↔ Daemon
├── containerd.sock                     # Daemon ↔ containerd
├── daemon.pid
├── daemon.log
├── containerd.log
├── shim/
│   └── <containerID>/
│       ├── shim.sock                   # containerd ↔ shim
│       ├── shim.pid
│       ├── shim.log
│       └── state.json                  # OCI runtime state
└── <containerID>.json                  # 容器元数据
```

---

## 核心原理详解

### 1. Namespace — 进程隔离

> 对应代码：`libcontainer/containerinit/init.go`（运行时通过 OCI Spec 中的 `linux.namespaces` 配置）

Namespace 是 Linux 内核提供的进程隔离机制，让一个进程只能看到与自己相关的系统资源，产生"我拥有整个系统"的错觉。

#### 六种 Namespace

| Namespace | 隔离内容 | 系统调用参数 | 效果 |
|-----------|---------|-------------|------|
| **PID** | 进程 ID | `CLONE_NEWPID` | 容器内进程从 PID 1 开始，看不到宿主机进程 |
| **Mount** | 文件系统挂载点 | `CLONE_NEWNS` | 容器有独立的文件系统视图 |
| **UTS** | 主机名和域名 | `CLONE_NEWUTS` | 容器可以有自己的 hostname |
| **IPC** | 进程间通信 | `CLONE_NEWIPC` | 隔离信号量、消息队列、共享内存 |
| **Network** | 网络栈 | `CLONE_NEWNET` | 独立的 IP、端口、路由表 |
| **User** | 用户/用户组 ID | `CLONE_NEWUSER` | 容器内 root ≠ 宿主机 root |

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

`docker exec` 的实现就是打开这些文件，调用 `setns()` 将新进程加入容器的 Namespace。

---

### 2. Cgroup — 资源限制

> 对应代码：`libcontainer/cgroups/`（cgroup v1 + v2 双实现，对齐 runc）

Namespace 解决了"能看到什么"的问题，Cgroup 解决了"能用多少"的问题。没有 Cgroup，一个容器可以耗尽宿主机所有资源。

#### Cgroup 核心概念

```
┌─────────────────────────────────────────────────────────────┐
│  Cgroup 层级树（v1 视图）                                    │
│                                                             │
│  /sys/fs/cgroup/memory/mini-docker/<id>/                    │
│  │   ├── cgroup.procs            ← 属于该组的进程 PID       │
│  │   ├── memory.limit_in_bytes   ← 内存上限                 │
│  │   └── memory.usage_in_bytes   ← 当前内存使用量           │
│  │                                                          │
│  /sys/fs/cgroup/cpu/mini-docker/<id>/                       │
│  │   ├── cgroup.procs                                       │
│  │   ├── cpu.shares              ← CPU 份额权重             │
│  │   └── cpu.cfs_quota_us        ← CPU 配额（v2 等价）      │
│  │                                                          │
│  /sys/fs/cgroup/freezer/mini-docker/<id>/                   │
│      ├── cgroup.procs                                       │
│      └── freezer.state           ← FROZEN / THAWED          │
└─────────────────────────────────────────────────────────────┘
```

#### 内存限制

```go
// libcontainer/cgroups/fs_linux.go（v1 示例）

// 向 cgroup.procs 写入 PID = 把进程加入该 cgroup
ioutil.WriteFile(filepath.Join(path, "cgroup.procs"), []byte(strconv.Itoa(pid)), 0644)

// 写入内存上限（字节）
ioutil.WriteFile(filepath.Join(path, "memory.limit_in_bytes"),
    []byte("104857600"), 0644) // 100MB
```

**这就是 `docker run -m 100m` 的全部秘密。**

#### 冻结/恢复

写入 `freezer.state = FROZEN` 暂停所有进程；写入 `THAWED` 恢复。**这就是 `docker pause/unpause` 的实现。**

#### Cgroup v1 vs v2

| 特性 | Cgroup v1 | Cgroup v2 |
|------|-----------|-----------|
| 目录结构 | 每个子系统独立挂载 | 统一层级 `/sys/fs/cgroup/` |
| mini-docker | ✅ 已实现 | ✅ 已实现（`fs2_linux.go`） |
| Docker 默认 | 旧版本 | 新版本（20.10+） |
| 复杂度 | 灵活但复杂 | 简洁统一 |

#### 动手验证

```bash
# 运行一个限制内存的容器
sudo ./mini-docker run -m 100m -it alpine:3.18 /bin/sh

# 在另一个终端查看 cgroup 设置
sudo cat /sys/fs/cgroup/memory/mini-docker-*/memory.limit_in_bytes
# 输出: 104857600 (100MB)

# 在容器内尝试分配超过限制的内存
dd if=/dev/zero bs=1M count=200
# 进程会被 OOM Kill！
```

---

### 3. RootFS — 文件系统隔离

> 对应代码：`libcontainer/containerinit/rootfs_linux.go`（pivot_root + OverlayFS）

RootFS 解决了"容器为什么看起来像独立操作系统"的问题。通过 Mount Namespace + `pivot_root` + OverlayFS，容器进程的 `/` 指向一个独立的、合成的目录。

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

#### 完整 RootFS 流程

```go
// libcontainer/containerinit/rootfs_linux.go

func SetupRootFS(rootFSPath string, overlay *types.OverlayDirs) error {
    // 1. 把 / 重新挂载为 private，防止挂载传播到宿主机
    unix.Mount("", "/", "", unix.MS_PRIVATE|unix.MS_REC, "")

    if overlay != nil {
        // 2a. 多层镜像：通过 Snapshotter.Mounts() 拿到 lowerdir 列表，叠加为 OverlayFS
        //     lower = [layerN/diff, ..., layer1/diff]
        //     upper = container/<id>/upper
        //     work  = container/<id>/work
        unix.Mount("overlay", merged, "overlay", 0,
            fmt.Sprintf("lowerdir=%s,upperdir=%s,workdir=%s",
                strings.Join(overlay.Lower, ":"), overlay.Upper, overlay.Work))
    } else {
        // 2b. 单层镜像（本地构建）：直接 bind mount
        unix.Mount(rootFSPath, rootFSPath, "bind", unix.MS_BIND|unix.MS_REC, "")
    }

    // 3. 切换根目录
    unix.PivotRoot(merged, pivotDir)

    // 4. 切换工作目录到新根
    unix.Chdir("/")

    // 5. 卸载旧根
    unix.Unmount(putOldDir, unix.MNT_DETACH)

    // 6. 挂载 /proc（让 ps 等命令正常工作，且只能看到容器内进程）
    unix.Mount("proc", "/proc", "proc", 0, "")

    // 7. 挂载 /tmp 为 tmpfs
    unix.Mount("tmpfs", "/tmp", "tmpfs", unix.MS_NOSUID|unix.MS_NODEV, "size=64m")

    return nil
}
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

---

### 4. 网络 — 容器通信

> 对应代码：`network/network_linux.go`

Docker 网络的核心：**Network Namespace + veth pair + Bridge + iptables NAT**

#### 网络拓扑

```
┌──────────────────────────────────────────────────────────────┐
│  宿主机 Network Namespace                                    │
│  ┌──────────────────────────────────────────────────────┐   │
│  │  eth0 (192.168.1.100)                                │   │
│  │     │                                                │   │
│  │  mini-bridge (172.19.0.1) ← Bridge                  │   │
│  │     ├── veth-xxx-h ← 容器A 的 veth 宿主机端          │   │
│  │     └── veth-yyy-h ← 容器B 的 veth 宿主机端          │   │
│  └──────────────────────────────────────────────────────┘   │
│                                                              │
│  ┌──────────────────────┐  ┌──────────────────────┐         │
│  │ 容器A Network NS     │  │ 容器B Network NS     │         │
│  │ eth0 (172.19.0.2)    │  │ eth0 (172.19.0.3)    │         │
│  │ veth-xxx-c           │  │ veth-yyy-c           │         │
│  └──────────────────────┘  └──────────────────────┘         │
└──────────────────────────────────────────────────────────────┘
```

#### veth pair — 虚拟网线

```
veth pair 总是成对出现，像一根虚拟网线：

veth-host ◄────────────────────► veth-container
(留在宿主机)                     (放入容器 Network NS)

创建命令：
ip link add veth-host type veth peer name veth-container

数据从一端进，从另一端出
删除一端，另一端也会消失
```

#### Bridge — 虚拟交换机

```
Bridge 连接多个网络设备，类似交换机。

mini-docker 默认创建 mini-bridge 网桥；
用户用 `network create <name>` 创建自定义网桥。

将 veth 连接到网桥：
ip link set veth-host master mini-bridge
```

#### NAT — 容器访问外网

```
容器访问外网的数据流：
容器 → 容器.eth0 → veth → Bridge → NAT(iptables) → 宿主机.eth0 → 外网

NAT 规则：
iptables -t nat -A POSTROUTING \
    -s 172.19.0.0/16 ! -o mini-bridge -j MASQUERADE
```

#### 端口映射（DNAT）

```
宿主机 8080 → 容器 80：
iptables -t nat -A MINI-DOCKER -p tcp --dport 8080 \
    -j DNAT --to-destination 172.19.0.2:80
```

#### 动手验证

```bash
# 创建网络
sudo ./mini-docker network create mynet

# 查看网桥
ip addr show mini-mynet

# 运行两个容器，加入同一网络
sudo ./mini-docker run -d --name c1 -n mynet alpine:3.18 sleep 3600
sudo ./mini-docker run -d --name c2 -n mynet alpine:3.18 sleep 3600

# 在 c1 里 ping c2（按 IP）
sudo ./mini-docker exec c1 ping <c2-IP>
```

---

### 5. 镜像 — Content Store + Snapshotter + BoltDB

> 对应代码：`containerd/images/`、`containerd/content/`、`containerd/snapshots/`、`containerd/metadata/`

mini-docker 的镜像管理**完全对齐 containerd 的 content-addressable 模型**，所有"看起来像 Docker 镜像"的东西都来自三个组件的协作：

```
┌──────────────────────────────────────────────────────────────┐
│  Image Service（containerd/images）                          │
│  ┌──────────────────────────────────────────────────────┐   │
│  │  Pull / Remove / List / Resolve / Register            │   │
│  │  ↓                                                    │   │
│  │  1. content.Store       ← blob 落盘 + SHA256 校验     │   │
│  │     content/sha256/<digest>                            │   │
│  │  2. snapshots.Snapshotter ← 解压层到 diff/            │   │
│  │     snapshots/overlay/<cache-id>/diff/                │   │
│  │  3. metadata.DB (boltdb) ← 写 image/layer/tag 索引    │   │
│  │     metadata.db                                        │   │
│  │  4. gc.LeaseManager    ← 拉取期间持有 Lease 防止 GC   │   │
│  └──────────────────────────────────────────────────────┘   │
└──────────────────────────────────────────────────────────────┘
```

#### 拉取镜像的全流程

```bash
# 触发：sudo ./mini-docker pull alpine:3.18
#
# CLI → Daemon(请求) → containerd(处理) → Registry V2
#
# 1. CLI 通过 Unix Socket 发 "pull" 请求给 Daemon
# 2. Daemon 转发给 containerd
# 3. containerd 创建一个 Lease（保护本次拉取的对象不被 GC 清理）
# 4. RegistryClient 走 Docker Registry V2 协议：
#    a. GET /v2/library/alpine/manifests/3.18  → manifest
#    b. GET /v2/library/alpine/blobs/sha256:<config-digest> → OCI config
#    c. 对每一层：
#       - GET /v2/library/alpine/blobs/sha256:<layer-digest> → blob
#       - 写入 content/sha256/<digest>（边写边算 SHA256 校验）
#       - Snapshotter.UnpackLayer()：解压 tar.gz 到 snapshots/overlay/<cache-id>/diff/
#         （处理 .wh..wh..opq 等 whiteout 文件）
#       - 处理 whiteout：在 diff/ 中留下标记，运行时构建 overlay 时再"删除"
# 5. 写入 metadata.DB（boltdb）：image / layer / tag 三类记录
# 6. 释放 Lease
```

#### 镜像存储结构

```
/var/lib/mini-docker/
├── metadata.db                         # BoltDB
│   └── Buckets: images / layers / tags / snapshots / leases / content
├── content/sha256/                     # Content Store
│   ├── abc123...                        # manifest blob
│   ├── def456...                        # OCI config blob
│   └── ghi789...                        # layer blob (tar.gz 压缩包)
└── snapshots/overlay/                  # OverlayFS Snapshotter
    ├── <layer1-cache-id>/
    │   └── diff/                        # 解压后的层1 文件
    ├── <layer2-cache-id>/
    │   └── diff/                        # 解压后的层2 文件
    └── <top-layer-cache-id>/
        └── diff/                        # 镜像最顶层

镜像运行时不需要预构建 rootfs —— 容器启动时由 Snapshotter.Mounts() 动态
收集所有层的 diff/ 路径，组成 OverlayFS 的 lowerdir，可插拔、可共享。
```

#### Content Store 详解

Content Store 只关心"按 digest 存 / 取不透明的二进制数据"，不关心数据是什么。三个核心角色：

- `fsStore`（仓库管理员）：知道 blob 存哪里（文件系统 + BoltDB 元信息）
- `contentWriter`（临时入库单）：边写边算 SHA256，调用 `Commit()` 时校验通过才正式落盘
- `contentReader`（按号取货）：按 digest 拿到 blob 字节流

详见 [containerd/content/contentStore详解.md](mini-docker/containerd/content/contentStore详解.md)

#### Whiteout 文件

OCI 规范用 `.wh.<filename>` 表达"删除下层文件"，用 `.wh..wh..opq` 表达"隐藏目录下所有下层文件"。layer tar 包里这种"白标"是删除操作的唯一表达方式（分层层是 append-only 的）。详见 [containerd/images/关于镜像每层是怎么来的.md](mini-docker/containerd/images/关于镜像每层是怎么来的.md)

#### 镜像构建（`docker build`）

> 对应代码：`builder/builder.go`

构建器按 Dockerfile 逐条执行指令，每条指令：

1. 基于"上一层"启动一个临时 OverlayFS（lower = 已构建层，upper = 空工作层）
2. 在 upper 层执行 `RUN` 指令 / `COPY` 文件
3. 退出容器，把 upper 层打包为新的 commit 快照，注册到 Snapshotter + metadata

支持的指令：`FROM` / `RUN` / `COPY` / `CMD` / `ENV` / `WORKDIR` / `EXPOSE` / `ENTRYPOINT` / `ARG`

---

### 6. 多进程分层 — Daemon / containerd / shim / runtime

> 对应代码：`daemon/`、`containerd/`、`shim/`、`runtime/`、`libcontainer/`

#### 为什么拆成多个进程？

| 痛点 | 多进程方案的解决方式 |
|------|---------------------|
| dockerd 崩溃时容器全部消失 | shim 是容器的父进程，dockerd 退出不影响 shim 也不影响容器 |
| dockerd 重启无法保持 -d 容器 | shim 维持 FIFO、I/O、日志，重启后 attach 还能继续 |
| 单进程内存爆炸 | containerd 单独进程，限制 image GC 不会影响容器运行 |
| 升级 dockerd 时需要停容器 | dockerd 升级时容器继续跑，shim 不需要动 |

#### 一次 `docker run -it alpine:3.18 /bin/sh` 的完整旅程

```
用户终端
  │
  │ ① sudo ./mini-docker run -it alpine:3.18 /bin/sh
  │
  ▼
[ mini-docker CLI 进程 ]    ← main.go 解析参数
  │                          发送 {"type":"run", "args":{...,"tty":"true",...}}
  │                          通过 /var/run/mini-docker/mini-docker.sock
  ▼
[ mini-docker daemon 进程 ] ← daemon/handler.go 处理 run
  │                          解析请求 → 调用 containerd 客户端
  │                          转发给 containerd
  ▼
[ mini-docker containerd ]  ← containerd/handler_task_linux.go 处理 create
  │                          1. 通过 Snapshotter.Prepare() 准备可写层
  │                             （从镜像的 layer chain 拉起 OverlayFS）
  │                          2. 生成 OCI Spec（containerd/spec 生成 config.json）
  │                          3. 创建 shim 进程
  │
  │  ② exec.Command("/proc/self/exe", "shim", id, bundle)
  │
  ▼
[ mini-docker shim 进程 ]   ← shim/shim.go
  │                          1. 打开 PTY（Master / Slave）
  │                          2. 启动 runtime 子进程，把 PTY Slave 传给它
  │
  │  ③ exec.Command("/proc/self/exe", "runtime", "create", "--bundle", ...)
  │
  ▼
[ mini-docker runtime 进程 ] ← runtime/runtime.go (对标 runc)
  │                           1. 读取 bundle/config.json (OCI Spec)
  │                           2. libcontainer 创建 LinuxContainer
  │
  │  ④ 通过 clone() + CLONE_NEWUTS|...|CLONE_NEWNET 创建容器 init
  │
  ▼
[ 容器 init 进程 ]            ← libcontainer/containerinit/init.go
  │                           1. 打开 FIFO（同步父 runtime）
  │                           2. SetupRootFS()：pivot_root + 挂 /proc
  │                           3. setHostname() / setCaps() / applySeccomp()
  │                           4. syscall.Exec("/bin/sh", ...) ← 替换为用户进程
  │
  │  ⑤ Daemon 在 shim 处 attach 双向 I/O
  │
  ▼
  用户的 /bin/sh 在容器内运行，stdin/stdout 连到 PTY Master
  shim 在 PTY Master 和 Unix Socket 之间做 io.Copy
  Daemon 在 Unix Socket 和 CLI 之间做 io.Copy
  CLI 直接连接到宿主机的 TTY
```

#### 通信协议

每两层之间都是 **JSON-over-UnixSocket**（真实 Docker 底下两层是 gRPC，本项目保持轻量）：

- **CLI ↔ Daemon**：`daemon.Request/Response`（`daemon/protocol.go`）+ 流式 I/O
- **Daemon ↔ containerd**：`containerd.Request/Response`（`containerd/api.go`）+ 流式 + 进度帧
- **containerd ↔ shim**：`types.ShimRequest/ShimResponse`（强类型字段）
- **shim ↔ runtime**：命令行参数 + FIFO（对齐 runc）

详见 [mini-docker/TODO-通信协议统一.md](mini-docker/TODO-通信协议统一.md)

#### shim 进程的关键设计

> 详细说明见 [shim/shim说明.md](mini-docker/shim/shim说明.md)

1. **shim 是容器的"养父"**：shim 进程是容器 init 的祖父（runtime → 容器 init），runtime 退出后 shim 仍在，shim 退出后容器才会被 reap
2. **shim 持有 PTY 和 I/O**：daemon 重启时通过 `attach` 命令重新连到 shim，恢复 I/O 转发
3. **控制 socket 和 I/O socket 分离**：shim 监听 `{runtimeDir}/{id}/shim.sock`，attach 走流式模式
4. **并发安全**：`sync.Once` 保护 `shutdownDone` 通道，防止 Daemon 重试导致重复 close 而 panic

---

### 7. GC — 基于 Lease 的垃圾回收

> 对应代码：`containerd/gc/`

mini-docker 实现了一个简化版 containerd GC，参考其**三色标记 + Lease 保护**模型。

#### 引用链

```
Tags → ImageID → LayerDigests → Content (blob)
ContainerInfo → Image → LayerDigests → Content (blob)
ContainerInfo → SnapshotID (containerID，容器可写层)
Snapshot → Parent → Parent (链式，递归标记)
Lease → Content/Snapshot (保护机制)
```

#### 两阶段

- **Mark（标记）**：从 Tags、ContainerInfo、Active 快照、Lease 出发，标记所有可达的对象
- **Sweep（清扫）**：遍历所有 content 和 snapshot，删除未被标记的

#### Lease 机制

- `pull` 镜像时创建 Lease（保护正在下载的 blob / 正在解压的快照）
- 容器运行时通过 Lease 锁定它依赖的 content 和 snapshot
- 业务结束时显式释放 Lease，对象进入可回收集合

---

## 容器创建全流程

这是 `sudo ./mini-docker run -m 100m -n mynet --name myalpine -it alpine:3.18 /bin/sh` 的完整执行链：

```
1. main.go: 解析命令行参数
   ├── -m 100m         → reqArgs["memory"] = "100m"
   ├── -n mynet        → reqArgs["network"] = "mynet"
   ├── --name myalpine → reqArgs["name"] = "myalpine"
   ├── -it             → reqArgs["tty"] = "true"
   ├── alpine:3.18     → reqArgs["image"] = "alpine:3.18"
   └── /bin/sh         → reqArgs["cmd"] = ["/bin/sh"]
   │
2. CLI → Daemon: 通过 /var/run/mini-docker/mini-docker.sock 发 run 请求
   │
3. Daemon.handler.Run() (daemon/handler.go)
   ├── 解析 image → 找到 snapshotKey
   ├── 通过 containerd.Client 发 create + start 请求
   │
4. containerd.handler_task.Create() (containerd/handler_task_linux.go)
   ├── 加载镜像 layers（从 metadata.DB 读出 digest 链）
   ├── Snapshotter.Prepare(containerID, parent=image.snapshotKey)
   │   → 创建可写层: containers/<id>/{upper, work, merged}
   │   → 通过 lowerDirs() 沿 parent 链递归收集所有 lowerdir
   ├── 生成 OCI Spec (spec/spec.go)
   │   → 写入 bundle/config.json
   │   → namespaces: pid, network, ipc, uts, mount
   │   → mounts: overlay (lowerdir=...:upperdir=...:workdir=...)
   │   → resources: memory.limit, cpu.shares
   │   → capabilities + seccomp
   ├── ShimManager.StartShim(containerID, bundlePath)
   │   → exec.Command("/proc/self/exe", "shim", id, bundle)
   │   → shim.SysProcAttr.Setsid = true
   │
5. shim.Run() (shim/shim.go)
   ├── 打开 PTY
   ├── 启动 runtime 子进程:
   │   exec.Command("/proc/self/exe", "runtime", "create",
   │                "--bundle", bundle, "--console", ptySlave)
   │
6. runtime.Create() (runtime/runtime.go)
   ├── 加载 OCI Spec
   ├── libcontainer 创建 LinuxContainer
   ├── 通过 clone(CLONE_NEWUTS|...|CLONE_NEWNET) 创建容器 init 进程
   │
7. 容器 init (libcontainer/containerinit/init.go)
   ├── HandleOCIInit()
   ├── 打开 bundle FIFO（与 runtime 同步）
   ├── 运行 CreateContainer hooks
   ├── SetupRootFS()：MS_PRIVATE 切断挂载传播
   │                  OverlayFS 挂载多层 lower + 自己的 upper
   │                  pivot_root 到 merged
   │                  挂 /proc /tmp
   ├── setHostname / applyCaps / applySeccomp
   ├── 写 state.json 到 bundle
   ├── 通过 FIFO 通知 runtime
   └── syscall.Exec("/bin/sh")  ← 替换为用户进程
   │
8. shim: 收到 FIFO 信号 → 通知 containerd
   containerd: 通过 attach 通知 Daemon
   Daemon: 把 CLI 的 socket、shim 的 socket、容器 PTY Master 三段串成 I/O 转发链
   │
9. 用户在终端看到的 /bin/sh:
   ├── hostname → "mini-docker" (UTS)
   ├── ps aux   → 只有 /bin/sh (PID)
   ├── ip addr  → 172.19.0.x (Network)
   ├── ls /     → 完整 rootfs (Mount + OverlayFS)
   └── memory   → 限制为 100MB (Cgroup)
```

---

## 命令使用指南

### 守护进程

```bash
sudo ./mini-docker daemon              # 启动 Daemon（自动拉起 containerd 子进程）
sudo ./mini-docker containerd          # 独立启动 containerd 进程（调试用）
```

### 镜像管理

```bash
sudo ./mini-docker pull <image>[:tag]              # 从 Registry 拉取（支持 mirror）
sudo ./mini-docker images                          # 列出本地镜像
sudo ./mini-docker rmi <image>                     # 删除本地镜像
sudo ./mini-docker build -t <name:tag> <context>   # Dockerfile 构建
```

支持的镜像引用：
- `alpine` / `alpine:3.18` — Docker Hub（自动加 `library/`）
- `library/alpine` / `docker.io/alpine` — 显式仓库
- `myreg.com/myapp:v1` — 私有 Registry
- 简单名（不含 `.` 或 `/`）— 本地构建（busybox rootfs）

### 容器生命周期

```bash
sudo ./mini-docker run [选项] <镜像> <命令>         # 创建并运行
sudo ./mini-docker exec <容器ID> <命令>            # 在运行中容器内执行
sudo ./mini-docker ps [-a]                         # 列出容器（-a 包含已停止）
sudo ./mini-docker start <容器ID>                  # 启动已停止容器
sudo ./mini-docker stop <容器ID>                   # 停止运行中容器
sudo ./mini-docker pause <容器ID>                  # 冻结
sudo ./mini-docker unpause <容器ID>                # 恢复
sudo ./mini-docker rm <容器ID>                     # 删除容器
sudo ./mini-docker logs <容器ID>                   # 查看日志
sudo ./mini-docker events                          # 实时事件流
```

### run 选项

```bash
# 基础
-it                                       # 交互式（分配 TTY）
-d                                        # 后台运行
--name <name>                             # 容器命名
--restart no|always|on-failure[:max]      # 重启策略

# 资源限制
-m <内存>                                 # 内存限制（100m, 1g）
-c <份额>                                 # CPU shares（默认 1024）

# 网络
-n <网络名>                                # 加入网络
--network=none|host                       # 无网络 / 主机网络
-p <宿主端口>:<容器端口>                  # 端口映射

# 存储
-v <卷挂载>                                # 多次指定：-v /host:/c -v vol:/data

# 示例
sudo ./mini-docker run -d --name web -p 8080:80 -v /data:/var/www nginx:alpine
```

### 网络管理

```bash
sudo ./mini-docker network create <name>          # 创建自定义网桥
sudo ./mini-docker network list                   # 列出所有网络
sudo ./mini-docker network delete <name>          # 删除网络
```

### 卷管理

```bash
sudo ./mini-docker volume create <name>           # 创建命名卷
sudo ./mini-docker volume list                    # 列出卷
sudo ./mini-docker volume rm <name>               # 删除卷
sudo ./mini-docker volume inspect <name>          # 查看卷详情
```

### 内部命令（OCI runtime + shim）

```bash
# 对标 runc 的命令
./mini-docker runtime create --bundle <dir> --console-socket <sock> <id>
./mini-docker runtime start <id>
./mini-docker runtime kill <id> <signal>
./mini-docker runtime delete <id>
./mini-docker runtime state <id>
./mini-docker runtime exec <id> <cmd>

# 对标 containerd-shim
./mini-docker shim <id> <bundle>
```

---

## 代码导读

### 推荐阅读顺序

#### 阶段一：理解进程分层和 reexec

| 文件 | 重点 |
|------|------|
| `main.go:30-90` | 入口分发：CLI 命令 vs init / runtime / shim 内部命令 |
| `main.go:137-219` | `daemonCommand` / `startContainerdProcess` — 怎么拉起子进程 |
| `daemon/daemon.go` | Daemon 主循环：监听 Socket、接收请求、派发 handler |
| `containerd/server_linux.go` | containerd 主循环 |
| `shim/shim.go` | shim 进程：拉起 runtime、桥接 PTY 与 socket |

**核心问题**：为什么不直接一个进程搞定？— 答：解耦、稳定性、升级。

#### 阶段二：理解容器运行时的内核调用

| 文件 | 重点 |
|------|------|
| `libcontainer/containerinit/init.go` | 容器 init 进程入口 |
| `libcontainer/containerinit/rootfs_linux.go` | pivot_root + OverlayFS |
| `libcontainer/cgroups/fs_linux.go` | cgroup v1 资源限制 |
| `libcontainer/cgroups/fs2_linux.go` | cgroup v2 统一层级 |
| `libcontainer/configs/capabilities.go` | Linux capabilities |
| `libcontainer/configs/seccomp.go` | seccomp BPF 规则 |

**动手实验**：在 `libcontainer/cgroups/fs_linux.go` 中给 `memory.limit_in_bytes` 加日志，看看 `-m 100m` 实际写入了什么。

#### 阶段三：理解镜像管理（containerd 模型）

| 文件 | 重点 |
|------|------|
| `containerd/images/service.go` | Image Service：Pull/Remove/List/Resolve |
| `containerd/images/image.go` | `pull` / `pullFromRegistry` / `pullLocal` |
| `containerd/images/registry.go` | Docker Registry V2 HTTP 客户端 + Bearer Token |
| `containerd/content/filesys.go` | Content Store 文件系统实现 |
| `containerd/snapshots/snapshotter.go` | Snapshotter 接口 |
| `containerd/snapshots/overlay/overlay.go` | OverlayFS 实现 + UnpackLayer |
| `containerd/metadata/db.go` | BoltDB 封装 |

**核心问题**：blob / diff / metadata 三个目录为什么不能合并？— 答：解耦，Content Store 不知道里面是 manifest 还是 layer，Snapshotter 不知道有镜像概念。

#### 阶段四：理解多进程通信

| 文件 | 重点 |
|------|------|
| `daemon/protocol.go` | CLI ↔ Daemon 协议 |
| `daemon/handler.go` | Daemon 端处理器 |
| `containerd/api.go` | Daemon ↔ containerd 协议 |
| `containerd/api_linux.go` | Daemon → containerd 客户端 |
| `containerd/handler_task_linux.go` | containerd 端 Task 处理器 |
| `types/types.go` | ShimRequest/ShimResponse 强类型 |

**动手实验**：在 `daemon/handler.go` 的 `run` 处理里加一行 `log.Println(req.Args)`，看看一次 run 请求到底传了什么。

#### 阶段五：理解网络、卷、构建

| 文件 | 重点 |
|------|------|
| `network/network_linux.go` | Bridge + veth + iptables NAT/DNAT |
| `volume/volume.go` | Bind mount + 命名卷 |
| `builder/builder.go` | Dockerfile 解析 + 增量 OverlayFS 构建 |

---

## 与真实 Docker 的差距

### 已实现的核心功能

| 功能 | mini-docker | Docker | 底层技术 |
|------|------------|--------|---------|
| 多进程分层 (CLI/Daemon/containerd/shim/runtime) | ✅ 5 进程 | ✅ 5 进程 | Unix Socket + /proc/self/exe |
| 命名空间隔离 | ✅ 5 种 | ✅ 6 种（含 User） | clone + Cloneflags |
| Cgroup 资源限制 | ✅ v1 + v2 | ✅ v1 + v2 | memory/CPU cgroup 文件 |
| RootFS 隔离 | ✅ pivot_root + OverlayFS | ✅ | mount + pivot_root |
| 多层镜像 | ✅ OverlayFS | ✅ | Snapshotter.Mounts() |
| Content Store | ✅ (fsStore + BoltDB) | ✅ | SHA256 blob 存储 |
| 镜像元数据 | ✅ BoltDB | ✅ BoltDB | boltdb buckets |
| 镜像 GC | ✅ 三色标记 + Lease | ✅ | mark + sweep |
| Registry 拉取 | ✅ Docker V2 + mirror | ✅ | HTTP/Token/blob |
| Dockerfile 构建 | ✅ FROM/RUN/COPY/CMD/ENV/... | ✅ | OverlayFS 增量 |
| Bridge 网络 | ✅ veth + bridge + NAT | ✅ | ip link / iptables |
| 端口映射 | ✅ DNAT | ✅ | iptables DNAT |
| 数据卷 | ✅ Bind + Named | ✅ | bind mount |
| 容器生命周期 | ✅ run/exec/start/stop/pause/rm/ps/logs/events | ✅ | signal + JSON |
| docker exec | ✅ setns | ✅ | /proc/<pid>/ns/* |
| TTY | ✅ PTY (openpty) | ✅ | pty + winsize |
| OCI runtime spec | ✅ config.json 生成与加载 | ✅ | OCI runtime-spec |
| libcontainer | ✅ (简化版) | ✅ | runc/libcontainer |
| shim 进程 | ✅ | ✅ | shim 维持 I/O |
| 重启策略 | ✅ no/always/on-failure | ✅ | Daemon event loop |
| 后台容器 (-d) | ✅ | ✅ | Daemon 管理 |
| 事件流 (events) | ✅ | ✅ | event bus |

### 未实现的重要功能

| 功能 | 重要程度 | 说明 |
|------|---------|------|
| **User Namespace** | 🔴 高 | 容器内 root = 宿主机 root（最危险） |
| **完整 Linux Capabilities 限制** | 🔴 高 | 已生成 Spec 但默认未做最小化裁剪 |
| **Seccomp 默认 profile** | 🔴 高 | 已生成 Spec 但默认未加载 |
| **docker push** | 🟡 中 | 内容已落 Content Store，未实现 push API |
| **Cgroup v2 systemd cgroup driver** | 🟡 中 | 仅支持 fs / fs2 驱动 |
| **host / none / overlay 网络模式** | 🟡 中 | 只有 bridge |
| **镜像签名验证 (Content Trust)** | 🟢 低 | 无 cosign / Notary 集成 |
| **AppArmor / SELinux** | 🟢 低 | 无强制访问控制 |
| **健康检查 (HEALTHCHECK)** | 🟡 中 | 已留 OCI hook 位但未做主动探针 |
| **多 Registry 同时配置** | 🟢 低 | mirror 列表只对 Docker Hub 生效 |
| **GPU / device cgroup** | 🟢 低 | 无设备透传 |
| **namespace 文件持久化（容器迁移）** | 🟢 低 | shim 不持有可移动的容器引用 |
| **gRPC 通信** | 🟢 低 | 现在是 JSON，未来可对齐 Docker 切到 gRPC（见 TODO-通信协议统一.md） |

### 未实现的协议层

| 能力 | 状态 | 说明 |
|------|------|------|
| dockerd ↔ containerd 多路复用 | ❌ | 单连接单流 |
| gRPC / TTRPC | ❌ | 自造 JSON-over-UnixSocket |
| Streaming 多路复用 | ❌ | 需要独立 Socket |

> ⚠️ **安全警告**：由于缺少 User Namespace、Capabilities 默认裁剪和 Seccomp 默认 profile，mini-docker 容器内的进程拥有宿主机的完整 root 权限。**绝对不要在生产环境使用，也不要运行不受信任的程序！**

---

## 学习路线建议

### 入门路线（1-2 天）

1. **阅读本文档**，建立对容器技术的整体认知
2. **编译并跑通**第一个容器，验证 Namespace / Cgroup / OverlayFS 隔离效果
3. **阅读 `main.go`**，理解 reexec 模式：同一个二进制怎么区分 CLI / init / runtime / shim
4. **动手实验**：修改 `libcontainer/configs/config.go` 中 namespace 列表，去掉某个 flag，观察容器行为变化

### 进阶路线（3-5 天）

5. **阅读 `libcontainer/containerinit/`**，理解 pivot_root + OverlayFS + /proc 重新挂载
6. **阅读 `libcontainer/cgroups/`**，对比 v1 / v2 两个实现，理解统一层级模型
7. **阅读 `network/network_linux.go`**，理解 veth pair + Bridge + iptables NAT
8. **动手实验**：手动 `ip link` / `iptables` 命令跟一遍代码

### 深入路线（1-2 周）

9. **阅读 `containerd/images/service.go` 和 `image.go`**，理解 containerd 的"service + 私有 pull 函数"分层
10. **阅读 `containerd/content/` 和 `containerd/snapshots/`**，理解 content-addressable 模型
11. **阅读 `containerd/gc/`**，理解三色标记 + Lease
12. **阅读 `daemon/` / `containerd/` / `shim/` 三层之间的协议和调用关系**
13. **阅读 `libcontainer/`**，理解 OCI Spec 如何驱动容器创建

### 扩展路线

14. **实现 User Namespace**：添加 `CLONE_NEWUSER`，在 `configs/namespace.go` 中启用
15. **实现 Capabilities 最小化**：在 `libcontainer/configs/capabilities.go` 中裁剪默认集合
16. **实现 Seccomp 默认 profile**：在 `libcontainer/configs/seccomp.go` 中加载白名单
17. **统一三层通信协议**：参考 [TODO-通信协议统一.md](mini-docker/TODO-通信协议统一.md)
18. **实现 docker push**：复用 Content Store，加 push API

### 扩展阅读

- [Docker 官方文档](https://docs.docker.com/)
- [containerd 架构设计](https://github.com/containerd/containerd)
- [OCI runtime-spec](https://github.com/opencontainers/runtime-spec)
- [OCI image-spec](https://github.com/opencontainers/image-spec)
- [Linux Namespace 手册](https://man7.org/linux/man-pages/man7/namespaces.7.html)
- [Linux Cgroup 手册](https://man7.org/linux/man-pages/man7/cgroups.7.html)
- [runc 源码](https://github.com/opencontainers/runc) — Docker 底层的容器运行时
- [自己动手写 Docker](https://book.douban.com/subject/27070705/) — 中文经典教材

---

## 常见问题

### Q: 为什么必须在 Linux 上运行？

A: 容器技术依赖 Linux 内核的三大特性：Namespace、Cgroup、OverlayFS。这些都是 Linux 内核特有的功能，Windows 和 macOS 的内核不支持。WSL2 运行的是真正的 Linux 内核，所以可以使用。

### Q: 为什么需要 root 权限？

A: 创建 Namespace、操作 Cgroup、执行 pivot_root、配置 iptables 都需要 root 权限。Docker 也是通过 docker daemon（root 进程）来执行这些操作的。

### Q: Daemon / containerd / shim 三个进程必须同时存在吗？

A: 容器运行时三个进程都需要。Daemon 负责接收 CLI 请求；containerd 负责镜像管理和 shim 管理；shim 是容器 init 的"养父"，负责持有 I/O / FIFO / 控制 socket。如果你只做镜像管理（pull/images/rmi）只需要 containerd + Daemon；如果只做容器运行需要全部三个。

### Q: 容器内为什么看不到宿主机的进程？

A: 因为 PID Namespace 隔离了进程 ID 空间。在新的 PID Namespace 中，容器进程从 PID 1 开始编号。同时，init 进程在 pivot_root 之后重新挂载了 `/proc`，所以 `ps aux` 只显示容器内的进程。

### Q: 容器内为什么 hostname 是 mini-docker？

A: 因为 UTS Namespace 隔离了主机名。`libcontainer/containerinit/init.go` 在 pivot_root 之后调用 `sethostname("mini-docker")`，这只影响当前 UTS Namespace，不会改变宿主机的主机名。

### Q: `-m 100m` 是如何限制内存的？

A: 向 `/sys/fs/cgroup/memory/<cgroup>/memory.limit_in_bytes`（v1）或 `/sys/fs/cgroup/<cgroup>/memory.max`（v2）写入 `104857600`。当容器进程尝试使用超过限制的内存时，内核会触发 OOM Killer 杀死进程。

### Q: docker exec 是怎么实现的？

A: 通过 `setns()` 系统调用。打开目标容器的 `/proc/<pid>/ns/` 下的文件，调用 `setns()` 将当前进程加入容器的 Namespace，然后在新 Namespace 中执行命令。

### Q: /proc/self/exe 是什么？

A: 它是指向当前可执行文件自身的符号链接。mini-docker 通过它重新执行自己，但传入不同的子命令（`init` / `runtime` / `shim`），让程序知道自己在哪一层运行。这就是 Docker 的 "reexec" 模式。

### Q: 为什么 pivot_root 比 chroot 更安全？

A: chroot 只改变进程对 "/" 的视角，但不改变当前工作目录，可以通过相对路径 ".." 逃逸。pivot_root 原子性地切换根目录，旧根被移动到新位置后卸载，无法通过路径逃逸。

### Q: Content Store 和 Snapshotter 的关系？

A: Content Store 存的是"压缩的、不透明的 blob"（manifest / config / layer.tar.gz），按 SHA256 寻址；Snapshotter 存的是"解压后的文件系统 diff/"，按 cache-id 寻址。拉镜像时 blob 写到 Content Store，tar.gz 解压到 Snapshotter。运行时 Snapshotter.Mounts() 提供 overlay 的 lowerdir，Content Store 不会被运行时直接使用。

### Q: 容器和虚拟机的根本区别是什么？

A: 容器共享宿主机内核，是"特殊的进程"；虚拟机有独立内核，是"完整的计算机"。容器启动快、体积小，但安全性较弱（共享内核意味着内核漏洞会影响所有容器）。

---

## 文档索引

- [containerd/content/contentStore详解.md](mini-docker/containerd/content/contentStore详解.md) — Content Store 内部机制
- [containerd/images/关于镜像每层是怎么来的.md](mini-docker/containerd/images/关于镜像每层是怎么来的.md) — 镜像层 / whiteout 详解
- [shim/shim说明.md](mini-docker/shim/shim说明.md) — shim 进程设计 + `sync.Once` 并发安全
- [mini-docker交互式容器-it全链路解析.md](mini-docker/mini-docker交互式容器-it全链路解析.md) — `run -it` 的 I/O 转发链路
- [TODO-通信协议统一.md](mini-docker/TODO-通信协议统一.md) — 三层通信协议演进计划
- [Unix系统调用详解.md](mini-docker/Unix系统调用详解.md) — 本项目用到的 Linux syscall 整理
- [宿主机与容器文件系统映射关系.md](mini-docker/宿主机与容器文件系统映射关系.md) — bind mount / volume / overlay 的关系
- [启动的各种隔离namespace的时机.md](mini-docker/启动的各种隔离namespace的时机.md) — 各 Namespace 的创建顺序
- [namespace是否复制了宿主机文件系统.md](mini-docker/namespace是否复制了宿主机文件系统.md) — Mount Namespace 的复制语义
- [mount参数详解.md](mini-docker/mount参数详解.md) — 各 mount flag 的作用
- [runtime/runtime_create_args.md](mini-docker/runtime/runtime_create_args.md) — runtime create 命令行参数
