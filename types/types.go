package types

type ExitInfo struct {
	ExitCode int    `json:"exit_code"`
	ExitedAt string `json:"exited_at"`
}

type ShimRequest struct {
	Type   string   `json:"type"`
	Signal int      `json:"signal,omitempty"`
	Args   []string `json:"args,omitempty"`
	Tty    bool     `json:"tty,omitempty"`
	Rows   uint16   `json:"rows,omitempty"`
	Cols   uint16   `json:"cols,omitempty"`
}

type ShimResponse struct {
	Success bool        `json:"success"`
	Message string      `json:"message,omitempty"`
	Data    interface{} `json:"data,omitempty"`
	Stream  bool        `json:"stream,omitempty"`
}

type LogEntry struct {
	Log    string `json:"log"`
	Stream string `json:"stream"`
	Time   string `json:"time"`
}

type OverlayDirs struct {
	Merged string `json:"merged"` //挂载点（容器看到的最终文件系统）
	Upper  string `json:"upper"`  //可写层目录（容器修改写入这里）
	Work   string `json:"work"`   //OverlayFS 内部工作目录
	Lower  string `json:"lower"`  //只读层目录链（镜像层）, OverlayFS lowerdir（多层用 ":" 分隔，由 Snapshotter.Mounts() 构建）
}
