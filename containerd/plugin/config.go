package plugin

import (
	"fmt"
	"os"

	"github.com/BurntSushi/toml"
)

/*
=======================================================================
  插件配置系统 —— 对齐 containerd 的 config.toml 配置机制

  真实 containerd 通过 /etc/containerd/config.toml 配置每个插件：
    [plugins."io.containerd.service.v1.garbage_collector"]
      interval = "5m"
    [plugins."io.containerd.snapshotter.v1.overlayfs"]
      root_path = "/var/lib/containerd/io.containerd.snapshotter.v1.overlayfs"

  mini-docker 对齐此设计，配置文件路径: /etc/mini-docker/containerd.toml

  配置加载优先级:
    1. 配置文件中的值（最高优先级）
    2. constants 包中的默认值（兜底）

=======================================================================
*/

// Config 插件管理器配置（对齐 containerd: config.toml 顶层结构）
type Config struct {
	// 全局配置
	DefaultSnapshotter string `toml:"default_snapshotter"` // 默认 snapshotter，"overlay" 或 "native"

	// 插件专属配置，key 为 PluginKey.String()，如 "service.metadata"
	// 对齐 containerd: [plugins."io.containerd.service.v1.xxx"]
	Plugins map[string]map[string]string `toml:"plugins"`
}

// DefaultConfig 返回基于 constants 包默认值的配置
func DefaultConfig() *Config {
	return &Config{
		DefaultSnapshotter: "overlay",
		Plugins: map[string]map[string]string{
			"service.metadata": {
				"path": "/var/lib/mini-docker/metadata/metadata.db",
			},
			"content.filesys": {
				"root": "/var/lib/mini-docker/content/sha256",
			},
			"snapshotter.overlay": {
				"root": "/var/lib/mini-docker/snapshots/overlay",
			},
			"snapshotter.native": {
				"root": "/var/lib/mini-docker/snapshots/overlay-native",
			},
			"service.gc": {
				"interval": "5m",
			},
		},
	}
}

// LoadConfig 从 TOML 文件加载配置
// 如果文件不存在，返回默认配置
func LoadConfig(path string) (*Config, error) {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		cfg := DefaultConfig()
		// 配置文件不存在时自动生成默认配置
		if writeErr := cfg.WriteFile(path); writeErr != nil {
			return cfg, fmt.Errorf("配置文件 %s 不存在，生成默认配置失败: %w", path, writeErr)
		}
		return cfg, nil
	}

	var cfg Config
	if _, err := toml.DecodeFile(path, &cfg); err != nil {
		return nil, fmt.Errorf("解析配置文件 %s 失败: %w", path, err)
	}

	// 合并默认值：配置文件中未指定的字段使用默认值
	cfg.mergeDefaults()

	return &cfg, nil
}

// mergeDefaults 将配置文件中未指定的字段填充为默认值
func (c *Config) mergeDefaults() {
	defaults := DefaultConfig()

	if c.DefaultSnapshotter == "" {
		c.DefaultSnapshotter = defaults.DefaultSnapshotter
	}

	if c.Plugins == nil {
		c.Plugins = defaults.Plugins
		return
	}

	// 合并每个插件的配置项
	for key, defaultVals := range defaults.Plugins {
		if c.Plugins[key] == nil {
			c.Plugins[key] = defaultVals
			continue
		}
		for k, v := range defaultVals {
			if _, exists := c.Plugins[key][k]; !exists {
				c.Plugins[key][k] = v
			}
		}
	}
}

// WriteFile 将配置写入 TOML 文件
func (c *Config) WriteFile(path string) error {
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("创建配置文件失败: %w", err)
	}
	defer f.Close()

	enc := toml.NewEncoder(f)
	if err := enc.Encode(c); err != nil {
		return fmt.Errorf("写入配置文件失败: %w", err)
	}
	return nil
}

// PluginConfig 获取指定插件的专属配置
func (c *Config) PluginConfig(key PluginKey) map[string]string {
	if c.Plugins == nil {
		return nil
	}
	return c.Plugins[key.String()]
}
