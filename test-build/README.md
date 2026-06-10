# mini-docker 构建测试目录

这个目录包含用于测试 mini-docker 镜像构建功能的文件。

## 文件说明

| 文件 | 说明 |
|------|------|
| `Dockerfile` | 测试用的 Dockerfile，使用了所有支持的指令（FROM、RUN、COPY、ENV、WORKDIR、EXPOSE、CMD） |
| `app.sh` | 测试脚本，会被 COPY 指令复制到镜像中 |

## 支持的 Dockerfile 指令

根据 [builder/builder.go](../builder/builder.go) 的实现，目前支持以下指令：

- **FROM** `<image>` — 基础镜像（必须是第一条指令）
- **RUN** `<command>` — 执行命令（生成新层）
- **COPY** `<src>` `<dst>` — 复制文件（生成新层）
- **ENV** `<key>=<value>` — 环境变量（元数据）
- **WORKDIR** `<path>` — 工作目录（元数据）
- **EXPOSE** `<port>` — 暴露端口（元数据）
- **CMD** `<command>` — 默认启动命令（元数据，不生成层）

## 使用方法

> **注意**：以下所有命令均在 `mini-docker` 目录下执行，不需要进入 `test-build` 子目录。

### 前提条件

1. 确保 mini-docker daemon 正在运行：
   ```bash
   sudo ./mini-docker daemon
   ```

2. 确保已经拉取了基础镜像：
   ```bash
   sudo ./mini-docker pull ubuntu:24.04
   ```

### 构建镜像

```bash
# 构建镜像（指定 test-build 作为构建上下文目录）
sudo ./mini-docker build -t test-app:latest test-build
```

### 验证构建结果

```bash
# 查看镜像列表
sudo ./mini-docker images

# 使用构建的镜像运行容器
sudo ./mini-docker run -t test-app:latest

# 或者交互式运行
sudo ./mini-docker run -it test-app:latest /bin/bash
```

## 预期输出

构建成功后，镜像 `test-app:latest` 应该包含：

- 基础 Ubuntu 24.04 文件系统
- `/app/hello.txt` 文件，内容为 "Hello from mini-docker build!"
- `/app/app.sh` 可执行脚本
- 环境变量 `APP_NAME=test-app`、`VERSION=1.0.0`
- 工作目录设置为 `/app`
- 暴露端口 8080 和 3000（元数据记录）

运行容器时的默认命令会显示 `hello.txt` 的内容。
