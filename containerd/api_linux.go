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
	"mini-docker/containerd/images"
)

// ConnectContainerd 连接到 containerd 进程的 Unix Socket
func ConnectContainerd() (net.Conn, error) {
	conn, err := net.DialTimeout("unix", constants.ContainerdSocketPath, constants.ShimConnectTimeout)
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

	// 对于耗时操作（如拉取镜像），使用更长的超时时间
	readTimeout := constants.DefaultConnectTimeout
	if req.Type == ReqPullImage {
		readTimeout = constants.RegistryPullTimeout
	}
	conn.SetReadDeadline(time.Now().Add(readTimeout))
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

// SendStreamProgressRequest 发送请求并通过回调接收流式进度帧
// progressFn 会收到所有帧（包括 result 帧），调用方需在 progressFn 中判断帧类型
func SendStreamProgressRequest(req Request, progressFn func(ProgressFrameData)) (*Response, error) {
	conn, err := ConnectContainerd()
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	data, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("序列化请求失败: %w", err)
	}
	conn.SetWriteDeadline(time.Now().Add(constants.DefaultConnectTimeout))
	if _, err := conn.Write(data); err != nil {
		return nil, fmt.Errorf("发送请求失败: %w", err)
	}

	// 持续读取进度帧直到收到 result 帧
	readTimeout := constants.RegistryPullTimeout

	decoder := json.NewDecoder(conn)
	for { // 循环读 JSON 帧
		conn.SetReadDeadline(time.Now().Add(readTimeout))
		var frame ProgressFrameData
		if err := decoder.Decode(&frame); err != nil {
			return nil, fmt.Errorf("解析进度帧失败: %w", err)
		}
		if progressFn != nil {
			progressFn(frame) // 回调,deamon进程将从containerd进程得到的镜像pull进度数据写回deamon的 client
		}
		if frame.Type == ResultFrame { //读到result帧
			success := frame.Status != images.StatusError
			return &Response{Success: success, Message: frame.Message, Data: frame.Data}, nil
		}
	}
}

// WriteResponse 向连接写入响应（供服务端使用）
func WriteResponse(conn net.Conn, resp Response) error {
	data, err := json.Marshal(resp) //将 Go 语言中的 Response 结构体转换为 JSON 格式的字节数组（[]byte），因为网络传输只能发送字节流。
	if err != nil {
		return fmt.Errorf("序列化响应失败: %w", err)
	}
	//循环逻辑：
	//n, err := conn.Write(data)：尝试写入当前所有数据，并返回实际成功写入的字节数 n。
	//如果报错（比如连接断开），直接返回错误。
	//data = data[n:]：利用 Go 的切片特性，把已经成功写入的前 n 个字节切除掉，保留剩下还没写完的数据，在下一次循环中继续发送。
	//当 len(data) 等于 0 时，说明所有数据都一字不差地发送完毕了，退出循环
	for len(data) > 0 {
		n, err := conn.Write(data)
		if err != nil {
			return fmt.Errorf("写入响应失败: %w", err)
		}
		data = data[n:]
	}
	return nil
}
