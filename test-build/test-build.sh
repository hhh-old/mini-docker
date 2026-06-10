#!/bin/bash
# test-build.sh —— 自动化测试 mini-docker 镜像构建功能

set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$SCRIPT_DIR"

echo "=========================================="
echo "  mini-docker 镜像构建功能测试"
echo "=========================================="
echo ""

# 检查是否在 Linux 环境
if [[ "$OSTYPE" != "linux-gnu"* ]]; then
    echo "⚠️  警告: 当前不是 Linux 环境，容器功能可能无法正常工作"
    echo "    建议在 Linux / WSL2 环境下运行此测试"
    echo ""
fi

# 检查权限
if [ "$EUID" -ne 0 ]; then
    echo "❌ 错误: 需要 root 权限运行（操作 namespace/cgroup/mount 需要）"
    echo "    请使用: sudo $0"
    exit 1
fi

# 检查 mini-docker 可执行文件
MINI_DOCKER="$SCRIPT_DIR/../mini-docker"
if [ ! -f "$MINI_DOCKER" ]; then
    echo "❌ 错误: 找不到 mini-docker 可执行文件"
    echo "    请先编译项目: cd .. && go build -o mini-docker"
    exit 1
fi

echo "✓ mini-docker 路径: $MINI_DOCKER"

# 检查 daemon 是否运行
if ! pgrep -f "mini-docker daemon" > /dev/null 2>&1; then
    echo ""
    echo "⚠️  mini-docker daemon 未运行"
    echo "    请先启动 daemon: sudo $MINI_DOCKER daemon"
    echo ""
    exit 1
fi

echo "✓ mini-docker daemon 正在运行"

# 检查基础镜像是否存在
echo ""
echo "--- 检查基础镜像 ---"
if ! "$MINI_DOCKER" images | grep -q "ubuntu"; then
    echo "⚠️  基础镜像 ubuntu:24.04 不存在，尝试拉取..."
    "$MINI_DOCKER" pull ubuntu:24.04
    if [ $? -ne 0 ]; then
        echo "❌ 拉取基础镜像失败"
        exit 1
    fi
    echo "✓ 基础镜像拉取成功"
else
    echo "✓ 基础镜像已存在"
fi

# 构建测试镜像
echo ""
echo "--- 构建测试镜像 ---"
echo "  Dockerfile: $SCRIPT_DIR/Dockerfile"
echo "  上下文目录: $SCRIPT_DIR"
echo "  镜像标签: test-app:latest"
echo ""

if "$MINI_DOCKER" build -t test-app:latest "$SCRIPT_DIR"; then
    echo ""
    echo "✅ 镜像构建成功!"
else
    echo ""
    echo "❌ 镜像构建失败"
    exit 1
fi

# 验证镜像
echo ""
echo "--- 验证构建结果 ---"
if "$MINI_DOCKER" images | grep -q "test-app"; then
    echo "✓ 镜像 test-app 已注册到镜像列表"
else
    echo "⚠️  警告: 镜像 test-app 未在镜像列表中找到"
fi

# 运行容器测试
echo ""
echo "--- 运行容器测试 ---"
echo "  执行默认命令 (CMD)..."
CONTAINER_ID=$("$MINI_DOCKER" run -t test-app:latest 2>/dev/null | tail -1)
if [ -n "$CONTAINER_ID" ]; then
    echo "✓ 容器启动成功，ID: $CONTAINER_ID"

    # 等待容器完成
    sleep 2

    # 查看日志
    echo ""
    echo "--- 容器日志 ---"
    "$MINI_DOCKER" logs "$CONTAINER_ID" 2>/dev/null || true

    # 清理容器
    echo ""
    echo "--- 清理测试容器 ---"
    "$MINI_DOCKER" rm "$CONTAINER_ID" 2>/dev/null || true
    echo "✓ 测试容器已清理"
else
    echo "⚠️  容器启动可能失败，请手动检查"
fi

echo ""
echo "=========================================="
echo "  测试完成!"
echo "=========================================="
echo ""
echo "你可以手动运行以下命令进行更多测试:"
echo "  sudo $MINI_DOCKER run -it test-app:latest /bin/bash   # 交互式运行"
echo "  sudo $MINI_DOCKER run -it test-app:latest /app/app.sh # 运行测试脚本"
echo ""
