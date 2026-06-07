//go:build linux

package containerd

/*
=======================================================================
  镜像管理处理器（对齐 Docker: containerd Image Service）

  处理 Daemon 发来的镜像相关请求，包括：
  - 拉取镜像（流式进度推送）
  - 列出/删除/查看/解析镜像
  - 注册已构建的镜像

=======================================================================
*/

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strings"

	"mini-docker/containerd/images"
	"mini-docker/containerstore"
	"mini-docker/utils"
)

// handlePullImage 拉取镜像，通过连接流式推送进度和最终结果
func (c *Containerd) handlePullImage(req Request, conn net.Conn) Response {
	imageName := req.Args["image"]
	if imageName == "" {
		// 流式请求：直接通过连接发送错误结果帧
		resultFrame := ProgressFrameData{
			Type:    ResultFrame, //结束帧
			Status:  "error",
			Message: "需要指定镜像名",
		}
		data, _ := json.Marshal(resultFrame)
		data = append(data, '\n')
		conn.Write(data)
		conn.Close()
		return Response{}
	}
	if c.imageService == nil {
		resultFrame := ProgressFrameData{
			Type:    ResultFrame, //结束帧
			Status:  "error",
			Message: "镜像服务未初始化",
		}
		data, _ := json.Marshal(resultFrame)
		data = append(data, '\n')
		conn.Write(data)
		conn.Close()
		return Response{}
	}

	// 定义进度回调：将进度帧写入连接
	progress := func(status, message string) {
		frame := ProgressFrameData{
			Type:    ProgressFrame,
			Status:  status,
			Message: message,
		}
		data, _ := json.Marshal(frame)
		data = append(data, '\n') // JSON 分帧分隔符
		conn.Write(data)
	}

	ctx := context.Background()
	info, err := c.imageService.Pull(ctx, imageName, progress)

	// 发送最终结果帧
	if err != nil {
		resultFrame := ProgressFrameData{
			Type:    ResultFrame,
			Status:  "error",
			Message: fmt.Sprintf("拉取镜像失败: %v", err),
		}
		data, _ := json.Marshal(resultFrame)
		data = append(data, '\n')
		conn.Write(data)
		conn.Close()
		return Response{}
	}

	resultFrame := ProgressFrameData{
		Type:    ResultFrame,
		Status:  "complete",
		Message: fmt.Sprintf("镜像 %s 拉取成功", imageName),
		Data:    info,
	}
	data, _ := json.Marshal(resultFrame)
	data = append(data, '\n')
	conn.Write(data)
	conn.Close()
	return Response{}
}

// handleListImages 列出本地所有镜像
func (c *Containerd) handleListImages(req Request) Response {
	if c.imageService == nil {
		return Response{Success: false, Message: "镜像服务未初始化"}
	}
	ctx := context.Background()
	images, err := c.imageService.List(ctx)
	if err != nil {
		return Response{Success: false, Message: fmt.Sprintf("列出镜像失败: %v", err)}
	}
	return Response{Success: true, Data: images}
}

// handleRemoveImage 删除本地镜像，若有容器正在使用则拒绝删除
func (c *Containerd) handleRemoveImage(req Request) Response {
	imageName := req.Args["image"]
	if imageName == "" {
		return Response{Success: false, Message: "需要指定镜像名"}
	}
	if c.imageService == nil {
		return Response{Success: false, Message: "镜像服务未初始化"}
	}

	if tag := req.Args["tag"]; tag != "" {
		imageName = imageName + ":" + tag
	}

	// 检查是否有容器正在使用该镜像
	ctx := context.Background()
	containers, err := containerstore.ListContainers()
	if err == nil {
		// 解析待删除镜像的 name:tag
		rmName, rmTag := utils.ParseImageTag(imageName)
		if rmTag == "" {
			rmTag = "latest"
		}
		for _, container := range containers {
			// 容器引用的是 imageName（可能不含 tag），需要做宽松匹配
			cName, cTag := utils.ParseImageTag(container.Image)
			if cTag == "" {
				cTag = "latest"
			}
			// 如果镜像名和标签都精确匹配，则拒绝删除
			if cName == rmName && cTag == rmTag {
				return Response{Success: false, Message: fmt.Sprintf("镜像 %s 正被容器 %s (%s) 使用，请先删除容器", imageName, container.ID, container.Name)}
			}
		}
	}

	if err := c.imageService.Remove(ctx, imageName); err != nil {
		return Response{Success: false, Message: fmt.Sprintf("删除镜像失败: %v", err)}
	}
	return Response{Success: true}
}

// handleInspectImage 查看指定镜像的详细信息（manifest 等）
func (c *Containerd) handleInspectImage(req Request) Response {
	imageRef := req.Args["image"]
	if imageRef == "" {
		return Response{Success: false, Message: "需要指定镜像名"}
	}
	if c.imageService == nil {
		return Response{Success: false, Message: "镜像服务未初始化"}
	}

	ctx := context.Background()
	manifest, err := c.imageService.Inspect(ctx, imageRef)
	if err != nil {
		return Response{Success: false, Message: fmt.Sprintf("查看镜像详情失败: %v", err)}
	}
	return Response{Success: true, Data: manifest}
}

// handleResolveImage 解析镜像引用，返回 rootfs 路径和 snapshot key
func (c *Containerd) handleResolveImage(req Request) Response {
	imageRef := req.Args["image"]
	if imageRef == "" {
		return Response{Success: false, Message: "需要指定镜像名"}
	}
	if c.imageService == nil {
		return Response{Success: false, Message: "镜像服务未初始化"}
	}

	ctx := context.Background()
	info, err := c.imageService.Resolve(ctx, imageRef)
	if err != nil {
		return Response{Success: false, Message: fmt.Sprintf("解析镜像路径失败: %v", err)}
	}
	return Response{Success: true, Data: map[string]interface{}{"rootfs_path": info.RootFS, "snapshot_key": info.SnapshotKey}}
}

// handleRegisterImage 注册一个已构建好的镜像（builder 通过 Daemon 调用）
// args: name, tag, image_id, size, created_at, rootfs, layers(逗号分隔)
func (c *Containerd) handleRegisterImage(req Request) Response {
	if c.imageService == nil {
		return Response{Success: false, Message: "镜像服务未初始化"}
	}

	layersCSV := req.Args["layers"]
	var layers []string
	if layersCSV != "" {
		for _, l := range strings.Split(layersCSV, ",") {
			if l != "" {
				layers = append(layers, l)
			}
		}
	}

	info := &images.ImageInfo{
		Name:        req.Args["name"],
		Tag:         req.Args["tag"],
		ImageID:     req.Args["image_id"],
		Size:        req.Args["size"],
		CreatedAt:   req.Args["created_at"],
		RootFS:      req.Args["rootfs"],
		SnapshotKey: req.Args["snapshot_key"],
		Layers:      layers,
	}

	ctx := context.Background()
	if err := c.imageService.Register(ctx, info); err != nil {
		return Response{Success: false, Message: fmt.Sprintf("注册镜像失败: %v", err)}
	}
	return Response{Success: true}
}
