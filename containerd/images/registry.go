package images

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"

	"mini-docker/constants"
	"mini-docker/containerd/content"
)

/*
=======================================================================
  Docker Registry HTTP API V2 客户端
=======================================================================

  对齐 Docker 的镜像拉取流程：

  ┌──────────┐         ┌──────────────┐         ┌──────────────┐
  │  Client  │ ──────→ │  Auth Server │ ──────→ │  Registry    │
  │          │  1. GET │  (auth.      │  3. GET │  (registry-1.│
  │          │  manifest│   docker.io)│  blob   │   docker.io) │
  │          │         │              │         │              │
  │          │ ←────── │  2. Bearer   │ ←────── │  4. Layer    │
  │          │  401    │     Token    │  tar.gz │     Data     │
  └──────────┘         └──────────────┘         └──────────────┘

  Docker Registry API V2 核心接口：
  - GET  /v2/                          → 检查 API 版本
  - GET  /v2/<name>/manifests/<ref>    → 获取镜像 manifest
  - GET  /v2/<name>/blobs/<digest>     → 下载镜像层（blob）
  - HEAD /v2/<name>/blobs/<digest>     → 检查层是否存在

  Docker Hub 认证流程：
  1. 客户端请求 GET /v2/<name>/manifests/<ref>
  2. 返回 401，WWW-Authenticate 头包含认证服务地址
  3. 客户端请求认证服务获取 Bearer Token
  4. 带着 Token 重新请求 manifest
  5. 逐层下载 blob，每层带 Token

  镜像引用格式：
  - library/alpine        → index.docker.io/library/alpine:latest
  - alpine                → index.docker.io/library/alpine:latest
  - alpine:3.18           → index.docker.io/library/alpine:3.18
  - myregistry.com/myapp  → myregistry.com/myapp:latest

=======================================================================
*/

// RegistryClient Docker Registry API V2 客户端
type RegistryClient struct {
	mu           sync.Mutex // 保护 token/tokenScope 的并发访问
	host         string
	scheme       string //协议
	token        string //当前缓存的 Bearer Token
	tokenScope   string //及其适用范围（tokenScope）
	client       *http.Client
	contentStore *content.Service // blob 存储服务（对齐 containerd: 所有 blob 写入通过 Content Service 完成）
}

// OCIManifest OCI Image Manifest V2 格式（对齐 Docker）
// https://docs.docker.com/registry/spec/manifest-v2-2/
// 镜像的“清单文件”结构,记录了该镜像包含哪些“层”（Layers）以及对应的元数据（大小、Hash 值/Digest、媒体类型）。
// 说明“这个镜像是由哪些文件组成的，以及你应该按什么顺序、去哪里下载它们。”
// 例子：
//
//	{
//	 "schemaVersion": 2,
//	 "mediaType": "application/vnd.oci.image.manifest.v1+json",
//	 "config": {
//	   "mediaType": "application/vnd.oci.image.config.v1+json",
//	   "size": 1507,
//	   "digest": "sha256:b5b2b2c5038227b3e5b3062831e5038227b3e5b3..."
//	 },
//	 "layers": [
//	   {
//	     "mediaType": "application/vnd.oci.image.layer.v1.tar+gzip",
//	     "size": 2814321,
//	     "digest": "sha256:e5038227b3e5b3062831e5038227b3e5b306283..."
//	   }
//	 ]
//	}
type OCIManifest struct {
	SchemaVersion int    `json:"schemaVersion"`       // 规范版本
	MediaType     string `json:"mediaType,omitempty"` // 媒体类型，声明这个 JSON 文件本身的标准格式类型。常见值： OCI 标准：application/vnd.oci.image.manifest.v1+json ；Docker v2.2 标准：application/vnd.docker.distribution.manifest.v2+json
	//含义：它是指向“镜像配置文件”（即我们前面提到的存放环境变量、启动命令、CPU架构等信息的 JSON）的引用。
	//注意：Config 字段本身并不包含具体的配置信息，而是一个 OCIDescriptor（内容描述符）。它只记录了配置文件的哈希值（Digest）和大小。客户端需要拿这个哈希值再次调用 DownloadBlob 才能拿到真正的配置内容。
	Config OCIDescriptor `json:"config"` // 镜像配置文件的描述符
	//含义：一个有序的数组，里面包含了一组 OCIDescriptor。
	//关键点：顺序极其重要！
	//数组的第 0 个元素（Layers[0]）是最底层（Base Layer），通常是类似 alpine、ubuntu 的基础系统。
	//随后的元素依次叠加在上面（比如安装的软件、拷贝的文件）。
	//容器引擎在解压这些层构建文件系统时，必须严格按照从 Layers[0] 到 Layers[n] 的顺序依次解压、覆盖。如果顺序乱了，文件系统的合并（OverlayFS）就会出错。
	Layers      []OCIDescriptor   `json:"layers"`                // 镜像层文件的描述符列表
	Annotations map[string]string `json:"annotations,omitempty"` // 附加标注信息
}

// OCIDescriptor OCI 内容描述符（引用 blob）
// 在 OCIManifest 中，无论是 Config 还是 Layers 数组，它们使用的都是 OCIDescriptor 结构体
type OCIDescriptor struct {
	MediaType string `json:"mediaType"` // 数据块的类型（是压缩包还是 JSON）
	Digest    string `json:"digest"`    // 该数据块的唯一 SHA-256 指纹,也是下载时的 Key,用来索引要下载的目标文件
	Size      int64  `json:"size"`      // 数据块的实际大小（字节数）
}

// OCIPlatform 平台标识（OCI Image Index 规范）
// 用于在多架构 manifest list 中选择目标平台的子 manifest
type OCIPlatform struct {
	Architecture string `json:"architecture"` // CPU 架构，如 amd64、arm64、arm
	OS           string `json:"os"`           // 操作系统，如 linux、windows
	// Variant / OSVersion 在绝大多数场景下用不到，按需扩展
}

// OCIManifestIndexEntry OCI Image Index（manifest list）中的单个条目
type OCIManifestIndexEntry struct {
	MediaType string      `json:"mediaType"` // 子 manifest 的媒体类型
	Digest    string      `json:"digest"`    // 子 manifest 的 digest，content-addressable
	Size      int64       `json:"size"`      // 子 manifest 大小
	Platform  OCIPlatform `json:"platform"`  // 该子 manifest 适用的平台
}

// OCIManifestIndex OCI Image Index（又称 manifest list）
// Registry 在按 tag 请求时通常返回这个，按 digest 请求才返回单架构 OCIManifest
type OCIManifestIndex struct {
	MediaType   string                  `json:"mediaType"`
	Manifests   []OCIManifestIndexEntry `json:"manifests"`
	Annotations map[string]string       `json:"annotations,omitempty"`
}

// OCIImageConfig OCI Image Config（镜像配置，存储在 blob 中）,是OCIManifest中Config文件解析出来的实体
// https://github.com/opencontainers/image-spec/blob/main/config.md
type OCIImageConfig struct {
	//Architecture（CPU 架构）如 amd64、arm64。
	//用途：容器引擎启动前会检查宿主机的 CPU 架构是否与此匹配。如果在一台 x86_64 的电脑上尝试运行一个 arm64 的镜像，引擎通常会发出警告或报错。
	Architecture string `json:"architecture"`
	//OS（操作系统类型）：通常是 linux，也可以是 windows 或 darwin。
	OS      string       `json:"os"`
	Config  OCIConfig    `json:"config"`
	RootFS  OCIRootFS    `json:"rootfs"`
	History []OCIHistory `json:"history,omitempty"`
}

// OCIConfig 容器运行时配置
type OCIConfig struct {
	Cmd          []string            `json:"Cmd,omitempty"`          // 默认执行命令的参数
	Env          []string            `json:"Env,omitempty"`          // 环境变量列表
	WorkingDir   string              `json:"WorkingDir,omitempty"`   // 默认工作目录
	ExposedPorts map[string]struct{} `json:"ExposedPorts,omitempty"` // 声明要暴露的端口
	Labels       map[string]string   `json:"Labels,omitempty"`       // 镜像标签（Key-Value 元数据）
	Entrypoint   []string            `json:"Entrypoint,omitempty"`   // 默认启动的可执行程序
	User         string              `json:"User,omitempty"`         // 运行容器进程的系统用户（安全控制）
	//Entrypoint 与 Cmd 的配合（运行什么命令？）
	//这是很多初学者容易混淆的地方。它们在容器启动时会被拼接成一个最终的执行命令。
	//Entrypoint：容器启动时的“主程序”。例如 ["python3"]。
	//Cmd：传给主程序的“默认参数”。例如 ["app.py"]。
	//当容器运行起来时，最终执行的命令就是 python3 app.py。
}

// OCIRootFS 文件系统的一致性地图
type OCIRootFS struct {
	Type    string   `json:"type"`     // 固定的文件系统层类型，通常是 "layers"
	DiffIDs []string `json:"diff_ids"` // 解压后的每一层的 SHA-256 哈希值
	//这里的 DiffIDs 与 Manifest 里的 digest 有什么区别？
	//Manifest 中的 digest 是网络传输和存储时的**压缩包（如 .tar.gz）**的哈希值。
	//OCIRootFS.DiffIDs 是这些压缩包被**解压后（原始的 .tar 归档）**的内容哈希值。
	//用途：当您的 mini-docker 把下载好的 .tar.gz 镜像层解压到本地目录时，它会计算解压后数据的哈希值，并与此处的 diff_ids 进行比对。这属于双重安全校验，确保解压出来的文件没有在本地磁盘发生损坏或丢失。
}

// OCIHistory 镜像构建审计日志
type OCIHistory struct {
	Created    string `json:"created,omitempty"`     // 构建时间
	CreatedBy  string `json:"created_by,omitempty"`  // 构建这一步所执行的 Dockerfile 命令（例如 "RUN apt-get update"）
	Comment    string `json:"comment,omitempty"`     // 备注
	EmptyLayer bool   `json:"empty_layer,omitempty"` // 是否是一个“空层”
	//EmptyLayer（空层）的作用是什么？
	//在 Dockerfile 中，像 RUN apt-get install 这种命令会产生实际的文件，从而在磁盘上生成一个物理镜像层（Layer Blob）。
	//但是，像 ENV MY_VAR=123、EXPOSE 80 或 WORKDIR /app 这种命令，它们只改变了元数据，没有产生任何新文件。
	//这种命令在构建时也会生成一条历史记录，但其 EmptyLayer 会被标记为 true。这就告诉容器引擎：“这一步没有对应的物理文件压缩包，不需要去下载或解压它。”
}

// tokenResponse Docker Hub 认证令牌响应
type tokenResponse struct {
	Token     string    `json:"token"`
	ExpiresIn float64   `json:"expires_in"`
	IssuedAt  time.Time `json:"issued_at"`
}

// NewRegistryClient 创建 Registry 客户端
// host 接受以下形式（用 url.Parse 解析，对多余路径/查询参数宽容地忽略）：
//   - "index.docker.io"                  → scheme=https, host=index.docker.io
//   - "my-registry:5000"                 → scheme=https, host=my-registry:5000
//   - "https://ghcr.io"                  → scheme=https, host=ghcr.io
//   - "http://192.168.1.10:5000"         → scheme=http,  host=192.168.1.10:5000
//   - "https://my-registry/v2/"          → scheme=https, host=my-registry（路径被忽略，统一由客户端拼 /v2/...）
//
// 不接受：含 userinfo 或除 http/https 外 scheme 的输入（Registry V2 不支持）
func NewRegistryClient(host string, contentStore *content.Service) *RegistryClient {
	if host == "" {
		panic("NewRegistryClient: host 不能为空")
	}
	if contentStore == nil {
		panic("NewRegistryClient: contentStore 不能为 nil（blob 去重和元数据一致性的前提）")
	}

	// url.Parse 比手撸 TrimPrefix 稳健：自动处理 scheme / 路径 / 查询 / fragment 的边界
	parsed, err := url.Parse(host)
	if err != nil {
		panic(fmt.Sprintf("NewRegistryClient: 解析 host 失败 %q: %v", host, err))
	}

	// 缺省 scheme → https（对齐 Docker 客户端默认行为）
	scheme := parsed.Scheme
	if scheme == "" {
		scheme = "https"
	}
	if scheme != "http" && scheme != "https" {
		panic(fmt.Sprintf("NewRegistryClient: 不支持的 scheme %q（仅支持 http/https）", scheme))
	}

	// url.Parse 把 "host" 同时支持 "host" 和 "scheme://host/path" 两种形式；
	// 任何情况下 Host 字段都会是干净的 "host[:port]"
	cleanHost := parsed.Host
	if cleanHost == "" {
		panic(fmt.Sprintf("NewRegistryClient: host 缺少主机部分 %q", host))
	}

	// Docker Hub 的 index.docker.io 仅用于认证和 manifest 查询
	// 实际的 blob 下载需要访问 registry-1.docker.io
	if cleanHost == "index.docker.io" {
		cleanHost = "registry-1.docker.io"
	}

	return &RegistryClient{
		host:   cleanHost,
		scheme: scheme,
		client: &http.Client{
			Timeout: constants.RegistryPullTimeout,
			Transport: &http.Transport{
				DialContext: (&net.Dialer{
					Timeout:   10 * time.Second, // TCP 连接超时，避免连不上 Docker Hub 时卡住
					KeepAlive: 30 * time.Second,
				}).DialContext,
				TLSHandshakeTimeout:   10 * time.Second,
				ResponseHeaderTimeout: 30 * time.Second,
			},
		},
		contentStore: contentStore,
	}
}

// ResolveImageRef 解析镜像引用，返回 (registry, repository, tag)
// 对齐 Docker 的镜像引用解析规则：
//
//	"alpine"                    → ("index.docker.io", "library/alpine", "latest")
//	"alpine:3.18"              → ("index.docker.io", "library/alpine", "3.18")
//	"library/alpine"           → ("index.docker.io", "library/alpine", "latest")
//	"myregistry.com/myapp"     → ("myregistry.com", "myapp", "latest")
//	"myregistry.com/myapp:v1"  → ("myregistry.com", "myapp", "v1")
func ResolveImageRef(imageRef string) (registry string, repository string, tag string) {
	parts := strings.SplitN(imageRef, ":", 2)
	name := parts[0]
	if len(parts) == 2 {
		tag = parts[1]
	} else {
		tag = "latest"
	}

	if strings.Contains(name, "/") {
		firstPart := strings.SplitN(name, "/", 2)[0]
		if strings.Contains(firstPart, ".") || strings.Contains(firstPart, ":") {
			registry = firstPart
			repository = strings.SplitN(name, "/", 2)[1]
		} else {
			registry = constants.DefaultRegistryHost
			repository = name
		}
	} else {
		registry = constants.DefaultRegistryHost
		repository = "library/" + name
	}

	return registry, repository, tag
}

// DownloadManifest 下载镜像的 OCI Manifest 并落盘到 Content Store
// reference 支持 tag（如 "latest"）和 digest（如 "sha256:abc..."）两种形式，对齐 Docker Registry API V2:
//   - 按 tag 请求时，Registry 可能返回 OCI Image Index（多架构 manifest list），本函数自动按当前平台向下解析到单架构 manifest
//   - 按 digest 请求时，Registry 直接返回单架构 manifest（content-addressable，不可变）
//
// 落盘策略：仅单架构 manifest 落盘到 Content Store，index 自身不落盘（content-addressable 模型下，index 可以从
// 子 manifest 的关系里重建，存储它没意义）。
func (rc *RegistryClient) DownloadManifest(repository, reference string) (*OCIManifest, []byte, error) {
	body, err := rc.fetchManifestBlob(repository, reference)
	if err != nil {
		return nil, nil, err
	}

	// OCI Image Index（多架构清单）时按平台选子 manifest 后重新拉取
	// 实际场景嵌套深度通常 ≤ 2，循环即可，无递归
	for isManifestIndex(body) {
		digest, err := selectPlatformDigest(body)
		if err != nil {
			return nil, nil, err
		}
		if body, err = rc.fetchManifestBlob(repository, digest); err != nil {
			return nil, nil, err
		}
	}

	var manifest OCIManifest
	if err := json.Unmarshal(body, &manifest); err != nil {
		return nil, nil, fmt.Errorf("解析 manifest 失败: %w", err)
	}

	// 对齐 containerd: 单架构 manifest 不再落盘到 Content Store
	// 原因：当前没有"通过 manifest digest 反查 blob"的用例（image 记录已直接
	// 保存 layer digest 数组），落盘只会写一份永远不会被读的死数据。
	// 后续若需镜像签名/多架构 index 支持，可在此处重新启用 storeManifestBlob，
	// 并在 metadata.Image 增加 Target 字段指向 manifest digest。
	return &manifest, body, nil
}

// fetchManifestBlob 纯 HTTP 拉取（认证 / 401 重试 / 读 body），不做任何解析
// 从原 fetchManifest 抽出，专注于"拉字节流"这一件事，便于 DownloadManifest 在循环里复用
// 401 走 ensureToken 重新认证后重试（最多 3 次），用循环而非递归，避免栈深度增长
func (rc *RegistryClient) fetchManifestBlob(repository, reference string) ([]byte, error) {
	const maxAuthRetries = 3

	for authAttempt := 0; authAttempt < maxAuthRetries; authAttempt++ {
		if err := rc.ensureToken(repository); err != nil {
			return nil, fmt.Errorf("获取认证令牌失败: %w", err)
		}

		rc.mu.Lock()
		token := rc.token
		rc.mu.Unlock()

		url := fmt.Sprintf("%s://%s/v2/%s/manifests/%s", rc.scheme, rc.host, repository, reference)

		req, err := http.NewRequest("GET", url, nil)
		if err != nil {
			return nil, fmt.Errorf("创建请求失败: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Add("Accept", "application/vnd.docker.distribution.manifest.v2+json")
		req.Header.Add("Accept", "application/vnd.docker.distribution.manifest.list.v2+json")
		req.Header.Add("Accept", "application/vnd.oci.image.manifest.v1+json")

		resp, err := rc.client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("请求 manifest 失败: %w", err)
		}

		if resp.StatusCode == http.StatusUnauthorized {
			resp.Body.Close()
			// 清除无效 token，下一轮循环会重新获取
			rc.mu.Lock()
			rc.token = ""
			rc.mu.Unlock()
			continue
		}

		if resp.StatusCode != http.StatusOK {
			errBody, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			truncateLen := len(errBody)
			if truncateLen > 200 {
				truncateLen = 200
			}
			return nil, fmt.Errorf("获取 manifest 失败 (HTTP %d): %s", resp.StatusCode, string(errBody[:truncateLen]))
		}

		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("读取 manifest 失败: %w", err)
		}
		return body, nil
	}

	return nil, fmt.Errorf("获取 manifest 失败: 认证重试 %d 次后仍返回 401（可能 Token 无效）", maxAuthRetries)
}

// isManifestIndex 判断 body 是否为 OCI Image Index（多架构 manifest list）
// 用 schemaVersion + mediaType 联合判断，比单纯看 HTTP Content-Type 可靠（不同 Registry 实现 Content-Type 命名不统一）
func isManifestIndex(body []byte) bool {
	var head struct {
		SchemaVersion int    `json:"schemaVersion"`
		MediaType     string `json:"mediaType"`
	}
	if err := json.Unmarshal(body, &head); err != nil {
		return false
	}
	if head.SchemaVersion != 2 {
		return false
	}
	return strings.Contains(head.MediaType, "manifest.list") ||
		strings.Contains(head.MediaType, "image.index")
}

// selectPlatformDigest 从 OCI Image Index 中按当前平台选择最匹配的子 manifest，返回其 digest
// 匹配优先级：1) 精确 GOOS+GOARCH  2) 同 OS 任意架构  3) 第一个
func selectPlatformDigest(body []byte) (string, error) {
	var index OCIManifestIndex
	if err := json.Unmarshal(body, &index); err != nil {
		return "", fmt.Errorf("解析 manifest index 失败: %w", err)
	}
	if len(index.Manifests) == 0 {
		return "", fmt.Errorf("manifest index 为空")
	}

	for i := range index.Manifests {
		if index.Manifests[i].Platform.OS == runtime.GOOS &&
			index.Manifests[i].Platform.Architecture == runtime.GOARCH {
			return index.Manifests[i].Digest, nil
		}
	}
	for i := range index.Manifests {
		if index.Manifests[i].Platform.OS == runtime.GOOS {
			return index.Manifests[i].Digest, nil
		}
	}
	return index.Manifests[0].Digest, nil
}

// BlobProgressFunc blob 下载进度回调
// total: 总字节数, completed: 已下载字节数
type BlobProgressFunc func(total, completed int64)

// DownloadBlob 下载镜像层 blob 到 Content Store
// 对齐 Docker: GET /v2/<name>/blobs/<digest>
// 对齐 containerd: 所有 blob 写入通过 contentStore.Writer() → Commit() 完成，确保文件与 BoltDB 元数据一致
// 返回下载的 blob 文件路径（content/sha256/<hex>）和 SHA256 校验结果
// progressFn 可为 nil，表示不需要进度回调
// 401 时自动重新认证并重试（与 fetchManifestBlob 行为对齐）
func (rc *RegistryClient) DownloadBlob(repository, digest string, expectedSize int64, progressFn BlobProgressFunc) (string, error) {
	ctx := context.Background()

	// 对齐 containerd: 通过 contentStore.Exists() 检查 blob 是否已存在
	// 这是 Docker 镜像分层复用机制的基石——相同 digest 的层只下载一次
	// 缓存命中走本地路径，无须重试
	if rc.contentStore.Exists(ctx, digest) {
		if progressFn != nil {
			progressFn(0, 0) // 通知 blob 已缓存
		}
		// 通过 content.Store 接口拿路径，避免 RegistryClient 触碰存储后端内部布局
		return rc.contentStore.Path(ctx, digest)
	}

	// HTTP 拉取（401 时循环重试，最多重试 maxAuthRetries 次）
	resp, err := rc.fetchBlobResponse(repository, digest)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	// 把流式字节写到 contentStore
	return rc.downloadBlobViaContentStore(resp, digest, expectedSize, progressFn)
}

// fetchBlobResponse 纯 HTTP 拉 blob 响应，401 时重试（与 fetchManifestBlob 行为对齐）
// 负责认证 / 401 重试 / 状态码校验，不做任何内容读取或存储
func (rc *RegistryClient) fetchBlobResponse(repository, digest string) (*http.Response, error) {
	const maxAuthRetries = 3
	url := fmt.Sprintf("%s://%s/v2/%s/blobs/%s", rc.scheme, rc.host, repository, digest)

	for authAttempt := 0; authAttempt < maxAuthRetries; authAttempt++ {
		if err := rc.ensureToken(repository); err != nil {
			return nil, fmt.Errorf("获取认证令牌失败: %w", err)
		}

		rc.mu.Lock()
		token := rc.token
		rc.mu.Unlock()

		req, err := http.NewRequest("GET", url, nil)
		if err != nil {
			return nil, fmt.Errorf("创建下载请求失败: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+token)

		resp, err := rc.client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("下载 blob 失败: %w", err)
		}

		if resp.StatusCode == http.StatusUnauthorized {
			resp.Body.Close()
			// 清除无效 token，下一轮循环会重新获取
			rc.mu.Lock()
			rc.token = ""
			rc.mu.Unlock()
			continue
		}

		if resp.StatusCode != http.StatusOK {
			errBody, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			truncateLen := len(errBody)
			if truncateLen > 200 {
				truncateLen = 200
			}
			return nil, fmt.Errorf("下载 blob 失败 (HTTP %d): %s", resp.StatusCode, string(errBody[:truncateLen]))
		}

		return resp, nil
	}

	return nil, fmt.Errorf("下载 blob 失败: 认证重试 %d 次后仍返回 401（可能 Token 无效）", maxAuthRetries)
}

// downloadBlobViaContentStore 通过 contentStore.Writer() 下载 blob
// 对齐 containerd: blob 写入的唯一正确路径
// 流程: contentStore.Writer() → io.Copy(网络流 → writer) → Commit(校验 digest + 写元数据)
func (rc *RegistryClient) downloadBlobViaContentStore(resp *http.Response, digest string, expectedSize int64, progressFn BlobProgressFunc) (string, error) {
	ctx := context.Background()

	// 解析 mediaType：layer blob 通常是 tar+gzip，config blob 通常是 json
	mediaType := ""
	if strings.Contains(digest, "tar") {
		mediaType = "application/vnd.oci.image.layer.v1.tar+gzip"
	} else {
		mediaType = "application/vnd.oci.image.config.v1+json"
	}

	writer, err := rc.contentStore.Writer(ctx, digest, resp.ContentLength, mediaType)
	if err != nil {
		return "", fmt.Errorf("创建 content writer 失败: %w", err)
	}
	defer writer.Close()

	// 分块下载并报告进度（对齐 Docker: docker pull 显示的下载进度条）
	buf := make([]byte, constants.RegistryBlobChunkSize)
	// 优先使用 HTTP Content-Length，当服务端使用 chunked 传输未返回 Content-Length 时，
	// 回退到 manifest 中记录的 expectedSize，确保进度百分比始终可显示
	total := resp.ContentLength
	if total <= 0 && expectedSize > 0 {
		total = expectedSize
	}
	var completed int64
	lastReportTime := time.Now()

	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			if _, writeErr := writer.Write(buf[:n]); writeErr != nil {
				return "", fmt.Errorf("写入 blob 失败: %w", writeErr)
			}
			completed += int64(n)
			// 每 500ms 报告一次进度，避免过于频繁的进度回调
			if progressFn != nil && time.Since(lastReportTime) >= 500*time.Millisecond {
				progressFn(total, completed)
				lastReportTime = time.Now()
			}
		}
		if readErr != nil {
			if readErr == io.EOF {
				break
			}
			return "", fmt.Errorf("读取 blob 数据失败: %w", readErr)
		}
	}

	// 最终进度报告
	if progressFn != nil {
		progressFn(total, completed)
	}

	// 对齐 containerd: Commit 时 contentStore 内部会:
	// 1. 校验计算的 digest 与期望 digest 是否一致
	// 2. 将临时文件 rename 为最终文件（原子操作）
	// 3. 将 Info 元数据写入 BoltDB
	if err := writer.Commit(ctx, digest); err != nil {
		return "", fmt.Errorf("提交 blob 失败: %w", err)
	}

	// 通过 content.Store 接口拿路径，避免 RegistryClient 触碰存储后端内部布局
	return rc.contentStore.Path(ctx, digest)
}

// GetImageConfig 下载并解析镜像配置
// 对齐 containerd: 通过 contentStore.Reader() 读取已存储的 blob，而非直接操作文件系统
func (rc *RegistryClient) GetImageConfig(repository string, configDesc *OCIDescriptor) (*OCIImageConfig, error) {
	_, err := rc.DownloadBlob(repository, configDesc.Digest, configDesc.Size, nil)
	if err != nil {
		return nil, fmt.Errorf("下载镜像配置失败: %w", err)
	}

	// 通过 contentStore.Reader() 读取，确保通过统一的存储接口访问
	ctx := context.Background()
	reader, err := rc.contentStore.Reader(ctx, configDesc.Digest)
	if err != nil {
		return nil, fmt.Errorf("读取镜像配置失败: %w", err)
	}
	defer reader.Close()

	data, err := io.ReadAll(reader)
	if err != nil {
		return nil, fmt.Errorf("读取镜像配置数据失败: %w", err)
	}

	var config OCIImageConfig
	if err := json.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("解析镜像配置失败: %w", err)
	}

	return &config, nil
}

// ensureToken 确保 Registry 认证令牌有效
// 对齐 Docker Hub 的 Bearer Token 认证流程：
// 1. 发送未认证请求到 /v2/
// 2. 从 401 响应的 WWW-Authenticate 头解析认证服务地址
// 3. 请求认证服务获取 Bearer Token
func (rc *RegistryClient) ensureToken(repository string) error {
	rc.mu.Lock()
	defer rc.mu.Unlock()

	if rc.token != "" && rc.tokenScope == repository {
		return nil
	}

	checkURL := fmt.Sprintf("%s://%s/v2/", rc.scheme, rc.host)
	req, err := http.NewRequest("GET", checkURL, nil)
	if err != nil {
		return err
	}

	resp, err := rc.client.Do(req)
	if err != nil {
		return fmt.Errorf("连接 Registry 失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK {
		rc.token = ""
		return nil
	}

	if resp.StatusCode != http.StatusUnauthorized {
		return fmt.Errorf("意外的 HTTP 状态码: %d", resp.StatusCode)
	}

	authHeader := resp.Header.Get("WWW-Authenticate")
	if authHeader == "" {
		return fmt.Errorf("缺少 WWW-Authenticate 头")
	}

	realm, service := parseAuthHeader(authHeader)
	if realm == "" {
		return fmt.Errorf("无法解析认证头: %s", authHeader)
	}

	tokenURL := fmt.Sprintf("%s?service=%s&scope=repository:%s:pull", realm, service, repository)
	tokenReq, err := http.NewRequest("GET", tokenURL, nil)
	if err != nil {
		return err
	}

	tokenResp, err := rc.client.Do(tokenReq)
	if err != nil {
		return fmt.Errorf("请求认证令牌失败: %w", err)
	}
	defer tokenResp.Body.Close()

	if tokenResp.StatusCode != http.StatusOK {
		return fmt.Errorf("获取认证令牌失败 (HTTP %d)", tokenResp.StatusCode)
	}

	var tokenData tokenResponse
	if err := json.NewDecoder(tokenResp.Body).Decode(&tokenData); err != nil {
		return fmt.Errorf("解析认证令牌失败: %w", err)
	}
	//向授权服务器申请临时 Token，并将其缓存，供后续的 Manifest 和 Blob 请求使用。
	rc.token = tokenData.Token
	rc.tokenScope = repository
	return nil
}

// parseAuthHeader 解析 WWW-Authenticate 响应头
// 格式: Bearer realm="https://auth.docker.io/token",service="registry.docker.io",scope="repository:library/alpine:pull"
func parseAuthHeader(header string) (realm, service string) {
	if !strings.HasPrefix(header, "Bearer ") {
		return
	}

	params := strings.TrimPrefix(header, "Bearer ")
	for _, part := range strings.Split(params, ",") {
		part = strings.TrimSpace(part)
		kv := strings.SplitN(part, "=", 2)
		if len(kv) != 2 {
			continue
		}
		value := strings.Trim(kv[1], "\"")
		switch kv[0] {
		case "realm":
			realm = value
		case "service":
			service = value
		}
	}
	//为什么 return 是空的？
	//这在 Go 语言中被称为裸返回（Bare Return）。
	//当你在函数声明中给返回值赋予了明确的变量名（也就是这里的 realm 和 service）时，Go 编译器会在函数内部自动初始化这两个变量（string 类型的零值是空字符串 ""）。
	//此时，如果你在函数体内直接写一个不带任何参数的 return，Go 就会自动把当前 realm 和 service 这两个变量的值作为结果返回。
	return
}

/*
=======================================================================
  Registry Mirror 配置（对齐 Docker: /etc/docker/daemon.json）
=======================================================================

  配置文件格式（/etc/mini-docker/daemon.json）：
  {
    "registry-mirrors": [
      "https://registry.cn-hangzhou.aliyuncs.com",
      "https://mirror.ccs.tencentyun.com"
    ]
  }

  当拉取 Docker Hub 镜像时，按顺序尝试 mirror，全部失败后回退到原始地址。
  自定义 registry（如 myregistry.com/myapp）不受 mirror 影响。
=======================================================================
*/

// DaemonConfig daemon 配置（对齐 Docker: /etc/docker/daemon.json）
type DaemonConfig struct {
	RegistryMirrors []string `json:"registry-mirrors"`
}

var (
	configOnce sync.Once
	daemonCfg  *DaemonConfig
)

// loadConfig 加载 daemon 配置文件（仅执行一次）
func loadConfig() *DaemonConfig {
	configOnce.Do(func() {
		daemonCfg = &DaemonConfig{}
		data, err := os.ReadFile(constants.DaemonConfigPath)
		if err != nil {
			return
		}
		if err := json.Unmarshal(data, daemonCfg); err != nil {
			fmt.Printf("  警告: 解析配置文件 %s 失败: %v\n", constants.DaemonConfigPath, err)
		}
	})
	return daemonCfg
}

// GetRegistryMirrors 获取配置的 registry mirror 列表
func GetRegistryMirrors() []string {
	cfg := loadConfig()
	return cfg.RegistryMirrors
}
