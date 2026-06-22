//go:build !linux

package containerd

// handleContentInfo 非 Linux 平台桩实现
func (c *Containerd) handleContentInfo(req Request) Response {
	return Response{Success: false, Message: "仅支持 Linux 平台"}
}

// handleContentPath 非 Linux 平台桩实现
func (c *Containerd) handleContentPath(req Request) Response {
	return Response{Success: false, Message: "仅支持 Linux 平台"}
}

// handleContentExists 非 Linux 平台桩实现
func (c *Containerd) handleContentExists(req Request) Response {
	return Response{Success: false, Message: "仅支持 Linux 平台"}
}

// handleContentDelete 非 Linux 平台桩实现
func (c *Containerd) handleContentDelete(req Request) Response {
	return Response{Success: false, Message: "仅支持 Linux 平台"}
}

// handleContentWrite 非 Linux 平台桩实现
func (c *Containerd) handleContentWrite(req Request) Response {
	return Response{Success: false, Message: "仅支持 Linux 平台"}
}

// handleContentCommit 非 Linux 平台桩实现
func (c *Containerd) handleContentCommit(req Request) Response {
	return Response{Success: false, Message: "仅支持 Linux 平台"}
}

// handleContentWalk 非 Linux 平台桩实现
func (c *Containerd) handleContentWalk(req Request) Response {
	return Response{Success: false, Message: "仅支持 Linux 平台"}
}

// handleContentUpdate 非 Linux 平台桩实现
func (c *Containerd) handleContentUpdate(req Request) Response {
	return Response{Success: false, Message: "仅支持 Linux 平台"}
}
