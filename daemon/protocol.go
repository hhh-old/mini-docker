package daemon

/*
=======================================================================
  通信协议 —— CLI 与 Daemon 之间的请求/响应格式
=======================================================================

  通信流程：
  ┌──────────┐                    ┌──────────┐
  │ CLI      │  ──── Request ───→ │ Daemon   │
  │          │  ←── Response ──── │          │
  └──────────┘                    └──────────┘

  所有通信使用 JSON 编码，通过 Unix Socket 传输。

  Request 结构：
  {
    "type": "run",           ← 请求类型
    "args": {                ← 请求参数
      "image": "myos",
      "cmd": "/bin/sh",
      "tty": "true",
      "memory": "100m",
      ...
    }
  }

  Response 结构：
  {
    "success": true,         ← 是否成功
    "message": "...",        ← 人类可读的消息
    "data": { ... }          ← 附加数据（如容器列表）
  }

=======================================================================
*/

// Request CLI 发送给 Daemon 的请求
type Request struct {
	Type string            `json:"type"` // 请求类型: run, stop, ps, exec, ...
	Args map[string]string `json:"args"` // 请求参数
}

// Response Daemon 返回给 CLI 的响应
type Response struct {
	Success bool        `json:"success"` // 是否成功
	Message string      `json:"message"` // 人类可读的消息
	Data    interface{} `json:"data"`    // 附加数据
}
