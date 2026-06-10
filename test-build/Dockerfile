# 测试 Dockerfile —— 用于验证 mini-docker 的镜像构建功能
# 这个 Dockerfile 展示了所有支持的指令

FROM ubuntu:24.04

# 设置环境变量
ENV APP_NAME=test-app
ENV VERSION=1.0.0
ENV HOME=/app

# 设置工作目录
WORKDIR /app

# 创建一个简单的测试文件（通过 RUN 指令）
RUN echo "Hello from mini-docker build!" > /app/hello.txt
RUN echo "App: $APP_NAME, Version: $VERSION" >> /app/hello.txt

# 复制本地文件到镜像中
COPY app.sh /app/app.sh
RUN chmod +x /app/app.sh

# 暴露端口
EXPOSE 8080
EXPOSE 3000

# 设置默认启动命令
CMD ["/bin/cat", "/app/hello.txt"]
