//go:build linux

package containerd

/*
=======================================================================
  通信客户端 —— Daemon 通过此客户端与 containerd 独立进程通信

  对齐 Docker 的 containerd 客户端架构：
  ┌──────────┐    Unix Socket     ┌──────────────┐
  │ Daemon   │ ───────────────→  │  containerd  │
  │ (client) │  SendRequest()    │  (server)    │
  │          │  SendStreamReq()  │              │
  └──────────┘                    └──────────────┘

=======================================================================
*/

import (
	"encoding/json"
	"fmt"
	"net"
	"time"

	"mini-docker/constants"
)

// ConnectContainerd 连接到 containerd 进程的 Unix Socket
func ConnectContainerd() (net.Conn, error) {
	conn, err := net.DialTimeout("unix", ContainerdSocketPath, constants.ShimConnectTimeout)
	if err != nil {
		return nil, fmt.Errorf("连接 containerd 失败（请确认 containerd 是否已启动）: %w", err)
	}
	return conn, nil
}

// SendRequest 发送普通请求并等待响应
func SendRequest(req Request) (*Response, error) {
	conn, err := ConnectContainerd()
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	conn.SetWriteDeadline(time.Now().Add(constants.DefaultConnectTimeout))
	data, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("序列化请求失败: %w", err)
	}
	if _, err := conn.Write(data); err != nil {
		return nil, fmt.Errorf("发送请求失败: %w", err)
	}

	conn.SetReadDeadline(time.Now().Add(constants.DefaultConnectTimeout))
	var resp Response
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		return nil, fmt.Errorf("解析响应失败: %w", err)
	}
	return &resp, nil
}

// SendStreamRequest 发送流式请求，返回连接供调用方进行 I/O 转发
// 对齐 Docker: dockerd 与 containerd 之间的流式连接（attach/exec）
func SendStreamRequest(req Request) (net.Conn, *Response, error) {
	conn, err := ConnectContainerd()
	if err != nil {
		return nil, nil, err
	}

	data, err := json.Marshal(req)
	if err != nil {
		conn.Close()
		return nil, nil, fmt.Errorf("序列化请求失败: %w", err)
	}
	if _, err := conn.Write(data); err != nil {
		conn.Close()
		return nil, nil, fmt.Errorf("发送请求失败: %w", err)
	}

	var resp Response
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		conn.Close()
		return nil, nil, fmt.Errorf("解析响应失败: %w", err)
	}

	if !resp.Success {
		conn.Close()
		return nil, &resp, fmt.Errorf("%s", resp.Message)
	}

	return conn, &resp, nil
}

// WriteResponse 向连接写入响应（供服务端使用）
func WriteResponse(conn net.Conn, resp Response) error {
	data, err := json.Marshal(resp)
	if err != nil {
		return fmt.Errorf("序列化响应失败: %w", err)
	}
	for len(data) > 0 {
		n, err := conn.Write(data)
		if err != nil {
			return fmt.Errorf("写入响应失败: %w", err)
		}
		data = data[n:]
	}
	return nil
}
