package walking

// LayerDiffer 层差异计算器实现
// 对齐 containerd: diff.Comparer 的默认实现
type LayerDiffer struct{}

// NewLayerDiffer 创建层差异计算器
func NewLayerDiffer() *LayerDiffer {
	return &LayerDiffer{}
}
