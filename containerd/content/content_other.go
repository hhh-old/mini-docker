//go:build !linux

package content

import "fmt"

// ErrNotSupported 非 Linux 平台的错误（保留用于 content_linux.go 中的错误判断）
var ErrNotSupported = fmt.Errorf("content store 仅支持 Linux 平台")
