//go:build linux

package containerd

import (
	"context"
	"encoding/json"
	"fmt"

	"mini-docker/containerd/diff"
)

// handleDiffApply 将层差异应用到 Active 快照
// args: digest（层 blob digest）, diff_id（未压缩 diffID）, key（目标 Active 快照 key）
func (c *Containerd) handleDiffApply(req Request) Response {
	svc := c.getDiffService()
	if svc == nil {
		return Response{Success: false, Message: "Diff Service 未初始化"}
	}

	digest := req.Args["digest"]
	diffID := req.Args["diff_id"]
	key := req.Args["key"]
	if digest == "" || key == "" {
		return Response{Success: false, Message: "需要指定 digest 和 key"}
	}

	if err := svc.Apply(context.Background(), digest, diffID, key); err != nil {
		return Response{Success: false, Message: fmt.Sprintf("应用层差异失败: %v", err)}
	}
	return Response{Success: true}
}

// handleDiffDiff 计算两个快照之间的差异，生成 blob 写入 Content Store
// args: lower_key（可为空，表示从空目录开始）, upper_key, config（JSON 序列化的 DiffConfig）
func (c *Containerd) handleDiffDiff(req Request) Response {
	svc := c.getDiffService()
	if svc == nil {
		return Response{Success: false, Message: "Diff Service 未初始化"}
	}

	lowerKey := req.Args["lower_key"]
	upperKey := req.Args["upper_key"]
	if upperKey == "" {
		return Response{Success: false, Message: "需要指定 upper_key"}
	}

	// 从请求中重建 diff 选项
	var opts []diff.DiffOpt
	if cfgJSON := req.Args["config"]; cfgJSON != "" {
		var cfg diff.DiffConfig
		if err := json.Unmarshal([]byte(cfgJSON), &cfg); err != nil {
			return Response{Success: false, Message: fmt.Sprintf("解析 diff 选项失败: %v", err)}
		}
		if cfg.MediaType != "" {
			opts = append(opts, diff.WithDiffMediaType(cfg.MediaType))
		}
		if len(cfg.Labels) > 0 {
			opts = append(opts, diff.WithDiffLabels(cfg.Labels))
		}
	}

	result, err := svc.Diff(context.Background(), lowerKey, upperKey, opts...)
	if err != nil {
		return Response{Success: false, Message: fmt.Sprintf("计算快照差异失败: %v", err)}
	}
	return Response{Success: true, Data: result}
}
