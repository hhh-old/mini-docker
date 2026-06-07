package images

/*
=======================================================================
  进度状态枚举 —— 镜像拉取/构建领域的 status 取值

  这些状态描述了"镜像操作正在做什么"或"结果如何"，属于领域层概念。
  对应的协议层类型（ProgressFrameData.Status）会通过本类型在 socket 上
  序列化传输。FrameType（progress/result）属于协议层，仍在 containerd 包。

  状态语义分组：
  - 进度帧状态（伴随 Type=progress 推送，提示用户当前阶段）
  - 结果帧状态（伴随 Type=result，作为终结帧的成功/失败标记）
=======================================================================
*/

// ProgressFrameStatus 进度帧的状态
//   - Downloading/Extracting/Building：进度帧的阶段性状态
//   - Warning：进度帧中提示非致命错误
//   - Complete/Error：结果帧的成功/失败标记
type ProgressFrameStatus string

const (
	// StatusDownloading 正在从 registry 下载镜像 manifest/layer
	StatusDownloading ProgressFrameStatus = "downloading"
	// StatusExtracting 正在解压镜像层（tar → overlay lowerdir）
	StatusExtracting ProgressFrameStatus = "extracting"
	// StatusBuilding 正在本地构建镜像（创建 rootfs、安装 busybox 等）
	StatusBuilding ProgressFrameStatus = "building"
	// StatusRegistering 正在把镜像元数据写入 boltdb（拉取/构建完成后）
	StatusRegistering ProgressFrameStatus = "registering"
	// StatusWarning 进度帧中的非致命警告（仅提示，不中断流程）
	StatusWarning ProgressFrameStatus = "warning"
	// StatusComplete 结果帧：操作成功
	StatusComplete ProgressFrameStatus = "complete"
	// StatusError 结果帧：操作失败
	StatusError ProgressFrameStatus = "error"
)
