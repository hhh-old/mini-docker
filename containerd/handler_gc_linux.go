//go:build linux

package containerd

import (
	"context"
	"fmt"
	"time"
)

/*
=======================================================================
  GC 处理器 —— 手动触发垃圾回收
=======================================================================

  对齐 containerd: 通过 RPC 接口暴露 GC 触发能力，运维/调试时可手动执行。
  周期性 GC 由 Collector.Start() 在后台自动跑，不需要 RPC 入口。

  GC 算法本身（标记/清扫）在 containerd/gc 包实现，handler 只做：
  1. 解析请求参数
  2. 调用 collector.Run()
  3. 封装结果为 Response

=======================================================================
*/

// handleGC 处理手动触发垃圾回收请求
func (c *Containerd) handleGC(req Request) Response {
	if c.gcCollector == nil {
		return Response{Success: false, Message: "GC 未初始化"}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	stats, err := c.gcCollector.Run(ctx)
	if err != nil {
		return Response{Success: false, Message: fmt.Sprintf("GC 执行失败: %v", err)}
	}
	return Response{Success: true, Data: stats}
}
