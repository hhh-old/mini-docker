package daemon

/*
=======================================================================
  通信协议 —— CLI 与 Daemon 之间的请求/响应格式

  对齐 Docker 的 C/S 架构：

  普通请求（ps/stop/images 等）：
  ┌──────────┐                    ┌──────────┐
  │ CLI      │  ──── Request ───→ │ Daemon   │
  │          │  ←── Response ──── │          │
  └──────────┘                    └──────────┘

  流式请求（run -it / attach）：
  ┌──────────┐                    ┌──────────┐
  │ CLI      │  ──── Request ───→ │ Daemon   │
  │          │  ←── Response ──── │          │
  │          │  ←──── I/O ──────→│          │  双向流式转发
  └──────────┘                    └──────────┘

  所有通信使用 JSON 编码，通过 Unix Socket 传输。
  流式模式下，初始 JSON 握手后切换为原始字节流。

=======================================================================
*/

type Request struct {
	Type string            `json:"type"`
	Args map[string]string `json:"args"`
}

type Response struct {
	Success     bool          `json:"success"`
	Message     string        `json:"message"`
	Data        interface{}   `json:"data"`
	Stream      bool          `json:"stream,omitempty"`
	StreamReady chan struct{} `json:"-"`
}

// FrameType 进度帧的类型（区分进度推送帧和结果终结帧）
type FrameType string

// ProgressFrameStatus 进度帧的状态
//   - Downloading/Extracting/Building：进度帧的阶段性状态
//   - Warning：进度帧中提示非致命错误
//   - Complete/Error：结果帧的成功/失败标记
type ProgressFrameStatus string

const (
	// ProgressFrame 进度帧类型（容器侧持续推送）
	ProgressFrame FrameType = "progress"
	// ResultFrame 结果帧类型（容器侧终结本次流式响应）
	ResultFrame FrameType = "result"
)

const (
	StatusDownloading ProgressFrameStatus = "downloading"
	StatusExtracting  ProgressFrameStatus = "extracting"
	StatusBuilding    ProgressFrameStatus = "building"
	StatusWarning     ProgressFrameStatus = "warning"
	StatusComplete    ProgressFrameStatus = "complete"
	StatusError       ProgressFrameStatus = "error"
)

// ProgressFrameData 进度消息帧（用于 pull 等流式进度推送）
type ProgressFrameData struct {
	Type    FrameType           `json:"type"`              // ProgressFrame / ResultFrame
	Status  ProgressFrameStatus `json:"status,omitempty"`  // downloading/extracting/building/warning/complete/error
	Success bool                `json:"success,omitempty"` // 仅 result 帧使用
	Message string              `json:"message"`           // 进度消息
	Data    interface{}         `json:"data,omitempty"`    // 最终结果（仅 result 帧）
}
