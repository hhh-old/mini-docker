package daemon

/*
=======================================================================
  Client —— CLI 端与 Daemon 通信的客户端

  对齐 Docker CLI 的通信模式：

  普通请求：Send() — 一次性 request/response
  流式请求：SendStream() — 保持连接，双向 I/O 转发

  流式通信流程（对齐 docker run -it）：
  ┌──────────┐   Request     ┌──────────┐
  │ CLI      │ ────────────→ │ Daemon   │
  │          │ ←──────────── │          │
  │          │   Response    │          │
  │          │   (stream=true)│         │
  │          │ ←────────────→│          │
  │          │  原始字节流    │          │
  │  stdin ──┤──────────────→│──→ shim  │
  │  stdout ←┤←──────────────│←── shim  │
  └──────────┘               └──────────┘

=======================================================================
*/

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"time"

	"mini-docker/constants"
)

type Client struct {
	socketPath string
	timeout    time.Duration
}

// 连接deamon进程
func NewClient() *Client {
	return &Client{
		socketPath: SocketPath,
		timeout:    constants.DefaultConnectTimeout,
	}
}

func (c *Client) Dial() (net.Conn, error) {
	conn, err := net.DialTimeout("unix", c.socketPath, c.timeout)
	if err != nil {
		return nil, fmt.Errorf("连接 Daemon 失败（请确认 Daemon 是否已启动）: %w", err)
	}
	return conn, nil
}

// 一次性短连接。一问一答，数据传输完毕后连接立即释放
func (c *Client) Send(req Request) (*Response, error) {
	conn, err := c.Dial()
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	data, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("序列化请求失败: %w", err)
	}

	conn.SetDeadline(time.Now().Add(c.timeout))

	if _, err := conn.Write(data); err != nil {
		return nil, fmt.Errorf("发送请求失败: %w", err)
	}

	respData, err := io.ReadAll(conn)
	if err != nil {
		return nil, fmt.Errorf("读取响应失败: %w", err)
	}

	var resp Response
	if err := json.Unmarshal(respData, &resp); err != nil {
		return nil, fmt.Errorf("解析响应失败: %w", err)
	}

	return &resp, nil
}

// SendStream 发送流式请求，返回连接供调用方进行 I/O 转发
// 对齐 Docker CLI 的 attach 行为：保持连接打开，双向转发终端 I/O
// 用于执行 run -it 或 exec 等需要实时双向传输终端数据的交互式命令
func (c *Client) SendStream(req Request) (net.Conn, *Response, error) {
	conn, err := c.Dial()
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
	//用 bufio.NewReaderSize 为连接包了一层带缓冲的读取器
	br := bufio.NewReaderSize(conn, constants.DefaultBufferSize)
	var resp Response
	if err := json.NewDecoder(br).Decode(&resp); err != nil {
		conn.Close()
		return nil, nil, fmt.Errorf("解析响应失败: %w", err)
	}

	if !resp.Success {
		conn.Close()
		return nil, &resp, fmt.Errorf("%s", resp.Message)
	}

	if !resp.Stream { //不支持流式传输
		conn.Close()
		return nil, &resp, nil
	}
	//将原始连接和缓冲区包装成一个自定义的 bufferedConn 结构体返回给调用者
	wrappedConn := &bufferedConn{conn: conn, reader: br}
	return wrappedConn, &resp, nil
}

// bufferedConn 包装 net.Conn，先排空 bufio.Reader 中的预读数据，再委托给原始连接
type bufferedConn struct {
	conn   net.Conn
	reader *bufio.Reader
}

func (bc *bufferedConn) Read(b []byte) (int, error) {
	if bc.reader != nil {
		if bc.reader.Buffered() > 0 {
			return bc.reader.Read(b)
		}
		bc.reader = nil
	}
	return bc.conn.Read(b)
}

func (bc *bufferedConn) Write(b []byte) (int, error) {
	return bc.conn.Write(b)
}

func (bc *bufferedConn) Close() error {
	return bc.conn.Close()
}

func (bc *bufferedConn) LocalAddr() net.Addr {
	return bc.conn.LocalAddr()
}

func (bc *bufferedConn) RemoteAddr() net.Addr {
	return bc.conn.RemoteAddr()
}

func (bc *bufferedConn) SetDeadline(t time.Time) error {
	return bc.conn.SetDeadline(t)
}

func (bc *bufferedConn) SetReadDeadline(t time.Time) error {
	return bc.conn.SetReadDeadline(t)
}

func (bc *bufferedConn) SetWriteDeadline(t time.Time) error {
	return bc.conn.SetWriteDeadline(t)
}
