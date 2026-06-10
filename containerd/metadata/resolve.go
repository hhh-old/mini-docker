package metadata

import (
	"fmt"

	bolt "go.etcd.io/bbolt"

	"mini-docker/utils"
)

/*
=======================================================================
  镜像引用解析 —— 统一入口
=======================================================================

  对齐 Docker/Containerd: 镜像引用（imageRef）有两种形式：
    - name:tag  如 test-app:latest、ubuntu:24.04
    - imageID   如 0d12cd5b28f7（64 位十六进制）

  ResolveImageRef 是唯一的解析入口，调用方无需关心用户输入的是哪种形式。

  解析策略：
  1. 先按 name:tag 查 tags bucket（最常见路径，O(1) 查找）
  2. 失败则按 imageID 查 images bucket（支持 docker rmi <imageID> 用法）

=======================================================================
*/

// ResolveImageRef 统一解析镜像引用，返回完整的 *Image 记录
// 支持 name:tag 和 imageID 两种格式，调用方无需关心引用类型
func ResolveImageRef(tx *bolt.Tx, imageRef string) (*Image, error) {
	// 路径1: 按 name:tag 查找（最常见路径）
	name, tag := utils.ParseImageTag(imageRef)
	if tag == "" {
		tag = "latest"
	}
	if id, err := ResolveImageID(tx, name, tag); err == nil {
		if m, err := LoadImage(tx, id); err == nil {
			return m, nil
		}
	}

	// 路径2: 按 imageID 查找（对齐 Docker: docker rmi 0d12cd5b28f7）
	if m, err := LoadImage(tx, imageRef); err == nil {
		return m, nil
	}

	return nil, fmt.Errorf("镜像 %s 不存在", imageRef)
}
