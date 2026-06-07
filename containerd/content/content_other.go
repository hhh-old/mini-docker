//go:build !linux

package content

import "fmt"

var ErrNotSupported = fmt.Errorf("content store 仅支持 Linux 平台")
