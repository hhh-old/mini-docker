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
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"mini-docker/constants"
	"mini-docker/utils"
)

// BuildConfig 构建配置
type BuildConfig struct {
	DockerfilePath string // Dockerfile 路径
	ContextDir     string // 构建上下文目录
	Tag            string // 镜像标签 (name:tag)
}

// BuildResult 构建结果
type BuildResult struct {
	ImageName string   // 生成的镜像名
	Tag       string   // 标签
	Layers    []string // 生成的层 ID 列表
	ImageID   string   // 最终镜像 ID
}

// DockerfileInstruction Dockerfile 指令
type DockerfileInstruction struct {
	Instruction string   // 指令名（FROM, RUN, COPY 等）
	Arguments   []string // 参数
	LineNum     int      // 行号（用于错误报告）
	Flags       []string // 标志参数（如 --chown）
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

	for scanner.Scan() {
		lineNum++
		line := strings.TrimSpace(scanner.Text())

		// 跳过空行和注释
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		// 解析指令
		parts := strings.Fields(line)
		if len(parts) == 0 {
			continue
		}

		instruction := strings.ToUpper(parts[0])
		args := parts[1:]

		// 处理多行指令（反斜杠续行）
		for strings.HasSuffix(line, "\\") && scanner.Scan() {
			lineNum++
			nextLine := strings.TrimSpace(scanner.Text())
			args = append(args, strings.Fields(nextLine)...)
			line = nextLine
		}

		instructions = append(instructions, DockerfileInstruction{
			Instruction: instruction,
			Arguments:   args,
			LineNum:     lineNum,
		})
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("读取 Dockerfile 失败: %w", err)
	}

	return instructions, nil
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
		contextDir: config.ContextDir,
		workDir:    "/",
		envVars:    make(map[string]string),
		imageName:  "",
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
			return nil, fmt.Errorf("第 %d 行执行失败: %w", inst.LineNum, err)
		}
	}

	// 4. 生成最终镜像
	name, tag := parseImageTag(config.Tag)
	if name == "" {
		name = "unnamed"
	}
	if tag == "" {
		tag = "latest"
	}

	result.ImageName = name
	result.Tag = tag
	result.Layers = buildCtx.layers
	result.ImageID = fmt.Sprintf("%x", len(buildCtx.layers))

	// 保存镜像元数据
	if err := buildCtx.saveFinalImage(name, tag); err != nil {
		fmt.Printf("  警告: 保存镜像元数据失败: %v\n", err)
	}

	fmt.Printf("Successfully built %s:%s\n", name, tag)
	return result, nil
}

// buildContext 构建上下文（跟踪构建状态）
type buildContext struct {
	contextDir   string            // 构建上下文目录
	workDir      string            // 当前工作目录
	envVars      map[string]string // 环境变量
	cmd          []string          // 默认命令
	exposedPorts []string          // 暴露端口
	imageName    string            // 基础镜像名
	layers       []string          // 已生成的层 ID
	rootfsPath   string            // 当前 rootfs 路径
}

func (ctx *buildContext) handleFrom(inst DockerfileInstruction) error {
	ctx.imageName = inst.Arguments[0]
	ctx.rootfsPath = filepath.Join(constants.ImageStoreDir, ctx.imageName, "rootfs")

	// 检查基础镜像是否存在
	if _, err := os.Stat(ctx.rootfsPath); os.IsNotExist(err) {
		return fmt.Errorf("基础镜像 %s 不存在，请先 pull", ctx.imageName)
	}

	fmt.Printf("    → 基础镜像: %s\n", ctx.imageName)
	return nil
}

func (ctx *buildContext) handleRun(inst DockerfileInstruction) error {
	cmd := strings.Join(inst.Arguments, " ")

	// 在临时 overlay 中执行命令
	// 简化实现：直接在宿主机执行（需要 chroot 或 namespace 隔离）
	// 教学版本：直接执行并记录
	fmt.Printf("    → 执行: %s\n", cmd)

	// 实际 Docker 会创建临时容器执行命令
	// 这里简化为提示用户
	fmt.Printf("    提示: RUN 指令需要创建临时容器执行，当前为教学简化版\n")
	fmt.Printf("    实际执行命令: %s\n", cmd)

	ctx.layers = append(ctx.layers, fmt.Sprintf("run-%d", len(ctx.layers)))
	return nil
}

func (ctx *buildContext) handleCopy(inst DockerfileInstruction) error {
	if len(inst.Arguments) < 2 {
		return fmt.Errorf("COPY 需要至少两个参数")
	}

	src := filepath.Join(ctx.contextDir, inst.Arguments[0])
	dst := inst.Arguments[1]

	fmt.Printf("    → 复制: %s → %s\n", src, dst)

	// 检查源文件是否存在
	if _, err := os.Stat(src); os.IsNotExist(err) {
		return fmt.Errorf("源文件 %s 不存在", src)
	}

	// 简化实现：直接复制到 rootfs
	targetPath := filepath.Join(ctx.rootfsPath, dst)
	if err := os.MkdirAll(filepath.Dir(targetPath), 0755); err != nil {
		return fmt.Errorf("创建目标目录失败: %w", err)
	}

	// 复制文件或目录
	srcInfo, _ := os.Stat(src)
	if srcInfo.IsDir() {
		// 目录复制（简化版）
		filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return nil
			}
			relPath, _ := filepath.Rel(src, path)
			dstPath := filepath.Join(targetPath, relPath)
			if info.IsDir() {
				os.MkdirAll(dstPath, info.Mode())
			} else {
				utils.CopyFile(path, dstPath)
			}
			return nil
		})
	} else {
		utils.CopyFile(src, targetPath)
	}

	ctx.layers = append(ctx.layers, fmt.Sprintf("copy-%d", len(ctx.layers)))
	return nil
}

func (ctx *buildContext) handleCmd(inst DockerfileInstruction) error {
	ctx.cmd = inst.Arguments
	fmt.Printf("    → 默认命令: %s\n", strings.Join(inst.Arguments, " "))
	return nil
}

func (ctx *buildContext) handleEnv(inst DockerfileInstruction) error {
	for _, arg := range inst.Arguments {
		parts := strings.SplitN(arg, "=", 2)
		if len(parts) == 2 {
			ctx.envVars[parts[0]] = parts[1]
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

func (ctx *buildContext) saveFinalImage(name, tag string) error {
	// 简化实现：保存为传统镜像格式（后续可切换到分层格式）
	fmt.Printf("    → 保存镜像 %s:%s\n", name, tag)
	return nil
}

// ---- 辅助函数 ----

func parseImageTag(tag string) (string, string) {
	parts := strings.SplitN(tag, ":", 2)
	if len(parts) == 1 {
		return parts[0], "latest"
	}
	return parts[0], parts[1]
}
