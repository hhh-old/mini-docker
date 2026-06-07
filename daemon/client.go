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
	"net"
	"time"

	"mini-docker/constants"
)

type Client struct {
	socketPath string
	timeout    time.Duration
}

func NewClient() *Client {
	return &Client{
		socketPath: SocketPath,
		timeout:    constants.DefaultConnectTimeout,
	}
}

func (c *Client) WithTimeout(timeout time.Duration) *Client {
	return &Client{
		socketPath: c.socketPath,
		timeout:    timeout,
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

	conn.SetWriteDeadline(time.Now().Add(c.timeout))

	if _, err := conn.Write(data); err != nil {
		return nil, fmt.Errorf("发送请求失败: %w", err)
	}

	// 对于耗时操作（如拉取镜像），使用更长的超时时间
	readTimeout := c.timeout
	if req.Type == "pull" {
		readTimeout = constants.RegistryPullTimeout
	}
	conn.SetReadDeadline(time.Now().Add(readTimeout))

	var resp Response
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
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
	br := bufio.NewReaderSize(conn, constants.DefaultBufferSize) //使用缓冲区来一次读取一批最多这么长一串的数据，减少每次conn.Read都要走系统调用的次数，从而提升性能
	var resp Response
	//unix socket通信，如果对端没有发来数据或者足够量的数据，阻塞发生在哪一步？
	//阻塞发生在 json.NewDecoder(br).Decode(&resp) ， 间接 发生在 br.Read() → conn.Read() 。
	//conn.Read() 是阻塞 syscall，行为规则：
	//对方socket状态 			conn.Read 行为
	//有 ≥1 字节 			立即返回，最多读 len(b) 字节（ 不会等到缓冲区满 ）
	//0 字节（对方还没写） 		阻塞，直到对端发来至少 1 字节
	//对端关闭（EOF） 		返回 io.EOF
	//网络断开/出错 			返回 error
	//也就是说，bufio 不会"硬等" 64KB， 只要对端有 1 字节就立刻返回 。64KB 只是 缓存粒度 ，不是"等齐才返回"的阈值。

	//Response 比 64KB 大，Decode 会怎样？解析Response会不会因为缓冲区数据不足而失败？
	//完全没问题，能正确解析 。这是最常见的误解之一。
	//64KB 不是消息上限，是缓存粒度 。bufio 内部 Read 流程是：
	//调用方: Read(b)  ──>  bufio 内部:
	//                         1. 内部 buf 有数据？直接拷贝
	//                         2. buf 为空？调 conn.Read 拿一批（最多 64KB）
	//                         3. 把这批放进内部 buf，再拷给调用方
	//具体到 200KB 的 JSON Response，过程是：
	//1. JSON decoder 第一次 Read → bufio 调用 conn.Read → 拿到 64KB
	//2. JSON 解析发现消息还没完整 → 第二次 Read → bufio 再调 conn.Read → 拿到下一个 64KB
	//3. 重复 ~4 次，直到 200KB 全部读完，JSON 解析完成

	//json.NewDecoder(conn).Decode(&req)怎么从一串字节流中解析出来需要的json字节流然后反序列化成json的？为什么不会有粘包问题?
	//json.NewDecoder(conn).Decode(&req) 能够在没有显式长度或分隔符的情况下，从字节流中提取出一个完整的 JSON 对象，核心原因是：JSON 语法是“自描述”的：每个合法的 JSON 值（对象、数组、字符串、数字、布尔、null）都有明确的起始和结束标记，解析器可以基于这些语法规则流式地判断一个值何时结束比如认为{...}就是一个完整的json对象。
	//所以这里网络协议设计的时候没有使用额外的字段来分割一串字节流来取出完整的“通信包”数据。
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
		if bc.reader.Buffered() > 0 { // 缓冲区里还有"陈年"数据
			return bc.reader.Read(b) // 优先吐出来
		}
		bc.reader = nil // 排空了，再读就直走 conn
	}
	return bc.conn.Read(b) // 走真实 socket
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
