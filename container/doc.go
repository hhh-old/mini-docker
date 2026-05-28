package container

/*
=======================================================================
  Docker 核心原理 —— 从零理解容器技术
=======================================================================

  容器到底是什么？一句话概括：

    容器 = Namespace（隔离）+ Cgroup（限制）+ RootFS（文件系统）

  ┌─────────────────────────────────────────────────────────────────┐
  │                    Docker 容器技术栈                             │
  ├─────────────────────────────────────────────────────────────────┤
  │                                                                 │
  │  用户层面:  docker run -it -m 100m ubuntu /bin/bash             │
  │       ↓                                                         │
  │  Docker 引擎:  解析参数，协调各组件                              │
  │       ↓                                                         │
  │  ┌─────────────────────────────────────────────────────────┐   │
  │  │  containerd  →  containerd-shim  →  runc                │   │
  │  │       容器运行时管理层                                    │   │
  │  └─────────────────────────────────────────────────────────┘   │
  │       ↓                                                         │
  │  ┌──────────┐  ┌──────────┐  ┌──────────┐                     │
  │  │Namespace │  │  Cgroup  │  │  RootFS  │   ← Linux 内核特性  │
  │  │  隔离    │  │  限制    │  │ 文件系统 │                     │
  │  └──────────┘  └──────────┘  └──────────┘                     │
  │       ↓              ↓              ↓                           │
  │  ┌─────────────────────────────────────────────────────────┐   │
  │  │                  Linux Kernel                            │   │
  │  └─────────────────────────────────────────────────────────┘   │
  └─────────────────────────────────────────────────────────────────┘

  本项目（mini-docker）的代码结构完全对应上述架构：

  container/namespace.go  → Namespace 隔离
  cgroup/cgroup.go       → Cgroup 资源限制
  container/rootfs.go    → RootFS 文件系统
  container/container.go → 容器生命周期管理
  image/image.go         → 镜像管理
  network/network.go     → 网络管理

=======================================================================
  Docker vs 虚拟机
=======================================================================

  ┌──────────────┬────────────────────┬────────────────────┐
  │              │     Docker 容器     │     虚拟机          │
  ├──────────────┼────────────────────┼────────────────────┤
  │ 隔离方式     │ Namespace (进程级)  │ Hypervisor (硬件级) │
  │ 资源限制     │ Cgroup             │ 虚拟硬件           │
  │ 内核共享     │ 共享宿主机内核      │ 独立内核           │
  │ 启动速度     │ 毫秒级             │ 分钟级             │
  │ 镜像大小     │ MB 级              │ GB 级              │
  │ 性能损耗     │ 接近原生           │ 5-10%              │
  │ 安全性       │ 较弱（共享内核）    │ 较强（硬件隔离）    │
  └──────────────┴────────────────────┴────────────────────┘

  容器本质上是"特殊的进程"，不是"轻量级虚拟机"。
  容器内的进程直接运行在宿主机内核上，只是被 Namespace 隔离了。

=======================================================================
  容器创建的完整流程（对应代码）
=======================================================================

  1. 用户执行: mini-docker run -m 100m myimage /bin/sh
     ↓
  2. main.go 解析参数，构造 ContainerInfo
     ↓
  3. Daemon handler.runWithID() 被调用:
     a. 准备 RootFS 路径
     b. fork 子进程，设置 Cloneflags（创建新 Namespace）
        → 对应 namespace_linux.go: setCloneFlags()
     c. 子进程执行 /proc/self/exe init <rootfs> <cmd>
        → 这是 Docker 的 "reexec" 模式
     ↓
  4. 子进程进入 InitProcess():
     a. SetupRootFS() → pivot_root 切换根目录
        → 对应 rootfs_linux.go: SetupRootFS()
     b. setHostname() → 设置主机名（UTS Namespace 生效）
     c. syscall.Exec() → 替换进程映像为用户命令
     ↓
  5. 父进程继续:
     a. CgroupManager.Apply() → 设置资源限制
        → 对应 cgroup/cgroup_linux.go: Apply()
     b. NetworkManager.Connect() → 配置网络
        → 对应 network/network_linux.go: Connect()
     c. saveContainerInfo() → 保存容器元数据
     ↓
  6. 容器运行中！

=======================================================================
  关键概念对照表
=======================================================================

  Docker 命令            →  mini-docker 实现           →  底层技术
  ─────────────────────────────────────────────────────────────────
  docker run             →  handler.runWithID()         →  shim → runtime create/start
  docker exec            →  handler.handleExec()        →  shim(nsenter) → setns
  docker stop            →  handler.handleStop()        →  shim(KillTask) → unix.Kill
  docker rm              →  handler.handleRm()          →  清理 网络/overlay/cgroup/shim
  docker ps              →  container.ListContainers()   →  读取 JSON 元数据
  docker pull            →  image.Pull()                →  创建 rootfs 目录
  docker images          →  image.ListImages()          →  读取 JSON 元数据
  docker network create  →  network.CreateNetwork()    →  ip link add bridge
  -m 100m                →  cgroup.setMemoryLimit()      →  memory.limit_in_bytes
  --cpu-shares 512       →  cgroup.setCpuShares()        →  cpu.shares
  docker pause           →  handler.handlePause()        →  shim → libcontainer.Pause → cgroup.Freeze

=======================================================================
*/
