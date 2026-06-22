//go:build linux

package containerd

/*
=======================================================================
  Content Store RPC 处理器 —— 对齐 containerd 的 Content Store gRPC 服务

  改造前：Daemon 直接创建本地 content.NewFilesystemStore 实例，绕过 containerd
  改造后：Daemon 通过 RPC 调用 containerd 的 Content Store，对齐真实架构

  Content Store 的 Writer 模式需要特殊处理：
  - Writer 创建：在 containerd 侧创建 Writer，返回 ref 标识
  - 数据写入：通过 RPC 分块传输 blob 数据
  - Commit 提交：通过 RPC 提交并校验 digest

  由于 blob 写入涉及大文件流式传输，采用"分块写入"模式：
  1. 客户端调用 ContentWriter(ref, expected, size, mediaType) 创建 Writer
  2. 客户端循环调用 ContentWrite(ref, data) 分块写入
  3. 客户端调用 ContentCommit(ref, expectedDigest) 提交

=======================================================================
*/

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"

	"mini-docker/containerd/content"
	"mini-docker/containerd/plugin"
)

// getContentStore 从插件管理器获取底层 Content Store
func (c *Containerd) getContentStore() content.Store {
	inst, _ := c.plugins.Get(plugin.TypeContent, "filesys")
	if inst == nil {
		return nil
	}
	return inst.(content.Store)
}

// getContentService 从插件管理器获取 Content Service
func (c *Containerd) getContentService() *content.Service {
	inst, _ := c.plugins.Get(plugin.TypeService, "content")
	if inst == nil {
		return nil
	}
	return inst.(*content.Service)
}

// handleContentInfo 查询 blob 元信息
func (c *Containerd) handleContentInfo(req Request) Response {
	svc := c.getContentService()
	if svc == nil {
		return Response{Success: false, Message: "Content Service 未初始化"}
	}

	digest := req.Args["digest"]
	if digest == "" {
		return Response{Success: false, Message: "需要指定 digest"}
	}

	info, err := svc.Info(context.Background(), digest)
	if err != nil {
		return Response{Success: false, Message: fmt.Sprintf("查询 blob 信息失败: %v", err)}
	}
	return Response{Success: true, Data: info}
}

// handleContentPath 获取 blob 本地存储路径
func (c *Containerd) handleContentPath(req Request) Response {
	svc := c.getContentService()
	if svc == nil {
		return Response{Success: false, Message: "Content Service 未初始化"}
	}

	digest := req.Args["digest"]
	if digest == "" {
		return Response{Success: false, Message: "需要指定 digest"}
	}

	path, err := svc.Path(context.Background(), digest)
	if err != nil {
		return Response{Success: false, Message: fmt.Sprintf("获取 blob 路径失败: %v", err)}
	}
	return Response{Success: true, Data: map[string]interface{}{"path": path}}
}

// handleContentExists 检查 blob 是否存在
func (c *Containerd) handleContentExists(req Request) Response {
	svc := c.getContentService()
	if svc == nil {
		return Response{Success: false, Message: "Content Service 未初始化"}
	}

	digest := req.Args["digest"]
	if digest == "" {
		return Response{Success: false, Message: "需要指定 digest"}
	}

	exists := svc.Exists(context.Background(), digest)
	return Response{Success: true, Data: map[string]interface{}{"exists": exists}}
}

// handleContentDelete 删除 blob
func (c *Containerd) handleContentDelete(req Request) Response {
	svc := c.getContentService()
	if svc == nil {
		return Response{Success: false, Message: "Content Service 未初始化"}
	}

	digest := req.Args["digest"]
	if digest == "" {
		return Response{Success: false, Message: "需要指定 digest"}
	}

	if err := svc.Delete(context.Background(), digest); err != nil {
		return Response{Success: false, Message: fmt.Sprintf("删除 blob 失败: %v", err)}
	}
	return Response{Success: true}
}

// handleContentWrite 写入 blob 数据（分块写入模式）
// 客户端先通过 ContentWriter 创建 Writer，然后循环调用此接口写入数据
func (c *Containerd) handleContentWrite(req Request) Response {
	svc := c.getContentService()
	if svc == nil {
		return Response{Success: false, Message: "Content Service 未初始化"}
	}

	ref := req.Args["ref"]
	dataStr := req.Args["data"]
	action := req.Args["action"] // "create" | "write" | "commit" | "close"

	switch action {
	case "create":
		// 创建新的 Writer
		expected := req.Args["expected"]
		var size int64
		fmt.Sscanf(req.Args["size"], "%d", &size)
		mediaType := req.Args["media_type"]

		w, err := svc.Writer(context.Background(), expected, size, mediaType)
		if err != nil {
			return Response{Success: false, Message: fmt.Sprintf("创建 Writer 失败: %v", err)}
		}

		c.activeWritersMu.Lock()
		c.activeWriters[ref] = w
		c.activeWritersMu.Unlock()

		return Response{Success: true, Data: map[string]interface{}{"ref": ref}}

	case "write":
		// 向已有 Writer 写入数据（客户端使用 base64 编码传输二进制数据）
		c.activeWritersMu.Lock()
		w, ok := c.activeWriters[ref]
		c.activeWritersMu.Unlock()
		if !ok {
			return Response{Success: false, Message: fmt.Sprintf("Writer %s 不存在", ref)}
		}

		data, err := base64.StdEncoding.DecodeString(dataStr)
		if err != nil {
			return Response{Success: false, Message: fmt.Sprintf("解码数据失败: %v", err)}
		}
		if _, err := w.Write(data); err != nil {
			return Response{Success: false, Message: fmt.Sprintf("写入数据失败: %v", err)}
		}

		written, _ := w.Status()
		return Response{Success: true, Data: map[string]interface{}{"written": written}}

	case "commit":
		// 提交 Writer 并校验 digest
		c.activeWritersMu.Lock()
		w, ok := c.activeWriters[ref]
		if ok {
			delete(c.activeWriters, ref)
		}
		c.activeWritersMu.Unlock()
		if !ok {
			return Response{Success: false, Message: fmt.Sprintf("Writer %s 不存在", ref)}
		}

		expectedDigest := req.Args["expected_digest"]
		if err := w.Commit(context.Background(), expectedDigest); err != nil {
			w.Close()
			return Response{Success: false, Message: fmt.Sprintf("提交 blob 失败: %v", err)}
		}

		digest := w.Digest()
		return Response{Success: true, Data: map[string]interface{}{"digest": digest}}

	case "close":
		// 关闭 Writer（不提交，丢弃数据）
		c.activeWritersMu.Lock()
		w, ok := c.activeWriters[ref]
		if ok {
			delete(c.activeWriters, ref)
		}
		c.activeWritersMu.Unlock()
		if !ok {
			return Response{Success: false, Message: fmt.Sprintf("Writer %s 不存在", ref)}
		}
		w.Close()
		return Response{Success: true}

	default:
		return Response{Success: false, Message: fmt.Sprintf("未知 action: %s", action)}
	}
}

// handleContentCommit 提交 blob 并校验 digest（快捷方式：创建+写入+提交一步完成）
func (c *Containerd) handleContentCommit(req Request) Response {
	// 此接口保留用于未来优化，当前通过 write+action=commit 实现
	return Response{Success: false, Message: "请使用 content_write 接口"}
}

// handleContentWalk 遍历所有 blob 元信息
func (c *Containerd) handleContentWalk(req Request) Response {
	svc := c.getContentService()
	if svc == nil {
		return Response{Success: false, Message: "Content Service 未初始化"}
	}

	var infos []content.Info
	err := svc.Walk(context.Background(), func(info content.Info) error {
		infos = append(infos, info)
		return nil
	})
	if err != nil {
		return Response{Success: false, Message: fmt.Sprintf("遍历 blob 失败: %v", err)}
	}
	return Response{Success: true, Data: map[string]interface{}{"infos": infos}}
}

// handleContentUpdate 更新 blob 标签
func (c *Containerd) handleContentUpdate(req Request) Response {
	svc := c.getContentService()
	if svc == nil {
		return Response{Success: false, Message: "Content Service 未初始化"}
	}

	digest := req.Args["digest"]
	if digest == "" {
		return Response{Success: false, Message: "需要指定 digest"}
	}

	var labels map[string]string
	if labelsJSON := req.Args["labels"]; labelsJSON != "" {
		json.Unmarshal([]byte(labelsJSON), &labels)
	}

	if err := svc.Update(context.Background(), digest, labels); err != nil {
		return Response{Success: false, Message: fmt.Sprintf("更新 blob 标签失败: %v", err)}
	}
	return Response{Success: true}
}
