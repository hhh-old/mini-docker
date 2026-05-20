package daemon

/*
=======================================================================
  Client —— CLI 端与 Daemon 通信的客户端
=======================================================================

  CLI 不再直接操作 container/cgroup/network 包，
  而是通过 Unix Socket 向 Daemon 发送请求。

  通信流程：
  ┌──────────┐   Request    ┌──────────┐
  │ CLI      │ ───────────→ │ Daemon   │
  │ (client) │ ←─────────── │ (server) │
  └──────────┘   Response   └──────────┘

=======================================================================
*/

import (
	"encoding/json"
	"fmt"
	"net"
	"time"
)

// Client Daemon 客户端
type Client struct {
	socketPath string
	timeout    time.Duration
}

// NewClient 创建客户端
func NewClient() *Client {
	return &Client{
		socketPath: SocketPath,
		timeout:    30 * time.Second,
	}
}

// Dial 连接到 Daemon
func (c *Client) Dial() (net.Conn, error) {
	conn, err := net.DialTimeout("unix", c.socketPath, c.timeout)
	if err != nil {
		return nil, fmt.Errorf("连接 Daemon 失败（请确认 Daemon 是否已启动）: %w", err)
	}
	return conn, nil
}

// Send 发送请求并接收响应
func (c *Client) Send(req Request) (*Response, error) {
	conn, err := c.Dial()
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	// 序列化请求
	data, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("序列化请求失败: %w", err)
	}

	// 发送请求
	if _, err := conn.Write(data); err != nil {
		return nil, fmt.Errorf("发送请求失败: %w", err)
	}

	// 读取响应
	buf := make([]byte, 1024*1024) // 1MB 缓冲区
	conn.SetReadDeadline(time.Now().Add(c.timeout))
	n, err := conn.Read(buf)
	if err != nil {
		return nil, fmt.Errorf("读取响应失败: %w", err)
	}

	var resp Response
	if err := json.Unmarshal(buf[:n], &resp); err != nil {
		return nil, fmt.Errorf("解析响应失败: %w", err)
	}

	return &resp, nil
}

// Ping 检查 Daemon 是否可用
func (c *Client) Ping() error {
	resp, err := c.Send(Request{Type: "ping"})
	if err != nil {
		return err
	}
	if !resp.Success {
		return fmt.Errorf("Daemon 不可用")
	}
	return nil
}
