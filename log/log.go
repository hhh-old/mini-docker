package log

import (
	"io"
	"log"
	"os"
	"sync"
)

// Level 日志级别
type Level int

const (
	DebugLevel Level = iota
	InfoLevel
	WarnLevel
	ErrorLevel
)

var (
	logger  *log.Logger
	level   Level = InfoLevel //level 是全局唯一的日志打印级别
	levelMu sync.RWMutex
)

func init() {
	logger = log.New(os.Stderr, "", log.LstdFlags|log.Lmicroseconds) //默认输出到进程的 os.Stderr
}

// SetLevel 设置全局日志级别
func SetLevel(l Level) {
	levelMu.Lock()
	level = l
	levelMu.Unlock()
}

// SetOutput 设置日志输出目标
// log.SetOutput(os.Stdout)          // 改到 stdout
// log.SetOutput(someFile)           // 改到文件
// log.SetOutput(io.Discard)         // 完全关闭
func SetOutput(w io.Writer) {
	logger.SetOutput(w)
}

// SetFlags 设置日志前缀标志
func SetFlags(flags int) {
	logger.SetFlags(flags)
}

// Debugf 输出 DEBUG 级别日志
func Debugf(format string, args ...interface{}) { logf(DebugLevel, "[DEBUG] "+format, args...) }

// Infof 输出 INFO 级别日志
func Infof(format string, args ...interface{}) { logf(InfoLevel, "[INFO] "+format, args...) }

// Warnf 输出 WARN 级别日志
func Warnf(format string, args ...interface{}) { logf(WarnLevel, "[WARN] "+format, args...) }

// Errorf 输出 ERROR 级别日志
func Errorf(format string, args ...interface{}) { logf(ErrorLevel, "[ERROR] "+format, args...) }

func logf(l Level, format string, args ...interface{}) {
	levelMu.RLock()
	cur := level
	levelMu.RUnlock()
	//当前设置 cur 			会打印的日志
	//DebugLevel (0) 		DEBUG、INFO、WARN、ERROR 全打印
	//InfoLevel (1) 		INFO、WARN、ERROR 打印，DEBUG 被过滤
	//WarnLevel (2) 		WARN、ERROR 打印，DEBUG、INFO 被过滤
	//ErrorLevel (3) 		只有 ERROR 打印
	if l < cur { //如果这条日志的级别 低于 当前设置的级别，就不打印。
		return
	}
	logger.Printf(format, args...)
}
