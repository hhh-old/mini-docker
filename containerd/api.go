package containerd

import "mini-docker/containerd/images"

/*
=======================================================================
  API 协议 —— containerd 独立进程与 Daemon 之间的通信协议
=======================================================================

  对齐 Docker 的 containerd 通信架构：
  ┌──────────┐    Unix Socket    ┌──────────────┐
  │ dockerd  │ ──────────────→  │  containerd  │
  │ (client) │  gRPC-like API   │  (server)    │
  └──────────┘                   └──────────────┘

  mini-docker 的对齐架构：
  ┌──────────�    Unix Socket    ┌──────────────┐
  │ Daemon   │ ──────────────→  │  containerd  │
  │ (client) │  JSON + 原始流    │  (server)    │
  └──────────┘                   └──────────────┘

  通信模式：
  1. 普通请求/响应：JSON 编码的 Request → Response
  2. 流式连接：先 JSON 握手，再切换为原始字节流（用于 Attach/Exec）

  本文件只包含协议结构体（Request/Response）和进度帧类型；
  路由键常量（ReqCreateTask 等）已分离到 routes.go；
  进程路径常量（ContainerdSocketPath）已收敛到 constants 包，本文件不再 re-export；
  跨平台类型别名（ExitInfo）已分离到 types.go。
  进度状态枚举（ProgressFrameStatus）属于领域层，已下沉到 images 包。

=======================================================================
*/

// ---------------------------------------------------------------------------
// 请求/响应结构体
// ---------------------------------------------------------------------------

// Request containerd API 请求（Daemon → containerd）
type Request struct {
	Type string            `json:"type"`
	Args map[string]string `json:"args,omitempty"`
}

// Response containerd API 响应（containerd → Daemon）
type Response struct {
	Success bool        `json:"success"`
	Message string      `json:"message,omitempty"`
	Data    interface{} `json:"data,omitempty"`
	Stream  bool        `json:"stream,omitempty"`
}

// ---------------------------------------------------------------------------
// 进度帧类型（用于流式进度推送）
//
// FrameType（progress/result）属于协议层概念，留在此包。
// ProgressFrameStatus（downloading/building/complete/error）属于领域层，
// 定义在 images 包；此处仅作引用，序列化走 string 底层，跨包无碍。
// ---------------------------------------------------------------------------

// FrameType 进度帧的类型（区分进度推送帧和结果终结帧）
type FrameType string

const (
	// ProgressFrame 进度帧类型（容器侧持续推送）
	ProgressFrame FrameType = "progress"
	// ResultFrame 结果帧类型（容器侧终结本次流式响应）
	ResultFrame FrameType = "result"
)

// ProgressFrameData 进度消息帧
type ProgressFrameData struct {
	Type    FrameType                  `json:"type"`             // ProgressFrame / ResultFrame
	Status  images.ProgressFrameStatus `json:"status,omitempty"` // downloading/extracting/building/warning/complete/error
	Layer   int                        `json:"layer,omitempty"`  // 当前层号
	Total   int                        `json:"total,omitempty"`  // 总层数
	Message string                     `json:"message"`          // 进度消息
	Data    interface{}                `json:"data,omitempty"`   // 最终结果（仅 result 帧）
}
