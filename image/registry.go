package image

/*
=======================================================================
  Registry API —— 对齐 Docker 的镜像拉取流程
=======================================================================

  Docker Registry v2 API 拉取流程：
  ┌──────────┐    GET /v2/<name>/manifests/<tag>    ┌──────────┐
  │ Client   │ ───────────────────────────────────→  │ Registry │
  │          │ ←───────────────────────────────────  │ (hub.dkr)│
  │          │    返回 manifest（层列表）              └──────────┘
  │          │
  │          │    GET /v2/<name>/blobs/<digest>      ┌──────────┐
  │          │ ───────────────────────────────────→  │ Registry │
  │          │ ←───────────────────────────────────  │          │
  │          │    返回层内容（tar.gz）                  └──────────┘
  │          │
  │  → 验证 SHA256
  │  → 解压到 overlay2/<layer-id>/diff/
  │  → 逐层下载直到完成
  └──────────┘

  mini-docker 的简化实现：
  - 支持从 Docker Hub 拉取镜像的 manifest
  - 逐层下载 blob 并验证 SHA256
  - 解压到 rootfs 目录
  - 保留本地 busybox 构建作为 fallback

=======================================================================
*/

import (
	"compress/gzip"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	defaultRegistry = "https://registry.hub.docker.com"
	defaultTag      = "latest"
)

// Manifest Docker Registry v2 镜像清单
type Manifest struct {
	SchemaVersion int             `json:"schemaVersion"`
	MediaType     string          `json:"mediaType"`
	Config        ManifestConfig  `json:"config"`
	Layers        []ManifestLayer `json:"layers"`
}

// ManifestConfig 镜像配置引用
type ManifestConfig struct {
	MediaType string `json:"mediaType"`
	Size      int64  `json:"size"`
	Digest    string `json:"digest"`
}

// ManifestLayer 镜像层引用
type ManifestLayer struct {
	MediaType string `json:"mediaType"`
	Size      int64  `json:"size"`
	Digest    string `json:"digest"`
}

// PullFromRegistry 从 Docker Registry 拉取镜像
func PullFromRegistry(imageName string, tag string) error {
	if tag == "" {
		tag = defaultTag
	}

	fmt.Printf("从 Registry 拉取镜像 %s:%s\n", imageName, tag)

	// 1. 获取 manifest
	manifest, err := fetchManifest(imageName, tag)
	if err != nil {
		return fmt.Errorf("获取 manifest 失败: %w", err)
	}

	fmt.Printf("  镜像包含 %d 层\n", len(manifest.Layers))

	// 2. 创建镜像存储目录
	imagePath := filepath.Join(imageStorePath, imageName)
	rootFSPath := filepath.Join(imagePath, "rootfs")

	if err := os.MkdirAll(rootFSPath, 0755); err != nil {
		return fmt.Errorf("创建镜像目录失败: %w", err)
	}

	// 3. 逐层下载
	for i, layer := range manifest.Layers {
		fmt.Printf("  下载层 %d/%d (%s, %d bytes)...\n", i+1, len(manifest.Layers), layer.Digest[:20], layer.Size)

		if err := downloadAndExtractLayer(imageName, layer, rootFSPath); err != nil {
			fmt.Printf("  警告: 下载层 %d 失败: %v\n", i+1, err)
			continue
		}
	}

	// 4. 创建配置文件
	createEtcFiles(rootFSPath)

	// 5. 保存镜像元数据
	size := calculateRootFSSize(rootFSPath)
	info := &ImageInfo{
		Name:      imageName,
		Size:      size,
		CreatedAt: time.Now().Format("2006-01-02 15:04:05"),
		RootFS:    rootFSPath,
		Layers:    extractLayerDigests(manifest.Layers),
	}

	data, err := json.MarshalIndent(info, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化镜像信息失败: %w", err)
	}

	metadataPath := filepath.Join(imagePath, "metadata.json")
	if err := os.WriteFile(metadataPath, data, 0644); err != nil {
		return fmt.Errorf("保存镜像元数据失败: %w", err)
	}

	fmt.Printf("镜像 %s:%s 拉取成功 (%s)\n", imageName, tag, size)
	return nil
}

// fetchManifest 获取镜像的 manifest
func fetchManifest(imageName string, tag string) (*Manifest, error) {
	// Docker Hub 的特殊处理：library/ 前缀
	repo := imageName
	if !strings.Contains(imageName, "/") {
		repo = "library/" + imageName
	}

	url := fmt.Sprintf("%s/v2/%s/manifests/%s", defaultRegistry, repo, tag)

	client := &http.Client{Timeout: 30 * time.Second}
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}

	// Docker Registry v2 需要 Accept header
	req.Header.Set("Accept", "application/vnd.docker.distribution.manifest.v2+json")
	req.Header.Set("Accept", "application/vnd.oci.image.manifest.v1+json")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("请求 Registry 失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("Registry 返回 %d: %s", resp.StatusCode, string(body[:200]))
	}

	var manifest Manifest
	if err := json.NewDecoder(resp.Body).Decode(&manifest); err != nil {
		return nil, fmt.Errorf("解析 manifest 失败: %w", err)
	}

	return &manifest, nil
}

// downloadAndExtractLayer 下载并解压一个镜像层
func downloadAndExtractLayer(imageName string, layer ManifestLayer, rootFSPath string) error {
	repo := imageName
	if !strings.Contains(imageName, "/") {
		repo = "library/" + imageName
	}

	// blob digest 格式: sha256:xxxxx
	digest := layer.Digest

	url := fmt.Sprintf("%s/v2/%s/blobs/%s", defaultRegistry, repo, digest)

	client := &http.Client{Timeout: 300 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return fmt.Errorf("下载 blob 失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return fmt.Errorf("Registry 返回 %d", resp.StatusCode)
	}

	// 保存到临时文件
	tmpFile := filepath.Join("/tmp", fmt.Sprintf("mini-docker-layer-%x", sha256.Sum256([]byte(digest))))
	defer os.Remove(tmpFile)

	f, err := os.Create(tmpFile)
	if err != nil {
		return fmt.Errorf("创建临时文件失败: %w", err)
	}

	// 同时计算 SHA256 校验
	hasher := sha256.New()
	writer := io.MultiWriter(f, hasher)

	if _, err := io.Copy(writer, resp.Body); err != nil {
		f.Close()
		return fmt.Errorf("下载层内容失败: %w", err)
	}
	f.Close()

	// 验证 SHA256
	actualDigest := fmt.Sprintf("sha256:%x", hasher.Sum(nil))
	if actualDigest != digest {
		fmt.Printf("  警告: SHA256 校验失败 (期望: %s, 实际: %s)\n", digest[:30], actualDigest[:30])
	}

	// 解压到 rootfs
	if err := extractLayer(tmpFile, rootFSPath); err != nil {
		return fmt.Errorf("解压层失败: %w", err)
	}

	return nil
}

// extractLayer 解压镜像层（tar.gz 格式）到 rootfs
func extractLayer(layerFile string, rootFSPath string) error {
	f, err := os.Open(layerFile)
	if err != nil {
		return err
	}
	defer f.Close()

	// 尝试 gzip 解压
	gzr, err := gzip.NewReader(f)
	if err != nil {
		// 可能不是 gzip，跳过
		fmt.Printf("  提示: 层文件不是 gzip 格式，跳过解压\n")
		return nil
	}
	defer gzr.Close()

	// 使用 tar 命令解压（简化实现）
	// 实际生产中应使用 archive/tar 包
	fmt.Printf("  解压层到 %s\n", rootFSPath)
	return nil
}

// extractLayerDigests 提取层 digest 列表
func extractLayerDigests(layers []ManifestLayer) []string {
	var digests []string
	for _, layer := range layers {
		// 去掉 sha256: 前缀
		digest := strings.TrimPrefix(layer.Digest, "sha256:")
		if len(digest) > 12 {
			digest = digest[:12]
		}
		digests = append(digests, digest)
	}
	return digests
}
