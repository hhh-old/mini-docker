//go:build linux

package containerd

import (
	"encoding/json"
	"log"
	"net"
	"time"

	"mini-docker/containerd/events"
)

// handlePublishEvent 处理 Daemon 或其他客户端发布的事件。
// 请求参数：
//   - topic: 事件 topic，例如 "/tasks/exit"
//   - namespace: 命名空间（可选，默认 "default"）
//   - timestamp: RFC3339 时间字符串（可选）
//   - event: 事件 payload 的 JSON 字符串
func (c *Containerd) handlePublishEvent(req Request) Response {
	svc := c.getEventService()
	if svc == nil {
		return Response{Success: false, Message: "events service not available"}
	}

	topic := req.Args["topic"]
	if topic == "" {
		return Response{Success: false, Message: "missing topic"}
	}

	var ts time.Time
	if v := req.Args["timestamp"]; v != "" {
		if t, err := time.Parse(time.RFC3339Nano, v); err == nil {
			ts = t
		}
	}

	var payload interface{}
	if v := req.Args["event"]; v != "" {
		payload = parseEventPayload(topic, []byte(v))
	}

	svc.Publish(&events.Envelope{
		Timestamp: ts,
		Namespace: req.Args["namespace"],
		Topic:     topic,
		Event:     payload,
	})

	return Response{Success: true}
}

// handleSubscribeEvents 处理事件订阅请求，建立长连接并持续推送事件。
// 请求参数：
//   - filters: JSON 字符串数组，topic glob 过滤规则
func (c *Containerd) handleSubscribeEvents(req Request, conn net.Conn) Response {
	svc := c.getEventService()
	if svc == nil {
		return Response{Success: false, Message: "events service not available"}
	}

	var filters []string
	if v := req.Args["filters"]; v != "" {
		_ = json.Unmarshal([]byte(v), &filters)
	}

	sub, err := svc.Subscribe(filters...)
	if err != nil {
		return Response{Success: false, Message: err.Error()}
	}

	// 发送流式握手响应，告知客户端订阅已建立
	if err := WriteResponse(conn, Response{Success: true, Stream: true}); err != nil {
		svc.Unsubscribe(sub)
		return Response{Success: false, Message: err.Error()}
	}

	// 在 goroutine 中持续推送事件，避免阻塞 routeRequest 的调用方。
	// 连接关闭时 Unsubscribe 并退出。
	go func() {
		defer svc.Unsubscribe(sub)
		defer conn.Close()

		encoder := json.NewEncoder(conn)
		for ev := range sub.Ch() {
			if err := encoder.Encode(ev); err != nil {
				log.Printf("事件流写入失败: %v", err)
				return
			}
		}
	}()

	// 返回空 Response，因为连接生命周期已由 goroutine 接管。
	// handleConnection 看到 stream 类型不会调用 conn.Close。
	return Response{}
}

// handleGetEventArchive 获取事件归档。
// 请求参数：
//   - since: RFC3339Nano 时间字符串（可选）
//   - until: RFC3339Nano 时间字符串（可选）
func (c *Containerd) handleGetEventArchive(req Request) Response {
	svc := c.getEventService()
	if svc == nil {
		return Response{Success: false, Message: "events service not available"}
	}

	var since, until time.Time
	if v := req.Args["since"]; v != "" {
		if t, err := time.Parse(time.RFC3339Nano, v); err == nil {
			since = t
		}
	}
	if v := req.Args["until"]; v != "" {
		if t, err := time.Parse(time.RFC3339Nano, v); err == nil {
			until = t
		}
	}

	archive := svc.GetArchive(since, until)
	return Response{Success: true, Data: archive}
}

// parseEventPayload 根据 topic 把 JSON payload 解析为对应的具体事件类型。
// 未知 topic 时退化为 map[string]interface{}，保证不丢事件。
func parseEventPayload(topic string, raw []byte) interface{} {
	var target interface{}
	switch topic {
	case "/containers/create":
		target = &events.ContainerCreate{}
	case "/containers/delete":
		target = &events.ContainerDelete{}
	case "/containers/update":
		target = &events.ContainerUpdate{}
	case "/images/create", "/images/pull":
		target = &events.ImagePull{}
	case "/images/delete":
		target = &events.ImageDelete{}
	case "/images/commit":
		target = &events.ImageCommit{}
	case "/tasks/create":
		target = &events.TaskCreate{}
	case "/tasks/start":
		target = &events.TaskStart{}
	case "/tasks/exit":
		target = &events.TaskExit{}
	case "/tasks/delete":
		target = &events.TaskDelete{}
	case "/tasks/paused":
		target = &events.TaskPause{}
	case "/tasks/resumed":
		target = &events.TaskResume{}
	case "/tasks/unhealthy":
		target = &events.TaskUnhealthy{}
	case "/gc/run":
		target = &events.GCRun{}
	default:
		var m map[string]interface{}
		if err := json.Unmarshal(raw, &m); err != nil {
			return string(raw)
		}
		return m
	}

	if err := json.Unmarshal(raw, target); err != nil {
		var m map[string]interface{}
		if err := json.Unmarshal(raw, &m); err != nil {
			return string(raw)
		}
		return m
	}
	return target
}
