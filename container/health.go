package container

/*
=======================================================================
  健康检查 —— 对齐 Docker 的 HEALTHCHECK 机制
=======================================================================

  Docker 健康检查：
  - Dockerfile 中定义: HEALTHCHECK --interval=30s --timeout=3s CMD curl -f http://localhost/
  - Docker 周期执行检查命令
  - 根据退出码判断: 0=healthy, 1=unhealthy, 2=reserved

  状态流转：
  starting → healthy ⇄ unhealthy

  mini-docker 的实现：
  - 支持 --health-cmd 和 --health-interval 参数
  - Daemon 周期执行检查命令
  - 更新容器健康状态

=======================================================================
*/

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"mini-docker/utils"
)

// HealthStatus 健康状态
type HealthStatus string

const (
	HealthStarting  HealthStatus = "starting"  // 容器启动中，尚未检查
	HealthHealthy   HealthStatus = "healthy"   // 健康检查通过
	HealthUnhealthy HealthStatus = "unhealthy" // 健康检查失败
	HealthNone      HealthStatus = "none"      // 未配置健康检查
)

// HealthConfig 健康检查配置
type HealthConfig struct {
	Cmd      string        // 检查命令
	Interval time.Duration // 检查间隔（默认 30s）
	Timeout  time.Duration // 超时时间（默认 3s）
	Retries  int           // 连续失败次数后标记为 unhealthy（默认 3）
}

// DefaultHealthConfig 返回默认的健康检查配置
func DefaultHealthConfig() *HealthConfig {
	return &HealthConfig{
		Interval: 30 * time.Second,
		Timeout:  3 * time.Second,
		Retries:  3,
	}
}

// HealthResult 健康检查结果
type HealthResult struct {
	Status    HealthStatus `json:"status"`
	LastCheck string       `json:"last_check"`
	Output    string       `json:"output"`
	ExitCode  int          `json:"exit_code"`
	FailCount int          `json:"fail_count"` // 连续失败计数
}

// ParseHealthConfig 从容器信息中解析健康检查配置
func ParseHealthConfig(info *ContainerInfo) *HealthConfig {
	config := DefaultHealthConfig()
	if info == nil {
		return config
	}
	if info.HealthCmd != "" {
		config.Cmd = info.HealthCmd
	}
	if info.HealthInterval != "" {
		if d, err := time.ParseDuration(info.HealthInterval); err == nil {
			config.Interval = d
		}
	}
	if info.HealthTimeout != "" {
		if d, err := time.ParseDuration(info.HealthTimeout); err == nil {
			config.Timeout = d
		}
	}
	if info.HealthRetries > 0 {
		config.Retries = info.HealthRetries
	}
	return config
}

// RunHealthCheck 执行一次健康检查
func RunHealthCheck(info *ContainerInfo, config *HealthConfig) *HealthResult {
	prevResult, _ := LoadHealthResult(info.ID)
	failCount := 0
	if prevResult != nil {
		failCount = prevResult.FailCount
	}

	result := &HealthResult{
		LastCheck: utils.NowFormatted(),
		FailCount: failCount,
	}

	cmd := exec.Command("nsenter",
		"-t", fmt.Sprintf("%d", info.Pid),
		"-m", "-p", "-n",
		"/bin/sh", "-c", config.Cmd,
	)

	if config.Timeout > 0 {
		timer := time.AfterFunc(config.Timeout, func() {
			if cmd.Process != nil {
				cmd.Process.Kill()
			}
		})
		defer timer.Stop()
	}

	output, err := cmd.CombinedOutput()
	result.Output = string(output)

	if err != nil {
		result.ExitCode = 1
		result.FailCount++
	} else {
		result.ExitCode = 0
		result.FailCount = 0
	}

	if result.FailCount >= config.Retries {
		result.Status = HealthUnhealthy
	} else if result.ExitCode == 0 {
		result.Status = HealthHealthy
	} else {
		result.Status = HealthStarting
	}

	return result
}

// SaveHealthResult 保存健康检查结果
func SaveHealthResult(containerID string, result *HealthResult) error {
	shortID := utils.FormatShortID(containerID)

	healthDir := filepath.Join(containerDataDir, shortID)
	if err := os.MkdirAll(healthDir, 0755); err != nil {
		return err
	}

	data, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return err
	}

	return os.WriteFile(filepath.Join(healthDir, "health.json"), data, 0644)
}

// LoadHealthResult 加载健康检查结果
func LoadHealthResult(containerID string) (*HealthResult, error) {
	shortID := utils.FormatShortID(containerID)

	data, err := os.ReadFile(filepath.Join(containerDataDir, shortID, "health.json"))
	if err != nil {
		return &HealthResult{Status: HealthNone}, nil
	}

	var result HealthResult
	if err := json.Unmarshal(data, &result); err != nil {
		return &HealthResult{Status: HealthNone}, nil
	}

	return &result, nil
}
