package containerd

import "mini-docker/types"

// ExitInfo 退出信息类型别名
// 对齐 shim 协议：shim 退出时把 ExitInfo 写入 $RUNTIME_DIR/<id>/exitinfo，
// containerd 进程读取后通过 ReqGetExitInfo/ReqReadExitInfo RPC 返回给 Daemon
//
// 类型在 types.ExitInfo 中定义，本文件只做包级别名转发，
// 避免 client_linux.go/server_linux.go 等文件频繁 import types 包
type ExitInfo = types.ExitInfo
