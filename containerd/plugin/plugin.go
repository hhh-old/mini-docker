// Package plugin 实现插件管理系统，对齐 containerd 的插件架构
// 在真实 containerd 中，所有核心组件（metadata/content/snapshotter/differ/gc/images）
// 都以插件形式注册，通过统一的 Plugin Manager 管理生命周期和依赖关系。
//
// 真实 containerd 插件类型:
//   - io.containerd.service.v1     : 服务插件 (metadata, content, images, diff, snapshots, gc, containers)
//   - io.containerd.snapshotter.v1 : 快照插件 (overlay, btrfs, native)
//   - io.containerd.differ.v1     : 差异插件 (walking, tar)
//   - io.containerd.content.v1    : 内容存储插件 (filesys)
//
// 插件生命周期:
//  1. Register: 插件通过 init() 函数注册自身
//  2. Init: PluginManager 按依赖顺序（拓扑排序）初始化所有插件
//  3. Get: 组件通过 type+ID 获取已初始化的插件实例
package plugin

import (
	"fmt"
	"io"
	"sort"
)

// PluginType 定义插件的类别
type PluginType string

const (
	TypeService     PluginType = "service"     // 服务插件 (metadata, images, gc, containers, task, shim, runtime)
	TypeSnapshotter PluginType = "snapshotter" // 快照插件 (overlay, btrfs, native)
	TypeDiffer      PluginType = "differ"      // 差异插件 (walking, tar)
	TypeContent     PluginType = "content"     // 内容存储插件 (filesys)
)

// PluginKey 通过 type+ID 唯一标识一个插件
type PluginKey struct {
	Type PluginType
	ID   string
}

// String 返回 PluginKey 的可读表示
func (k PluginKey) String() string {
	return fmt.Sprintf("%s.%s", k.Type, k.ID)
}

// Plugin 表示一个已注册的插件
type Plugin struct {
	Type     PluginType  // 插件类型类别
	ID       string      // 类型内的唯一标识 (如 "overlay", "walking")
	Init     InitFunc    // 初始化函数
	Depends  []PluginKey // 必须先初始化的依赖插件
	instance interface{} // 初始化后的实例（Init 调用后设置）
}

// PluginKey 返回插件的唯一标识
func (p *Plugin) PluginKey() PluginKey {
	return PluginKey{Type: p.Type, ID: p.ID}
}

// InitFunc 是插件初始化函数
// 接收一个上下文，提供对已初始化插件的访问
type InitFunc func(ic *InitContext) (interface{}, error)

// InitContext 为插件初始化提供依赖
type InitContext struct {
	Plugins *Manager          // 插件管理器引用，用于解析依赖
	Config  map[string]string // 插件专属配置（从 Config.Plugins[key] 解析）
	Global  *Config           // 全局配置引用，插件可读取 DefaultSnapshotter 等全局设置
}

// Get 是 m.Plugins.Get 的快捷方式，用于获取已初始化的插件实例
func (ic *InitContext) Get(typ PluginType, id string) (interface{}, error) {
	return ic.Plugins.Get(typ, id)
}

// GetPlugin 是类型安全的插件获取，将结果断言为指定类型
// 如果插件不存在或类型不匹配则返回错误
func GetPlugin[T any](ic *InitContext, typ PluginType, id string) (T, error) {
	var zero T
	inst, err := ic.Get(typ, id)
	if err != nil {
		return zero, err
	}
	typed, ok := inst.(T)
	if !ok {
		return zero, fmt.Errorf("插件 %s.%s 类型不匹配: 期望 %T, 实际 %T", typ, id, zero, inst)
	}
	return typed, nil
}

// Manager 管理插件的注册和初始化
type Manager struct {
	plugins map[PluginKey]*Plugin // 已注册的插件
	order   []PluginKey           // 拓扑排序后的初始化顺序
}

// NewManager 创建新的插件管理器
func NewManager() *Manager {
	return &Manager{
		plugins: make(map[PluginKey]*Plugin),
	}
}

// Register 注册一个插件
// 如果 PluginKey 已存在则返回错误
func (m *Manager) Register(p *Plugin) error {
	key := p.PluginKey()
	if _, exists := m.plugins[key]; exists {
		return fmt.Errorf("插件 %s 已注册，不允许重复注册", key)
	}
	m.plugins[key] = p
	return nil
}

// Get 获取已初始化的插件实例（按 type+ID）
// 如果插件未注册或未初始化则返回错误
func (m *Manager) Get(typ PluginType, id string) (interface{}, error) {
	key := PluginKey{Type: typ, ID: id}
	p, ok := m.plugins[key]
	if !ok {
		return nil, fmt.Errorf("插件 %s 未注册", key)
	}
	if p.instance == nil {
		return nil, fmt.Errorf("插件 %s 尚未初始化", key)
	}
	return p.instance, nil
}

// GetByType 获取指定类型的所有已初始化插件
// 返回 map[id]instance
func (m *Manager) GetByType(typ PluginType) map[string]interface{} {
	result := make(map[string]interface{})
	for key, p := range m.plugins {
		if key.Type == typ && p.instance != nil {
			result[key.ID] = p.instance
		}
	}
	return result
}

// Initialize 按依赖顺序初始化所有已注册的插件
// 1. 对依赖关系做拓扑排序
// 2. 按排序顺序依次调用每个插件的 Init 函数
// 3. 检测循环依赖并返回错误
//
// config 参数对齐 containerd 的 config.toml 配置机制：
//   - config.DefaultSnapshotter: 全局默认 snapshotter
//   - config.Plugins[key]: 每个插件的专属配置
//
// 如果 config 为 nil，使用 DefaultConfig()
func (m *Manager) Initialize(config *Config) error {
	// 配置为空时使用默认值
	if config == nil {
		config = DefaultConfig()
	}

	// 拓扑排序，确定初始化顺序
	order, err := m.topologicalSort()
	if err != nil {
		return err
	}
	m.order = order

	// 按拓扑顺序依次初始化
	for _, key := range order {
		p := m.plugins[key]

		// 获取插件专属配置
		pluginConfig := config.PluginConfig(key)
		if pluginConfig == nil {
			pluginConfig = make(map[string]string)
		}

		ic := &InitContext{
			Plugins: m,
			Config:  pluginConfig,
			Global:  config,
		}

		instance, err := p.Init(ic)
		if err != nil {
			return fmt.Errorf("初始化插件 %s 失败: %w", key, err)
		}
		p.instance = instance
	}

	return nil
}

// Close 按逆初始化顺序关闭所有插件
// 对实现了 io.Closer 接口的插件调用 Close()
func (m *Manager) Close() error {
	// 逆序关闭，先初始化的后关闭
	var firstErr error
	for i := len(m.order) - 1; i >= 0; i-- {
		key := m.order[i]
		p := m.plugins[key]
		if p.instance == nil {
			continue
		}
		if closer, ok := p.instance.(io.Closer); ok {
			if err := closer.Close(); err != nil && firstErr == nil {
				firstErr = fmt.Errorf("关闭插件 %s 失败: %w", key, err)
			}
		}
	}
	return firstErr
}

// topologicalSort 对插件依赖关系进行拓扑排序
// 使用 Kahn 算法（BFS），同时检测循环依赖
func (m *Manager) topologicalSort() ([]PluginKey, error) {
	// 构建邻接表和入度表
	// adj[A] = [B, C] 表示 A 初始化后 B 和 C 才能初始化（A → B, A → C）
	// 即 B 和 C 依赖 A
	adj := make(map[PluginKey][]PluginKey)
	inDegree := make(map[PluginKey]int)

	// 初始化所有节点的入度为 0
	for key := range m.plugins {
		inDegree[key] = 0
	}

	// 构建依赖图
	for key, p := range m.plugins {
		for _, dep := range p.Depends {
			// 检查依赖是否存在
			if _, exists := m.plugins[dep]; !exists {
				return nil, fmt.Errorf("插件 %s 依赖的 %s 不存在", key, dep)
			}
			// dep → key: dep 初始化后 key 才能初始化
			adj[dep] = append(adj[dep], key)
			inDegree[key]++
		}
	}

	// Kahn 算法：从入度为 0 的节点开始 BFS
	var queue []PluginKey
	for key, deg := range inDegree {
		if deg == 0 {
			queue = append(queue, key)
		}
	}

	// 对队列排序，保证确定性输出（相同入度的节点按 PluginKey 字典序）
	sort.Slice(queue, func(i, j int) bool {
		return queue[i].String() < queue[j].String()
	})

	var order []PluginKey
	for len(queue) > 0 {
		// 取出队首节点
		current := queue[0]
		queue = queue[1:]
		order = append(order, current)

		// 减少所有后继节点的入度
		var nextBatch []PluginKey
		for _, successor := range adj[current] {
			inDegree[successor]--
			if inDegree[successor] == 0 {
				nextBatch = append(nextBatch, successor)
			}
		}

		// 排序后加入队列，保证确定性
		sort.Slice(nextBatch, func(i, j int) bool {
			return nextBatch[i].String() < nextBatch[j].String()
		})
		queue = append(queue, nextBatch...)
	}

	// 如果排序后的节点数不等于总节点数，说明存在循环依赖
	if len(order) != len(m.plugins) {
		// 找出参与循环的节点
		var cyclic []PluginKey
		for key, deg := range inDegree {
			if deg > 0 {
				cyclic = append(cyclic, key)
			}
		}
		return nil, fmt.Errorf("检测到循环依赖，涉及插件: %v", cyclic)
	}

	return order, nil
}
