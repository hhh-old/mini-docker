//go:build !linux

package containerd

import (
	"fmt"
	"net"
)

func ConnectContainerd() (net.Conn, error) {
	return nil, fmt.Errorf("containerd 独立进程仅支持 Linux 平台")
}

func SendRequest(req Request) (*Response, error) {
	return nil, fmt.Errorf("containerd 独立进程仅支持 Linux 平台")
}

func SendStreamRequest(req Request) (net.Conn, *Response, error) {
	return nil, nil, fmt.Errorf("containerd 独立进程仅支持 Linux 平台")
}

func WriteResponse(conn net.Conn, resp Response) error {
	return fmt.Errorf("containerd 独立进程仅支持 Linux 平台")
}
