package image

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

/*
=======================================================================
  镜像管理 —— Docker 镜像系统的简化实现
=======================================================================

  Docker 镜像的核心概念：

  1. 镜像（Image）: 一个只读的文件系统模板，包含运行应用所需的一切
  2. 层（Layer）: 镜像由多个只读层叠加而成（Union File System）
  3. 仓库（Repository）: 存储相关镜像的集合
  4. 标签（Tag）: 镜像的版本标识

  Docker 镜像的存储结构：
  /var/lib/docker/
  ├── overlay2/          ← 镜像层存储（OverlayFS）
  │   ├── <layer-id>/
  │   │   ├── diff/     ← 该层的文件差异
  │   │   ├── link      ← 指向该层的符号链接
  │   │   └── lower     ← 下层 ID 列表
  │   └── <merged-id>/
  │       ├── merged/   ← 叠加后的完整文件系统
  │       ├── upper/    ← 容器可写层
  │       └── work/     ← OverlayFS 工作目录
  ├── image/
  │   └── overlay2/
  │       ├── imagedb/  ← 镜像元数据
  │       └── layerdb/  ← 层元数据
  └── containers/       ← 容器运行时数据

  本实现简化为：
  /var/lib/mini-docker/images/
  └── <image-name>/
      ├── metadata.json  ← 镜像元数据
      └── rootfs/        ← 镜像的根文件系统（含 busybox + 210+ 命令）
=======================================================================
*/

const (
	imageStorePath = "/var/lib/mini-docker/images"
)

type ImageInfo struct {
	Name      string   `json:"name"`
	Size      string   `json:"size"`
	CreatedAt string   `json:"created_at"`
	RootFS    string   `json:"rootfs"`
	Layers    []string `json:"layers"`
}

/*
=======================================================================
  Pull —— 拉取镜像（简化版，自动构建可用 RootFS）
=======================================================================

  真实的 docker pull 流程：
  1. 客户端向 Registry 发送 API 请求
     GET /v2/<name>/manifests/<tag>
  2. Registry 返回镜像的 manifest（包含层信息）
  3. 客户端逐层下载：
     GET /v2/<name>/blobs/<digest>
  4. 每层下载后验证 SHA256 校验和
  5. 解压并存储到 overlay2 目录
  6. 更新镜像元数据

  Docker Registry API（v2）:
  - GET /v2/                          → 检查 Registry 版本
  - GET /v2/<name>/tags/list          → 列出标签
  - GET /v2/<name>/manifests/<ref>    → 获取镜像 manifest
  - GET /v2/<name>/blobs/<digest>     → 下载镜像层

  本实现简化为：自动创建包含 busybox 的完整 rootfs 作为镜像。
  类似于 docker pull alpine 的效果 —— 拉取后即可直接运行。
=======================================================================
*/

func Pull(imageName string) error {
	imagePath := filepath.Join(imageStorePath, imageName)
	rootFSPath := filepath.Join(imagePath, "rootfs")

	if _, err := os.Stat(imagePath); err == nil {
		return fmt.Errorf("镜像 %s 已存在，请先删除: rm -rf %s", imageName, imagePath)
	}

	fmt.Printf("Pulling image %s...\n", imageName)

	fmt.Printf("  创建 rootfs 目录结构...\n")
	if err := createRootFSDirs(rootFSPath); err != nil {
		return fmt.Errorf("创建目录失败: %w", err)
	}

	fmt.Printf("  创建配置文件...\n")
	if err := createEtcFiles(rootFSPath); err != nil {
		return fmt.Errorf("创建配置文件失败: %w", err)
	}

	if err := createDevNodes(rootFSPath); err != nil {
		fmt.Printf("  警告: 创建设备节点失败: %v\n", err)
	}

	fmt.Printf("  安装 busybox（提供基础命令）...\n")
	if err := setupBusybox(rootFSPath); err != nil {
		fmt.Printf("  警告: busybox 安装失败: %v\n", err)
		fmt.Printf("  镜像已创建但缺少基础命令，请手动安装 busybox 到 %s/bin/\n", rootFSPath)
	}

	size := calculateRootFSSize(rootFSPath)

	info := &ImageInfo{
		Name:      imageName,
		Size:      size,
		CreatedAt: time.Now().Format("2006-01-02 15:04:05"),
		RootFS:    rootFSPath,
		Layers:    []string{"base-layer"},
	}

	data, err := json.MarshalIndent(info, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化镜像信息失败: %w", err)
	}

	metadataPath := filepath.Join(imagePath, "metadata.json")
	if err := os.WriteFile(metadataPath, data, 0644); err != nil {
		return fmt.Errorf("保存镜像元数据失败: %w", err)
	}

	fmt.Printf("Image %s (%s) created successfully\n", imageName, size)
	fmt.Printf("  Ready to use: mini-docker run -it %s /bin/sh\n", imageName)

	return nil
}

func createRootFSDirs(rootFSPath string) error {
	requiredDirs := []string{
		"bin", "sbin",
		"usr", "usr/bin", "usr/sbin", "usr/lib", "usr/local", "usr/local/bin",
		"lib", "lib64",
		"etc", "etc/init.d",
		"proc", "sys", "dev", "dev/pts", "dev/shm",
		"tmp", "root", "run",
		"var", "var/log", "var/run", "var/tmp", "var/cache", "var/lib",
		"opt", "home",
	}

	for _, dir := range requiredDirs {
		if err := os.MkdirAll(filepath.Join(rootFSPath, dir), 0755); err != nil {
			return fmt.Errorf("创建目录 %s 失败: %w", dir, err)
		}
	}

	os.Chmod(filepath.Join(rootFSPath, "root"), 0700)
	os.Chmod(filepath.Join(rootFSPath, "tmp"), 01777)

	return nil
}

func createEtcFiles(rootFSPath string) error {
	etcFiles := map[string]string{
		"hostname":      "mini-docker",
		"resolv.conf":   "nameserver 8.8.8.8\nnameserver 8.8.4.4\n",
		"hosts":         "127.0.0.1\tlocalhost\n::1\t\tlocalhost\n",
		"passwd":        "root:x:0:0:root:/root:/bin/sh\nnobody:x:65534:65534:nobody:/nonexistent:/bin/false\n",
		"group":         "root:x:0:\nnogroup:x:65534:\n",
		"shadow":        "root::0:0:99999:7:::\nnobody:*:0:0:99999:7:::\n",
		"os-release":    "NAME=\"MiniDocker\"\nVERSION=\"1.0\"\nID=minidocker\nPRETTY_NAME=\"MiniDocker Container\"\n",
		"nsswitch.conf": "passwd:         files\ngroup:          files\nshadow:         files\nhosts:          files dns\nnetworks:       files\nprotocols:      files\nservices:       files\n",
		"profile":       "export PATH=/bin:/sbin:/usr/bin:/usr/sbin:/usr/local/bin\nexport HOME=/root\nexport PS1='# '\nexport TERM=linux\n",
		"fstab":         "proc            /proc   proc    defaults        0 0\ntmpfs           /tmp    tmpfs   defaults,nosuid,nodev 0 0\ndevpts          /dev/pts devpts  defaults        0 0\n",
		"issue":         "Welcome to MiniDocker Container\n",
		"shells":        "/bin/sh\n/bin/ash\n/bin/bash\n",
		"protocols":     "ip\t0\tIP\nicmp\t1\tICMP\ntcp\t6\tTCP\nudp\t17\tUDP\n",
		"services":      "ssh\t22/tcp\nhttp\t80/tcp\nhttps\t443/tcp\n",
	}

	for filename, content := range etcFiles {
		filePath := filepath.Join(rootFSPath, "etc", filename)
		if err := os.WriteFile(filePath, []byte(content), 0644); err != nil {
			return fmt.Errorf("创建 %s 失败: %w", filename, err)
		}
	}

	os.Chmod(filepath.Join(rootFSPath, "etc", "shadow"), 0640)

	return nil
}

func createDevNodes(rootFSPath string) error {
	devDir := filepath.Join(rootFSPath, "dev")

	nullPath := filepath.Join(devDir, "null")
	if err := os.WriteFile(nullPath, nil, 0644); err != nil {
		return err
	}

	return nil
}

func calculateRootFSSize(rootFSPath string) string {
	var size int64
	filepath.Walk(rootFSPath, func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if !info.IsDir() {
			size += info.Size()
		}
		return nil
	})

	const (
		KB = 1024
		MB = 1024 * KB
		GB = 1024 * MB
	)

	switch {
	case size >= GB:
		return fmt.Sprintf("%.1fg", float64(size)/float64(GB))
	case size >= MB:
		return fmt.Sprintf("%.1fm", float64(size)/float64(MB))
	case size >= KB:
		return fmt.Sprintf("%.1fk", float64(size)/float64(KB))
	default:
		return fmt.Sprintf("%db", size)
	}
}

/*
=======================================================================
  ListImages —— 列出本地镜像
=======================================================================

  docker images 的实现：
  1. 遍历镜像存储目录
  2. 读取每个镜像的 metadata.json
  3. 格式化输出

  Docker 的镜像存储在 imagedb 中，使用 content-addressable 存储：
  镜像 ID = SHA256(镜像配置 JSON)
=======================================================================
*/

func ListImages() ([]*ImageInfo, error) {
	if err := os.MkdirAll(imageStorePath, 0755); err != nil {
		return nil, err
	}

	entries, err := os.ReadDir(imageStorePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var images []*ImageInfo
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		metadataPath := filepath.Join(imageStorePath, entry.Name(), "metadata.json")
		data, err := os.ReadFile(metadataPath)
		if err != nil {
			continue
		}

		var img ImageInfo
		if err := json.Unmarshal(data, &img); err != nil {
			continue
		}

		images = append(images, &img)
	}

	return images, nil
}

/*
=======================================================================
  RemoveImage —— 删除本地镜像
=======================================================================

  docker rmi 的实现：
  1. 检查镜像是否存在
  2. 删除镜像目录（包含 rootfs 和元数据）
=======================================================================
*/

func RemoveImage(imageName string) error {
	imagePath := filepath.Join(imageStorePath, imageName)
	if _, err := os.Stat(imagePath); os.IsNotExist(err) {
		return fmt.Errorf("镜像 %s 不存在", imageName)
	}
	return os.RemoveAll(imagePath)
}
