//go:build linux

package container

/*
=======================================================================
  容器日志 —— 对齐 Docker 的日志驱动
=======================================================================

  Docker 日志系统：
  ┌──────────┐   stdout/stderr   ┌──────────┐   write   ┌───────────┐
  │ 容器进程  │ ───────────────→ │ Daemon   │ ───────→ │ json.log  │
  │          │                   │ (日志驱动) │          │ (磁盘文件) │
  └──────────┘                   └──────────┘           └───────────┘

  Docker 的默认日志驱动：json-file
  - 每行日志格式：{"log":"输出内容\n","stream":"stdout","time":"..."}
  - 存储路径：/var/lib/docker/containers/<id>/<id>-json.log

  mini-docker 的实现：
  - 在 Run() 中将容器 stdout/stderr 重定向到日志文件
  - 交互模式 (-it)：同时输出到终端和日志文件（类似 tee）
  - 后台模式 (-d)：只写日志文件

=======================================================================
*/

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// LogEntry 日志条目（对齐 Docker 的 json-log 格式）
type LogEntry struct {
	Log    string `json:"log"`    // 日志内容（含换行符）
	Stream string `json:"stream"` // stdout 或 stderr
	Time   string `json:"time"`   // RFC3339Nano 格式时间戳
}

// ContainerLogger 容器日志记录器
type ContainerLogger struct {
	logFile *os.File
	logPath string
}

// NewContainerLogger 创建容器日志记录器
func NewContainerLogger(containerID string) (*ContainerLogger, error) {
	shortID := containerID
	if len(shortID) > 12 {
		shortID = shortID[:12]
	}

	logDir := filepath.Join(containerDataDir, shortID)
	if err := os.MkdirAll(logDir, 0755); err != nil {
		return nil, fmt.Errorf("创建日志目录失败: %w", err)
	}

	logPath := filepath.Join(logDir, shortID+"-json.log")
	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return nil, fmt.Errorf("打开日志文件失败: %w", err)
	}

	return &ContainerLogger{
		logFile: f,
		logPath: logPath,
	}, nil
}

// WriteLog 写入一条日志
func (l *ContainerLogger) WriteLog(line string, stream string) error {
	entry := LogEntry{
		Log:    line,
		Stream: stream,
		Time:   time.Now().Format(time.RFC3339Nano),
	}

	data, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	data = append(data, '\n')

	_, err = l.logFile.Write(data)
	return err
}

// Close 关闭日志文件
func (l *ContainerLogger) Close() error {
	if l.logFile != nil {
		return l.logFile.Close()
	}
	return nil
}

// LogPath 返回日志文件路径
func (l *ContainerLogger) LogPath() string {
	return l.logPath
}

// TeeWriter 类似 tee 命令，同时写入两个 Writer
// 用于交互模式：同时输出到终端和日志文件
type TeeWriter struct {
	writers []io.Writer
	logger  *ContainerLogger
	stream  string // "stdout" 或 "stderr"
}

// NewTeeWriter 创建 TeeWriter
func NewTeeWriter(terminal io.Writer, logger *ContainerLogger, stream string) *TeeWriter {
	return &TeeWriter{
		writers: []io.Writer{terminal},
		logger:  logger,
		stream:  stream,
	}
}

// Write 同时写入终端和日志文件
func (t *TeeWriter) Write(p []byte) (n int, err error) {
	// 写入终端
	for _, w := range t.writers {
		w.Write(p)
	}

	// 写入日志文件
	if t.logger != nil {
		t.logger.WriteLog(string(p), t.stream)
	}

	return len(p), nil
}

// LogWriter 只写日志不写终端（后台模式）
type LogWriter struct {
	logger *ContainerLogger
	stream string
}

// NewLogWriter 创建只写日志的 Writer
func NewLogWriter(logger *ContainerLogger, stream string) *LogWriter {
	return &LogWriter{
		logger: logger,
		stream: stream,
	}
}

// Write 只写入日志文件
func (l *LogWriter) Write(p []byte) (n int, err error) {
	if l.logger != nil {
		l.logger.WriteLog(string(p), l.stream)
	}
	return len(p), nil
}
