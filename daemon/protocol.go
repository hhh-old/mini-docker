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
