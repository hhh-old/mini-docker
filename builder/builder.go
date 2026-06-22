package builder

/*
=======================================================================
  Dockerfile 构建器 —— 对齐 Docker 的镜像构建流程
=======================================================================

  Docker 构建流程：
  ┌──────────────────────────────────────────────────────────────┐
  │  1. 解析 Dockerfile                                          │
  │  2. 逐条执行指令                                             │
  │  3. 每条指令生成一个镜像层                                    │
  │  4. 利用 OverlayFS 的 Copy-on-Write 实现层叠加              │
  │  5. 最终生成镜像元数据                                       │
  └──────────────────────────────────────────────────────────────┘

  支持的 Dockerfile 指令：
  - FROM <image>          基础镜像
  - RUN <command>         执行命令（生成新层）
  - COPY <src> <dst>      复制文件（生成新层）
  - CMD <command>         默认启动命令（元数据，不生成层）
  - ENV <key>=<value>     环境变量（元数据）
  - WORKDIR <path>        工作目录（元数据）
  - EXPOSE <port>         暴露端口（元数据）

  构建原理（利用 OverlayFS）：
  ┌─────────────────────────────────────────────────────────────┐
  │  FROM myos                                                  │
  │  ↓  创建临时 OverlayFS：lower=myos/rootfs                   │
  │                                                              │
  │  RUN apt-get update                                         │
  │  ↓  在 overlay 的 upper 层执行命令                           │
  │  ↓  upper 层包含所有修改 → 保存为 Layer 1                   │
  │                                                              │
  │  COPY app.py /app/                                          │
  │  ↓  直接将文件写入 overlay 的 upper 层                      │
  │  ↓  upper 层包含新文件 → 保存为 Layer 2                     │
  │                                                              │
  │  CMD ["python", "app.py"]                                   │
  │  ↓  只记录元数据，不生成新层                                 │
  └─────────────────────────────────────────────────────────────┘

=======================================================================
*/

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"mini-docker/containerd/content"
	"mini-docker/containerd/diff"
	"mini-docker/containerd/metadata"
	"mini-docker/containerd/snapshots"
	"mini-docker/utils"
)

type BuildConfig struct {
	DockerfilePath string
	ContextDir     string
	Tag            string
	Service        BuildService
}

// BuildService 提供构建过程中需要的镜像操作（与 containerd 解耦）
// daemon/builder 调用方负责注入具体实现（通常包装 containerd.Client）。
type BuildService interface {
	// ResolveImage 通过 name[:tag] 解析已有镜像，返回 snapshot ID
	ResolveImage(imageRef string) (string, error)
	// RegisterImage 注册一个已构建好的镜像（写入元数据 DB）
	// 入参为 metadata.Image，Size 字段被 omitempty 忽略不入 boltdb
	RegisterImage(info *metadata.Image) error
	// Snapshotter 返回 Snapshotter 接口，用于注册构建层的快照元数据
	// 对齐 containerd: 构建器通过 Snapshotter.Prepare-Commit 注册每层快照，
	// 确保 GC 和 lowerDirs() 能正确感知构建镜像的层
	Snapshotter() snapshots.Snapshotter
	// ContentStore 返回 Content Store 接口，用于 Differ 计算层差异写入 blob
	// 对齐 containerd: 构建器通过 Differ.Diff() 计算每层的真实 tar blob digest
	ContentStore() content.Store
	// Differ 返回层差异计算器接口，由插件系统注入
	// 对齐 containerd: diff 服务可插拔，构建器通过此接口计算层差异
	Differ() diff.Differ
}

type BuildResult struct {
	ImageName string
	Tag       string
	Layers    []string
	ImageID   string
	Info      *metadata.Image
}

type DockerfileInstruction struct {
	Instruction  string
	Arguments    []string
	LineNum      int
	IsExecFormat bool // true = JSON 数组格式（如 CMD ["/bin/cat", "file"]），不经过 shell
}

// ParseDockerfile 解析 Dockerfile
func ParseDockerfile(path string) ([]DockerfileInstruction, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("打开 Dockerfile 失败: %w", err)
	}
	defer file.Close()

	var instructions []DockerfileInstruction
	scanner := bufio.NewScanner(file)
	lineNum := 0

	for scanner.Scan() { // 逐行读取文本
		lineNum++
		line := strings.TrimSpace(scanner.Text()) //去除首尾空格

		// 跳过空行和注释
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		// 解析指令
		instruction, args, isExec, ok := parseInstructionLine(line)
		if !ok {
			continue
		}

		// 处理多行指令（反斜杠续行）
		// 对齐 Docker: 续行只是把多行文本拼接为一条指令，不应当作新指令解析
		// 因此续行使用 shellSplit（而非 parseInstructionLine），避免续行被误判为新指令/JSON 格式
		for strings.HasSuffix(line, "\\") && scanner.Scan() {
			lineNum++
			nextLine := strings.TrimSpace(scanner.Text())
			// 续行内容只是参数的延续，用 shellSplit 解析后追加
			nextArgs := shellSplit(nextLine)
			if len(nextArgs) > 0 {
				args = append(args, nextArgs...)
			}
			line = nextLine
		}

		instructions = append(instructions, DockerfileInstruction{
			Instruction:  strings.ToUpper(instruction),
			Arguments:    args,
			LineNum:      lineNum,
			IsExecFormat: isExec,
		})
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("读取 Dockerfile 失败: %w", err)
	}

	return instructions, nil
}

// parseInstructionLine 解析单行指令，返回指令名、参数列表和是否为 exec 格式
// 支持 Shell 格式和 JSON 数组格式（Exec 格式）：
//   - Shell: RUN apt-get update → ("RUN", ["apt-get", "update"], false)
//   - Exec: RUN ["apt-get", "update"] → ("RUN", ["apt-get", "update"], true)
//   - Shell: CMD /bin/sh -c "echo hi" → ("CMD", ["/bin/sh", "-c", "echo hi"], false)
//   - Exec: CMD ["/bin/sh", "-c", "echo hi"] → ("CMD", ["/bin/sh", "-c", "echo hi"], true)
func parseInstructionLine(line string) (string, []string, bool, bool) {
	// 提取指令名（第一个空白前的 token）
	idx := strings.IndexAny(line, " \t")
	if idx == -1 {
		// 整行就是指令名（不太可能，但防御性处理）
		return line, nil, false, false
	}
	instruction := line[:idx]
	rest := strings.TrimSpace(line[idx:])

	// 检测 JSON 数组格式（以 [ 开头）
	if strings.HasPrefix(rest, "[") {
		var arr []string
		if err := json.Unmarshal([]byte(rest), &arr); err == nil {
			return instruction, arr, true, true
		}
		// JSON 解析失败，回退到 shell 格式
	}

	// Shell 格式：引号感知的分割
	args := shellSplit(rest)
	if len(args) == 0 {
		return instruction, nil, false, false
	}
	return instruction, args, false, true
}

// shellSplit 将 shell 格式的参数字符串分割为 token 列表
// 支持双引号和单引号包裹的参数（引号内的空格不分割）
// 对齐 Docker: ENV MY_VAR="hello world" → ["MY_VAR=hello world"]
func shellSplit(s string) []string {
	var tokens []string
	var current strings.Builder
	inDoubleQuote := false
	inSingleQuote := false

	for i := 0; i < len(s); i++ {
		ch := s[i]

		if inSingleQuote {
			if ch == '\'' {
				inSingleQuote = false
			} else {
				current.WriteByte(ch)
			}
			continue
		}

		if inDoubleQuote {
			if ch == '"' {
				inDoubleQuote = false
			} else if ch == '\\' && i+1 < len(s) {
				// 转义字符
				current.WriteByte(s[i+1])
				i++
			} else {
				current.WriteByte(ch)
			}
			continue
		}

		switch ch {
		case '"':
			inDoubleQuote = true
		case '\'':
			inSingleQuote = true
		case ' ', '\t':
			if current.Len() > 0 {
				tokens = append(tokens, current.String())
				current.Reset()
			}
		default:
			current.WriteByte(ch)
		}
	}

	if current.Len() > 0 {
		tokens = append(tokens, current.String())
	}

	return tokens
}

// ValidateDockerfile 验证 Dockerfile 指令
func ValidateDockerfile(instructions []DockerfileInstruction) error {
	if len(instructions) == 0 {
		return fmt.Errorf("Dockerfile 为空")
	}

	// 第一条指令必须是 FROM
	if instructions[0].Instruction != "FROM" {
		return fmt.Errorf("Dockerfile 必须以 FROM 指令开头")
	}

	// 验证每条指令
	for _, inst := range instructions {
		switch inst.Instruction {
		case "FROM":
			if len(inst.Arguments) < 1 {
				return fmt.Errorf("第 %d 行: FROM 需要指定镜像名", inst.LineNum)
			}
		case "RUN":
			if len(inst.Arguments) < 1 {
				return fmt.Errorf("第 %d 行: RUN 需要指定命令", inst.LineNum)
			}
		case "COPY":
			if len(inst.Arguments) < 2 {
				return fmt.Errorf("第 %d 行: COPY 需要指定源和目标", inst.LineNum)
			}
		case "CMD":
			if len(inst.Arguments) < 1 {
				return fmt.Errorf("第 %d 行: CMD 需要指定命令", inst.LineNum)
			}
		case "ENV":
			if len(inst.Arguments) < 1 {
				return fmt.Errorf("第 %d 行: ENV 需要指定变量", inst.LineNum)
			}
		case "WORKDIR":
			if len(inst.Arguments) < 1 {
				return fmt.Errorf("第 %d 行: WORKDIR 需要指定路径", inst.LineNum)
			}
		case "EXPOSE":
			if len(inst.Arguments) < 1 {
				return fmt.Errorf("第 %d 行: EXPOSE 需要指定端口", inst.LineNum)
			}
		default:
			fmt.Printf("  警告: 第 %d 行: 不支持的指令 %s，跳过\n", inst.LineNum, inst.Instruction)
		}
	}

	return nil
}

// Build 执行 Dockerfile 构建
func Build(config BuildConfig) (*BuildResult, error) {
	// 1. 解析 Dockerfile
	dockerfilePath := config.DockerfilePath
	if dockerfilePath == "" {
		dockerfilePath = filepath.Join(config.ContextDir, "Dockerfile")
	}

	instructions, err := ParseDockerfile(dockerfilePath)
	if err != nil {
		return nil, fmt.Errorf("解析 Dockerfile 失败: %w", err)
	}

	// 2. 验证
	if err := ValidateDockerfile(instructions); err != nil {
		return nil, fmt.Errorf("Dockerfile 验证失败: %w", err)
	}

	fmt.Printf("构建镜像 %s...\n", config.Tag)
	fmt.Printf("  Dockerfile: %s\n", dockerfilePath)
	fmt.Printf("  指令数量: %d\n", len(instructions))

	// 3. 逐条执行指令
	result := &BuildResult{}

	// 构建上下文：追踪当前状态
	buildCtx := &buildContext{
		svc:          config.Service,
		snap:         config.Service.Snapshotter(),
		contentStore: config.Service.ContentStore(),
		contextDir:   config.ContextDir,
		workDir:      "/",
		envVars:      make(map[string]string),
		imageName:    "",
	}

	for i, inst := range instructions {
		fmt.Printf("  Step %d/%d : %s %s\n", i+1, len(instructions), inst.Instruction, strings.Join(inst.Arguments, " "))

		switch inst.Instruction {
		case "FROM":
			err = buildCtx.handleFrom(inst)
		case "RUN":
			err = buildCtx.handleRun(inst)
		case "COPY":
			err = buildCtx.handleCopy(inst)
		case "CMD":
			err = buildCtx.handleCmd(inst)
		case "ENV":
			err = buildCtx.handleEnv(inst)
		case "WORKDIR":
			err = buildCtx.handleWorkdir(inst)
		case "EXPOSE":
			err = buildCtx.handleExpose(inst)
		default:
			continue // 跳过不支持的指令
		}

		if err != nil {
			// 对齐 Docker: 构建失败时清理所有已创建的中间层，避免留下孤儿快照
			buildCtx.cleanupLayers()
			return nil, fmt.Errorf("第 %d 行执行失败: %w", inst.LineNum, err)
		}
	}

	// 4. 生成最终镜像
	name, tag := utils.ParseImageTag(config.Tag)
	if name == "" {
		name = "unnamed"
	}
	if tag == "" {
		tag = "latest"
	}

	result.ImageName = name
	result.Tag = tag
	result.Layers = buildCtx.layers

	// 保存镜像元数据
	info, err := buildCtx.saveFinalImage(name, tag)
	if err != nil {
		fmt.Printf("  警告: 保存镜像元数据失败: %v\n", err)
	} else {
		result.ImageID = info.ImageID
		result.Info = info
	}

	fmt.Printf("Successfully built %s:%s\n", name, tag)
	return result, nil
}

// buildContext 构建上下文（跟踪构建状态）
type buildContext struct {
	svc            BuildService
	snap           snapshots.Snapshotter // 通过 Snapshotter 统一管理构建层
	contentStore   content.Store         // Content Store，用于 Differ 计算层差异
	contextDir     string
	workDir        string
	envVars        map[string]string //存储环境变量
	cmd            []string
	exposedPorts   []string
	imageName      string
	layers         []string // 记录已 Commit 的层 ID（cacheID）
	layerDigests   []string // 每层的真实 tar blob digest（sha256:...），由 Differ 计算
	currentLayerID string   // 当前最顶层的快照 ID，作为下一层的 parent
}

func (ctx *buildContext) handleFrom(inst DockerfileInstruction) error {
	ctx.imageName = inst.Arguments[0]

	if ctx.svc == nil {
		return fmt.Errorf("BuildService 未配置，无法解析基础镜像")
	}
	snapshotID, err := ctx.svc.ResolveImage(ctx.imageName)
	if err != nil {
		return fmt.Errorf("基础镜像 %s 不存在，请先 pull: %w", ctx.imageName, err)
	}

	// 设置当前层 ID，作为后续 RUN/COPY 的 parent
	// 注意：这里不需要获取 fs 目录路径，因为：
	// 1. Stat + diff.FSDir 只返回单层 fs/ 目录，不是完整 rootfs（多层镜像会缺失下层内容）
	// 2. 后续的 RUN/COPY 会通过 Snapshotter.Prepare(parent=snapshotID) 自动构建完整的 overlay lowerdir 链
	ctx.currentLayerID = snapshotID

	fmt.Printf("    → 基础镜像: %s (snapshot: %s)\n", ctx.imageName, snapshotID)
	return nil
}

func (ctx *buildContext) handleRun(inst DockerfileInstruction) error {
	// 对齐 Docker: 区分 Shell 格式和 Exec 格式
	//   Shell 格式: RUN apt-get update → /bin/sh -c "apt-get update"
	//   Exec 格式:  RUN ["apt-get", "update"] → 直接执行 apt-get update（不经过 shell）
	var execArgs []string
	var displayCmd string

	if inst.IsExecFormat {
		// Exec 格式：直接执行，不经过 shell
		// 对齐 Docker: RUN ["apt-get", "update"] 等价于直接 exec apt-get update
		displayCmd = strings.Join(inst.Arguments, " ")
		if ctx.workDir != "" && ctx.workDir != "/" {
			// Exec 格式不支持 cd，需要通过 chroot 的 working directory 机制
			// 回退到 /bin/sh -c "cd WORKDIR && exec args..."
			shellCmd := fmt.Sprintf("cd %s && exec %s", ctx.workDir, displayCmd)
			execArgs = []string{"chroot", ctx.mergedDirPlaceholder(), "/bin/sh", "-c", shellCmd}
		} else {
			// chroot + 直接执行命令列表
			chrootArgs := []string{"chroot", ctx.mergedDirPlaceholder()}
			chrootArgs = append(chrootArgs, inst.Arguments...)
			execArgs = chrootArgs
		}
	} else {
		// Shell 格式：通过 /bin/sh -c 执行
		cmd := strings.Join(inst.Arguments, " ")
		displayCmd = cmd
		fullCmd := cmd
		if ctx.workDir != "" && ctx.workDir != "/" {
			fullCmd = fmt.Sprintf("cd %s && %s", ctx.workDir, cmd)
		}
		execArgs = []string{"chroot", ctx.mergedDirPlaceholder(), "/bin/sh", "-c", fullCmd}
	}

	fmt.Printf("    → 执行: %s\n", displayCmd)

	// 生成唯一层 ID
	layerID := fmt.Sprintf("run-%d-%x", len(ctx.layers), time.Now().UnixNano())

	// 1. Prepare: 创建 Active 快照
	if ctx.snap == nil {
		return fmt.Errorf("Snapshotter 未配置，无法执行 RUN")
	}
	mounts, err := ctx.snap.Prepare(context.Background(), layerID, ctx.currentLayerID)
	if err != nil {
		return fmt.Errorf("Prepare 层快照失败: %w", err)
	}

	// 2. Mount: 使用 Snapshotter 返回的 mount 信息挂载 overlay
	// 对齐 containerd: 挂载点路径不依赖 Snapshotter 内部目录结构
	// 使用系统临时目录作为挂载点，与 Snapshotter 的存储目录解耦
	mergedDir, err := os.MkdirTemp("", "mini-docker-build-*")
	if err != nil {
		ctx.snap.Remove(context.Background(), layerID)
		return fmt.Errorf("创建临时挂载目录失败: %w", err)
	}
	// 记录 merged 目录路径，用于后续 umount 和清理
	// workDir 是实际文件操作的目标目录，overlay 时等于 mergedDir，bind 时等于 Source
	// mergedDir 保持为临时目录，确保 defer RemoveAll 只清理临时目录而非 Snapshotter 管理的目录
	workDir := mergedDir
	defer os.RemoveAll(mergedDir)

	isOverlayMount := false
	if len(mounts) > 0 && mounts[0].Type == "overlay" {
		isOverlayMount = true
		mountOpts := strings.Join(mounts[0].Options, ",")
		mountCmd := exec.Command("mount", "-t", "overlay", "overlay", "-o", mountOpts, mergedDir)
		if err := mountCmd.Run(); err != nil {
			ctx.snap.Remove(context.Background(), layerID)
			return fmt.Errorf("挂载 OverlayFS 失败: %w", err)
		}
	} else if len(mounts) > 0 && mounts[0].Type == "bind" {
		// bind 挂载：直接使用 Source 作为工作目录
		// 不重新赋值 mergedDir，避免 defer RemoveAll 删除 Snapshotter 管理的目录
		workDir = mounts[0].Source
	}

	// 注意：不创建 proc/sys/dev 目录，这些由容器运行时挂载
	// 在此处创建会把空目录持久化到镜像层（architecturally wrong）

	// 确保 WORKDIR 目录存在（对齐 Docker: WORKDIR 会自动创建目录）
	if ctx.workDir != "" && ctx.workDir != "/" {
		workDirPath := filepath.Join(workDir, ctx.workDir)
		if _, err := os.Stat(workDirPath); os.IsNotExist(err) {
			os.MkdirAll(workDirPath, 0755)
		}
	}

	// 3. 执行命令（在隔离的 namespace 中执行，并注入 ENV 环境变量）
	// 将 mergedDir 占位符替换为实际路径
	actualExecArgs := make([]string, len(execArgs))
	for i, a := range execArgs {
		if a == ctx.mergedDirPlaceholder() {
			actualExecArgs[i] = workDir
		} else {
			actualExecArgs[i] = a
		}
	}

	// 对齐 Docker: RUN 命令在隔离的 namespace 中执行
	// -m: mount namespace（防止 mount 泄漏到宿主机）
	// -u: UTS namespace（隔离 hostname）
	// -i: IPC namespace（隔离 System V IPC 和 POSIX 消息队列）
	// -p: PID namespace（隔离进程 ID）
	// -f: fork（unshare 不阻塞，子进程在新的 namespace 中运行）
	// 注意：不加 -n（network namespace），对齐 Docker 默认构建网络行为
	// Docker 构建默认网络模式为 default（即有网络），仅 docker build --network=none 时无网络
	// RUN apt-get update / pip install / curl 等命令需要网络才能正常工作
	unshareArgs := []string{"-m", "-u", "-i", "-p", "-f", "--"}
	unshareArgs = append(unshareArgs, actualExecArgs...)

	execCmd := exec.Command("unshare", unshareArgs...)
	execCmd.Stdout = os.Stdout
	execCmd.Stderr = os.Stderr
	// 注入 ENV 环境变量（对齐 Docker: RUN 继承所有 ENV）
	execCmd.Env = append(os.Environ(), ctx.envSlice()...)

	runErr := execCmd.Run()

	// 4. Umount（无论成功失败都必须先卸载，否则 Remove 会因 "device busy" 失败）
	if isOverlayMount {
		if err := exec.Command("umount", mergedDir).Run(); err != nil {
			// umount 失败时尝试懒卸载
			if lazyErr := exec.Command("umount", "-l", mergedDir).Run(); lazyErr != nil {
				log.Printf("警告: 卸载 %s 失败 (普通: %v, 懒卸载: %v)，跳过目录清理防止数据损坏",
					mergedDir, err, lazyErr)
				return fmt.Errorf("卸载 OverlayFS 失败，请手动执行 umount %s", mergedDir)
			}
		}
	}

	if runErr != nil {
		ctx.snap.Remove(context.Background(), layerID)
		return fmt.Errorf("执行命令失败: %w", runErr)
	}

	// 5. Commit: 提交为 Committed 快照
	if err := ctx.snap.Commit(context.Background(), layerID, layerID); err != nil {
		// Commit 失败意味着该层不可用，必须清理并报错
		// 不能静默追加到 layers，否则后续层会基于损坏的层构建
		ctx.snap.Remove(context.Background(), layerID)
		return fmt.Errorf("Commit 层 %s 失败: %w", layerID, err)
	}

	// 6. 使用 Differ 计算层差异（对齐 containerd: 每层都有真实的 tar blob digest）
	layerDigest, err := ctx.computeLayerDigest(layerID)
	if err != nil {
		// Differ 失败不阻断构建，使用 layerID 作为降级 digest
		fmt.Printf("    警告: 计算层差异失败: %v\n", err)
		layerDigest = layerID
	}

	// 更新上下文
	ctx.layers = append(ctx.layers, layerID)
	ctx.layerDigests = append(ctx.layerDigests, layerDigest)
	ctx.currentLayerID = layerID

	fmt.Printf("    → 层 %s 已生成\n", layerID)
	return nil
}

// mergedDirPlaceholder 返回 mergedDir 的占位符，用于延迟替换
// （因为 mergedDir 在 Prepare+Mount 之后才确定）
func (ctx *buildContext) mergedDirPlaceholder() string {
	return "<<MERGED_DIR>>"
}

func (ctx *buildContext) handleCopy(inst DockerfileInstruction) error {
	// 对齐 Docker: 过滤不支持的 COPY 标志（--chown, --chmod 等）
	// 这些标志以 -- 开头，不是源路径，需在处理源文件前移除
	var filteredArgs []string
	for _, arg := range inst.Arguments {
		if strings.HasPrefix(arg, "--") {
			fmt.Printf("    警告: COPY 不支持标志 %s，已忽略\n", arg)
			continue
		}
		filteredArgs = append(filteredArgs, arg)
	}

	if len(filteredArgs) < 2 {
		return fmt.Errorf("COPY 需要至少两个参数（源和目标）")
	}

	// 对齐 Docker: 最后一个参数是目标路径，前面所有参数都是源路径
	dst := filteredArgs[len(filteredArgs)-1]
	srcs := filteredArgs[:len(filteredArgs)-1]

	// 对齐 Docker: COPY 目标路径如果不以 / 开头，则相对于 WORKDIR
	if !filepath.IsAbs(dst) && ctx.workDir != "" && ctx.workDir != "/" {
		dst = filepath.Join(ctx.workDir, dst)
	}

	// 展开通配符并收集所有源文件路径
	var resolvedSrcs []string
	for _, src := range srcs {
		fullSrc := filepath.Join(ctx.contextDir, src)
		// 对齐 Docker: COPY 支持通配符（filepath.Glob 展开）
		matches, err := filepath.Glob(fullSrc)
		if err != nil || len(matches) == 0 {
			// Glob 不匹配时，当作普通路径处理（检查是否存在）
			if _, statErr := os.Stat(fullSrc); statErr != nil {
				return fmt.Errorf("源文件 %s 不存在", src)
			}
			resolvedSrcs = append(resolvedSrcs, fullSrc)
		} else {
			resolvedSrcs = append(resolvedSrcs, matches...)
		}
	}

	fmt.Printf("    → 复制: %s → %s\n", strings.Join(srcs, ", "), dst)

	// 生成唯一层 ID
	layerID := fmt.Sprintf("copy-%d-%x", len(ctx.layers), time.Now().UnixNano())

	// 1. Prepare: 创建 Active 快照
	if ctx.snap == nil {
		return fmt.Errorf("Snapshotter 未配置，无法执行 COPY")
	}
	mounts, err := ctx.snap.Prepare(context.Background(), layerID, ctx.currentLayerID)
	if err != nil {
		return fmt.Errorf("Prepare 层快照失败: %w", err)
	}

	// 2. Mount: 挂载 overlay
	// 对齐 containerd: 挂载点路径不依赖 Snapshotter 内部目录结构
	mergedDir, err := os.MkdirTemp("", "mini-docker-build-*")
	if err != nil {
		ctx.snap.Remove(context.Background(), layerID)
		return fmt.Errorf("创建临时挂载目录失败: %w", err)
	}
	// workDir 是实际文件操作的目标目录，overlay 时等于 mergedDir，bind 时等于 Source
	// mergedDir 保持为临时目录，确保 defer RemoveAll 只清理临时目录而非 Snapshotter 管理的目录
	workDir := mergedDir
	defer os.RemoveAll(mergedDir)

	isOverlayMount := false
	if len(mounts) > 0 && mounts[0].Type == "overlay" {
		isOverlayMount = true
		mountOpts := strings.Join(mounts[0].Options, ",")
		mountCmd := exec.Command("mount", "-t", "overlay", "overlay", "-o", mountOpts, mergedDir)
		if err := mountCmd.Run(); err != nil {
			ctx.snap.Remove(context.Background(), layerID)
			return fmt.Errorf("挂载 OverlayFS 失败: %w", err)
		}
	} else if len(mounts) > 0 && mounts[0].Type == "bind" {
		// bind 挂载：直接使用 Source 作为工作目录
		// 不重新赋值 mergedDir，避免 defer RemoveAll 删除 Snapshotter 管理的目录
		workDir = mounts[0].Source
	}

	// 确保 WORKDIR 目录存在（对齐 Docker: WORKDIR 会自动创建目录）
	if ctx.workDir != "" && ctx.workDir != "/" {
		workDirPath := filepath.Join(workDir, ctx.workDir)
		if _, err := os.Stat(workDirPath); os.IsNotExist(err) {
			os.MkdirAll(workDirPath, 0755)
		}
	}

	// 3. 复制文件到工作目录
	targetPath := filepath.Join(workDir, dst)
	if err := os.MkdirAll(filepath.Dir(targetPath), 0755); err != nil {
		if isOverlayMount {
			exec.Command("umount", mergedDir).Run()
		}
		ctx.snap.Remove(context.Background(), layerID)
		return fmt.Errorf("创建目标目录失败: %w", err)
	}

	// 对齐 Docker: 多源复制时，目标必须是目录（自动以 / 结尾）
	if len(resolvedSrcs) > 1 {
		os.MkdirAll(targetPath, 0755)
	}

	// 复制所有源文件/目录，收集复制错误
	var copyErrors []string
	for _, src := range resolvedSrcs {
		srcInfo, err := os.Stat(src)
		if err != nil {
			copyErrors = append(copyErrors, fmt.Sprintf("stat %s: %v", src, err))
			continue
		}
		if srcInfo.IsDir() {
			// 目录复制
			walkErr := filepath.Walk(src, func(path string, info os.FileInfo, walkErr error) error {
				if walkErr != nil {
					copyErrors = append(copyErrors, fmt.Sprintf("walk %s: %v", path, walkErr))
					return nil
				}
				relPath, _ := filepath.Rel(src, path)
				dstPath := filepath.Join(targetPath, relPath)
				if info.IsDir() {
					if err := os.MkdirAll(dstPath, info.Mode()); err != nil {
						copyErrors = append(copyErrors, fmt.Sprintf("mkdir %s: %v", dstPath, err))
					}
				} else {
					if err := utils.CopyFile(path, dstPath); err != nil {
						copyErrors = append(copyErrors, fmt.Sprintf("copy %s → %s: %v", path, dstPath, err))
					}
				}
				return nil
			})
			if walkErr != nil {
				copyErrors = append(copyErrors, fmt.Sprintf("walk %s: %v", src, walkErr))
			}
		} else {
			if err := utils.CopyFile(src, targetPath); err != nil {
				copyErrors = append(copyErrors, fmt.Sprintf("copy %s → %s: %v", src, targetPath, err))
			}
		}
	}

	// 报告复制错误
	if len(copyErrors) > 0 {
		fmt.Printf("    警告: COPY 过程中有 %d 个文件复制失败:\n", len(copyErrors))
		for _, e := range copyErrors {
			fmt.Printf("      - %s\n", e)
		}
	}

	// 4. Umount
	if isOverlayMount {
		if err := exec.Command("umount", mergedDir).Run(); err != nil {
			// umount 失败时尝试懒卸载
			if lazyErr := exec.Command("umount", "-l", mergedDir).Run(); lazyErr != nil {
				log.Printf("警告: 卸载 %s 失败 (普通: %v, 懒卸载: %v)，跳过目录清理防止数据损坏",
					mergedDir, err, lazyErr)
				return fmt.Errorf("卸载 OverlayFS 失败，请手动执行 umount %s", mergedDir)
			}
		}
	}

	// 5. Commit: 提交为 Committed 快照
	if err := ctx.snap.Commit(context.Background(), layerID, layerID); err != nil {
		// Commit 失败意味着该层不可用，必须清理并报错
		ctx.snap.Remove(context.Background(), layerID)
		return fmt.Errorf("Commit 层 %s 失败: %w", layerID, err)
	}

	// 6. 使用 Differ 计算层差异（对齐 containerd: 每层都有真实的 tar blob digest）
	layerDigest, err := ctx.computeLayerDigest(layerID)
	if err != nil {
		// Differ 失败不阻断构建，使用 layerID 作为降级 digest
		fmt.Printf("    警告: 计算层差异失败: %v\n", err)
		layerDigest = layerID
	}

	// 更新上下文
	ctx.layers = append(ctx.layers, layerID)
	ctx.layerDigests = append(ctx.layerDigests, layerDigest)
	ctx.currentLayerID = layerID

	fmt.Printf("    → 层 %s 已生成\n", layerID)
	return nil
}

func (ctx *buildContext) handleCmd(inst DockerfileInstruction) error {
	// 对齐 Docker: CMD 区分两种格式
	//   Exec 格式:  CMD ["/bin/cat", "/app/hello.txt"] → 直接执行
	//   Shell 格式: CMD /bin/cat /app/hello.txt → 以 /bin/sh -c 包裹
	// JSON 数组格式由 parseInstructionLine 解析后已经是 []string，直接使用
	// Shell 格式在运行时由 daemon 包裹为 ["/bin/sh", "-c", "joined command"]
	if len(inst.Arguments) == 0 {
		return fmt.Errorf("CMD 需要指定命令")
	}

	if inst.IsExecFormat {
		// Exec 格式：直接使用参数列表，运行时不经过 shell
		ctx.cmd = inst.Arguments
	} else {
		// Shell 格式：记录为 ["/bin/sh", "-c", "joined command"]
		// 对齐 Docker: Shell 格式的 CMD 在运行时以 /bin/sh -c 包裹执行
		ctx.cmd = []string{"/bin/sh", "-c", strings.Join(inst.Arguments, " ")}
	}

	fmt.Printf("    → 默认命令: %s\n", strings.Join(ctx.cmd, " "))
	return nil
}

func (ctx *buildContext) handleEnv(inst DockerfileInstruction) error {
	// 对齐 Docker: ENV 支持两种格式：
	//   ENV KEY=VALUE          → 单个键值对，VALUE 可以包含空格（由 shellSplit 正确处理）
	//   ENV KEY1=VAL1 KEY2=VAL2 → 多个键值对
	for _, arg := range inst.Arguments {
		parts := strings.SplitN(arg, "=", 2)
		if len(parts) == 2 {
			ctx.envVars[parts[0]] = parts[1]
		} else if len(parts) == 1 && parts[0] != "" {
			// ENV KEY VALUE 格式（Docker 旧式语法）：下一个 token 是值
			// 但由于 shellSplit 已经把 KEY 和 VALUE 分开了，这种情况无法直接处理
			// 这里只记录 key，值为空（对齐 Docker: ENV MY_VAR 等价于 ENV MY_VAR=""）
			ctx.envVars[parts[0]] = ""
		}
	}
	fmt.Printf("    → 环境变量: %v\n", ctx.envVars)
	return nil
}

func (ctx *buildContext) handleWorkdir(inst DockerfileInstruction) error {
	ctx.workDir = inst.Arguments[0]
	fmt.Printf("    → 工作目录: %s\n", ctx.workDir)
	return nil
}

func (ctx *buildContext) handleExpose(inst DockerfileInstruction) error {
	ctx.exposedPorts = append(ctx.exposedPorts, inst.Arguments...)
	fmt.Printf("    → 暴露端口: %s\n", strings.Join(inst.Arguments, " "))
	return nil
}

// computeLayerDigest 使用 Differ 计算已 Commit 层的真实 tar blob digest
// 对齐 containerd: 构建器每层都通过 Differ.Diff() 计算差异，
// 生成 tar.gz blob 写入 Content Store，返回真实的 sha256 digest
func (ctx *buildContext) computeLayerDigest(layerID string) (string, error) {
	if ctx.contentStore == nil || ctx.snap == nil {
		return "", fmt.Errorf("ContentStore 或 Snapshotter 未配置")
	}

	// 获取当前层的 Mount 信息（Committed 快照，包含 lowerdir）
	upperMounts, err := ctx.snap.Mounts(context.Background(), layerID)
	if err != nil {
		return "", fmt.Errorf("获取层挂载信息失败: %w", err)
	}

	// 获取父层的 Mount 信息（作为 lower）
	info, err := ctx.snap.Stat(context.Background(), layerID)
	if err != nil {
		return "", fmt.Errorf("获取层快照信息失败: %w", err)
	}

	var lowerMounts []snapshots.Mount
	if info.Parent != "" {
		lowerMounts, err = ctx.snap.Mounts(context.Background(), info.Parent)
		if err != nil {
			return "", fmt.Errorf("获取父层挂载信息失败: %w", err)
		}
	}

	// 使用注入的 Differ 计算差异（对齐 containerd: diff 服务可插拔）
	var differ diff.Differ
	if ctx.svc != nil {
		differ = ctx.svc.Differ()
	}
	if differ == nil {
		return "", fmt.Errorf("Differ 未配置")
	}
	result, err := differ.Diff(context.Background(), lowerMounts, upperMounts, ctx.contentStore)
	if err != nil {
		return "", fmt.Errorf("Differ.Diff 失败: %w", err)
	}

	return result.Digest, nil
}

// envSlice 将 envVars map 转换为 "KEY=VALUE" 切片，用于 exec.Cmd.Env
func (ctx *buildContext) envSlice() []string {
	var env []string
	for k, v := range ctx.envVars {
		env = append(env, k+"="+v)
	}
	return env
}

// cleanupLayers 清理构建过程中已创建的中间层快照
// 对齐 Docker: 构建失败时删除所有中间层，避免留下孤儿快照
func (ctx *buildContext) cleanupLayers() {
	if ctx.snap == nil {
		return
	}
	for _, layerID := range ctx.layers {
		if err := ctx.snap.Remove(context.Background(), layerID); err != nil {
			fmt.Printf("    警告: 清理中间层 %s 失败: %v\n", layerID, err)
		}
	}
}

func (ctx *buildContext) saveFinalImage(name, tag string) (*metadata.Image, error) {
	fmt.Printf("    → 保存镜像 %s:%s\n", name, tag)

	// TopLayerSnapshotID 是最后一层的 ID（已是 cacheID/snapshot key）
	topLayerSnapshotID := ctx.currentLayerID

	// 对齐 containerd: LayerDigests 使用 Differ 计算的真实 tar blob digest（sha256:...）
	// 每层在 Commit 后已通过 computeLayerDigest 计算并存储在 layerDigests 中
	// 如果 Differ 失败，降级使用 layerID 作为 digest
	layerDigests := ctx.layerDigests

	// 计算镜像大小：所有层的 Usage 之和（对齐 Docker: 镜像大小 = 各层大小总和）
	// 对齐 containerd: 通过 snap.Usage() 获取快照资源使用量，而非手动拼接路径
	var totalSize int64
	if ctx.snap != nil {
		for _, layerID := range ctx.layers {
			usage, err := ctx.snap.Usage(context.Background(), layerID)
			if err != nil {
				continue
			}
			totalSize += usage.Size
		}
	}
	imageSize := formatImageSize(totalSize)

	// 填充 Config：将构建期间收集的 CMD/ENV/WORKDIR/EXPOSE 持久化到镜像元数据
	// 对齐 Docker: 构建出的镜像在 docker run 时能继承这些默认值
	var envSlice []string
	for k, v := range ctx.envVars {
		envSlice = append(envSlice, k+"="+v)
	}

	// 计算镜像 ID：基于镜像内容确定性计算（对齐 Docker: SHA256(config JSON)）
	imageID := computeBuildImageID(name, tag, layerDigests, ctx.cmd, envSlice, ctx.workDir)

	info := &metadata.Image{
		Name:               name,
		Tag:                tag,
		ImageID:            imageID,
		CreatedAt:          time.Now().Format("2006-01-02 15:04:05"),
		TopLayerSnapshotID: topLayerSnapshotID,
		LayerDigests:       layerDigests,
		Config: metadata.ImageConfig{
			Cmd:          ctx.cmd,
			Env:          envSlice,
			WorkingDir:   ctx.workDir,
			ExposedPorts: ctx.exposedPorts,
		},
		Size: imageSize,
	}

	if ctx.svc == nil {
		fmt.Printf("    警告: BuildService 未配置，跳过元数据注册\n")
		return info, nil
	}
	if err := ctx.svc.RegisterImage(info); err != nil {
		return nil, fmt.Errorf("注册镜像元数据失败: %w", err)
	}

	return info, nil
}

// calculateDirSize 计算目录总大小（字节数）
func calculateDirSize(dir string) int64 {
	var size int64
	filepath.Walk(dir, func(_ string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		size += info.Size()
		return nil
	})
	return size
}

// formatImageSize 将字节数格式化为人类可读字符串
// 对齐 Docker: 使用 B/KB/MB/GB 单位，与 docker images 的 SIZE 列格式一致
func formatImageSize(sizeBytes int64) string {
	const (
		KB = 1024
		MB = 1024 * KB
		GB = 1024 * MB
	)
	switch {
	case sizeBytes >= GB:
		return fmt.Sprintf("%.1fGB", float64(sizeBytes)/float64(GB))
	case sizeBytes >= MB:
		return fmt.Sprintf("%.1fMB", float64(sizeBytes)/float64(MB))
	case sizeBytes >= KB:
		return fmt.Sprintf("%.1fKB", float64(sizeBytes)/float64(KB))
	default:
		return fmt.Sprintf("%dB", sizeBytes)
	}
}

// computeBuildImageID 基于镜像内容确定性计算镜像 ID
// 对齐 Docker: imageID = SHA256(config JSON)，相同内容产生相同 ID
func computeBuildImageID(name, tag string, layerDigests, cmd, env []string, workDir string) string {
	h := sha256.New()
	h.Write([]byte(name + ":" + tag))
	for _, d := range layerDigests {
		h.Write([]byte(d))
	}
	for _, c := range cmd {
		h.Write([]byte(c))
	}
	for _, e := range env {
		h.Write([]byte(e))
	}
	h.Write([]byte(workDir))
	return fmt.Sprintf("%x", h.Sum(nil))
}
