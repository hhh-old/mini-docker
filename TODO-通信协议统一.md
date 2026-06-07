# TODO：统一三层通信协议

> 创建时间：2026-06-06
> 状态：待定（未决定方案）
> 优先级：低（当前可工作，重复代码可接受）

---

## 背景

mini-docker 中存在三套几乎相同的 JSON-over-UnixSocket 协议：

| 链路              | 类型定义                                | 客户端函数                              |
| ----------------- | --------------------------------------- | --------------------------------------- |
| CLI ↔ Daemon      | `daemon.Request/Response`               | `daemon.Client.Send / SendStream`       |
| Daemon ↔ containerd | `containerd.Request/Response`         | `SendRequest / SendStreamRequest / SendStreamProgressRequest` |
| containerd ↔ shim | `types.ShimRequest/ShimResponse`        | `shimCall / shimCallWithData`           |

三层的"形状"一致（都有 `Type` 路由、`Success/Message/Data` 响应、相同的三种使用姿势），但**类型各写一份、编解码逻辑各写一份、进度帧各写一份**。

详见代码：
- [daemon/protocol.go](daemon/protocol.go) — `Request/Response/ProgressFrameData`
- [daemon/client.go](daemon/client.go) — `Send/SendStream`
- [containerd/api.go](containerd/api.go) — `Request/Response/ProgressFrameData`
- [containerd/api_linux.go](containerd/api_linux.go) — `SendRequest/SendStreamRequest/SendStreamProgressRequest`
- [containerd/shim_manager_linux.go](containerd/shim_manager_linux.go) — `shimCall/shimCallWithData`
- [types/types.go](types/types.go) — `ShimRequest/ShimResponse`

参考对比：真实 Docker 底下两层（dockerd↔containerd、containerd↔shim）使用 **gRPC**，靠方法签名区分一次性 / 服务端流 / 双向流；CLI↔dockerd 仍是 REST，streaming 走 chunked / WebSocket。

---

## 问题清单

- [ ] **重复类型定义**：`daemon.Request` vs `containerd.Request` 字段完全相同；`daemon.ProgressFrameData` vs `containerd.ProgressFrameData` 几乎相同
- [ ] **重复进度帧实现**：daemon 侧少了 `Layer/Total` 字段
- [ ] **重复 send/connect/JSON 编解码逻辑**：三个包里各一份
- [ ] **daemon 独有的 `StreamReady` 字段**：本质是本地同步原语，混在协议结构体里不干净
- [ ] **shim 层用强类型字段** vs 另两层用 `map[string]string`：风格不一致（这条可能是有意为之，保留）

---

## 待选方案

### 方案 A：轻量抽象（推荐起点）

把通用壳子提到共享包，三层都基于它：

1. 新建 `ipc/` 包，提供
   - `Request{Type, Args map[string]string}`
   - `Response{Success, Message, Data, Stream}`
   - `ProgressFrame{Type, Status, Message, Data, ...}`
   - `Client{Send / SendStream / SendStreamProgress}`，socketPath 作为构造参数
2. `daemon.Request/Response` 改成 alias + 局部扩展 `StreamReady`
3. `containerd.Request/Response` 直接用 `ipc` 包
4. `types.ShimRequest` 用 `ipc.Request` 嵌入 + 私有强类型字段

**收益**：代码量下降 ~30%，三层协议演进更同步
**代价**：三层失去一点独立性（但本来也没什么独立性可言）

### 方案 A+：自己实现统一帧协议（Frame Envelope）

比方案 A 更进一步：把"一次性 RPC / 流式 I/O / 流式进度"在传输层就统一成一种"帧"。

1. 定义统一的 `Frame` 信封，**所有通信都变成读写 Frame**：

   ```go
   type Frame struct {
       ID      string          `json:"id"`              // 请求/流标识, 支持多路复用
       Type    string          `json:"type"`            // "request" | "response" | "progress" | "stream_data" | "stream_end"
       Method  string          `json:"method,omitempty"`  // 路由键 (request/response 用)
       Stream  bool            `json:"stream,omitempty"`  // 标记当前帧是否属于某个流
       Status  bool            `json:"status,omitempty"`  // response 帧的成功/失败
       Message string          `json:"message,omitempty"`
       Args    json.RawMessage `json:"args,omitempty"`  // request 参数
       Data    json.RawMessage `json:"data,omitempty"`  // response/stream_data 载荷
   }
   ```

2. 三种通信模式在 Frame 层面对应：
   - **一次性 RPC**：发 1 帧 `request` + 收 1 帧 `response`
   - **流式 I/O**：发 1 帧 `request` + 收 1 帧 `response(stream=true)` + 双向 `stream_data` 帧 + 收 1 帧 `stream_end`
   - **流式进度**：发 1 帧 `request` + 收 N 帧 `progress` + 收 1 帧 `response`（type=result）

3. 一份 `ipc.Client` 同时处理连接管理、帧读写、流多路复用（靠 `ID` 区分不同流）

**收益**：
- 一份编解码代码、一种连接管理逻辑
- 天然支持**多路复用**（一个连接上同时跑多个流）
- 三种模式在代码层不再有"if stream: ..."分支，纯粹靠 Frame.Type 分发
- 进度帧、流式 I/O、一次性 RPC 走同一条路径

**代价**：
- 自己造轮子：要做帧边界处理、连接复用、流状态机、超时/心跳/断连重试
- `ID` 字段需要新加一套会话/流标识机制
- 比方案 A 工作量大，比 gRPC 工作量小（不用引入 protobuf）

### 方案 B：迁移到 gRPC（对齐真实 Docker）

1. CLI↔Daemon 仍用 HTTP/REST（和 Docker 一样），可加 chunked/WebSocket 支持 streaming
2. 底下两层用 gRPC，`.proto` 文件声明方法：
   - `rpc CreateTask(Req) returns (Resp);`           // 一次性
   - `rpc PullImage(Req) returns (stream Progress);` // 服务端流
   - `rpc Attach(stream IOPacket) returns (stream IOPacket);` // 双向流
3. shim 层可用 TTRPC 减小二进制

**收益**：和 Docker 架构完全一致，生态成熟（拦截器、超时、metadata、流控开箱即用）
**代价**：引入 protobuf 工具链，对 mini-docker 这种规模可能偏重

### 方案 C：维持现状

- 接受"三套协议壳子"的设计，理由是对齐 Docker 进程边界清晰
- 唯一要做的小事：把 `daemon.ProgressFrameData` 和 `containerd.ProgressFrameData` 字段对齐（daemon 加上缺失的 `Layer/Total`）

---

## 决定事项

- [ ] **选哪个方案？**（A / B / C）
- [ ] 如果选 A，共享包放哪？建议 `mini-docker/ipc/`
- [ ] 如果选 A，是否一次性重构还是分阶段（先合并 daemon↔containerd，再处理 shim）？
- [ ] 如果选 B，是否所有改动一次性做？还是先 gRPC 化 Daemon↔containerd？

---

## 注意事项

- 重构时不要破坏现有的"流式连接时序"：daemon 收到 `stream=true` 后要等 `StreamReady` 才能让 CLI 接管连接（见 `daemon/daemon.go` 的 `handleConnection`）。统一壳子时这块逻辑需要保留或重新设计
- `types.ShimRequest` 的强类型字段（`Signal/Args/Tty/Rows/Cols`）比 `map[string]string` 更安全，如果选方案 A，shim 层建议保留强类型（不强行统一为 map）
- 改完后要跑 [tests/run-all.sh](tests/run-all.sh) 全量回归

---

## 相关阅读

- `mini-docker交互式容器-it全链路解析.md` — 流式 I/O 转发链路
- `shim/shim说明.md` — shim 进程设计
- 真实 Docker 协议参考：containerd 项目的 `api/` 目录（gRPC 定义）
