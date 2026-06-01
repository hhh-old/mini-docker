//go:build linux

package shim

// 请求类型常量，避免魔法字符串
// 参考 Docker/containerd-shim: 使用常量定义请求类型，提高代码可维护性
const (
	// ReqState 获取容器状态
	ReqState = "state"
	// ReqKill 发送信号到容器
	ReqKill = "kill"
	// ReqExitInfo 获取容器退出信息
	ReqExitInfo = "exit_info"
	// ReqExec 在容器内执行命令
	ReqExec = "exec"
	// ReqAttach 附加到容器终端
	ReqAttach = "attach"
	// ReqResize 调整终端大小
	ReqResize = "resize"
	// ReqStart 启动容器
	ReqStart = "start"
	// ReqPause 暂停容器
	ReqPause = "pause"
	// ReqUnpause 恢复容器
	ReqUnpause = "unpause"
	// ReqShutdown 关闭 shim
	ReqShutdown = "shutdown"
)
