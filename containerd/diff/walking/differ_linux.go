//go:build linux

package walking

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/unix"

	"mini-docker/containerd/content"
	"mini-docker/containerd/diff"
	"mini-docker/containerd/snapshots"
)

// Diff 计算两个快照之间的文件系统差异，生成 tar.gz blob 写入 Content Store
//
// 对齐 containerd: 接收 []snapshots.Mount 而非原始目录路径。
// 从 Mount 对象中提取 upperdir 和 lowerdir，然后计算差异。
//
// 流程：
//  1. 从 upper mounts 提取 upperdir（Active 快照的可写层目录）
//  2. 从 lower mounts 提取 lowerdir（父快照的合并视图目录列表）
//  3. 遍历 upper 目录，与 lower 目录列表比较，生成差异 tar 条目
//  4. 处理 overlay whiteout 字符设备（转换为 .wh.<name> 条目）
//  5. 处理 opaque xattr（转换为 .wh..wh..opq 条目）
//  6. 将 tar 数据 gzip 压缩后写入 Content Store
//  7. 同时计算压缩 digest (Digest) 和未压缩 digest (DiffID)
//  8. 提交 blob 并返回 DiffResult
func (d *LayerDiffer) Diff(ctx context.Context, lower, upper []snapshots.Mount, contentStore content.Store, opts ...diff.DiffOpt) (diff.DiffResult, error) {
	// 1. 应用选项
	cfg := diff.DiffConfig{
		MediaType: "application/vnd.oci.image.layer.v1.tar+gzip",
	}
	for _, opt := range opts {
		if err := opt(&cfg); err != nil {
			return diff.DiffResult{}, fmt.Errorf("应用差异选项失败: %w", err)
		}
	}

	// 2. 从 Mount 对象提取目录路径
	// upper: Active 快照的可写层（upperdir）
	upperDir := diff.UpperDir(upper)
	if upperDir == "" {
		return diff.DiffResult{}, fmt.Errorf("无法从 upper mounts 中提取 upperdir: %+v", upper)
	}

	// lower: 父快照的合并视图（lowerdir 列表）
	// 可为空（表示从空目录开始，如基础层）
	// lowerdir 顺序与 overlayfs 一致：越靠前越上层。
	lowerDirs := diff.LowerDirs(lower)

	// 3. 创建 Content Store 写入器
	writer, err := contentStore.Writer(ctx, "", 0, cfg.MediaType)
	if err != nil {
		return diff.DiffResult{}, fmt.Errorf("创建内容写入器失败: %w", err)
	}
	defer writer.Close()

	// 4. 创建双摘要计算管道
	//    tar 数据 → teeReader → gzipWriter → contentWriter (计算压缩 digest)
	//                ↓
	//           diffIDHasher (计算未压缩 DiffID)
	diffIDHasher := sha256.New()
	compressedHasher := sha256.New()

	// gzip 压缩写入器，同时写入 contentWriter 和 compressedHasher
	// 使用 io.MultiWriter 将压缩数据同时写入 contentWriter 和 compressedHasher
	multiWriter := io.MultiWriter(writer, compressedHasher)
	gzipWriter := gzip.NewWriter(multiWriter)
	defer gzipWriter.Close()

	// tar 写入器，输出到 gzipWriter
	// 使用 io.MultiWriter 将未压缩 tar 数据同时写入 gzipWriter 和 diffIDHasher
	tarMultiWriter := io.MultiWriter(gzipWriter, diffIDHasher)
	tarWriter := tar.NewWriter(tarMultiWriter)

	// 5. 遍历差异，写入 tar 条目
	if err := walkDiff(tarWriter, lowerDirs, upperDir); err != nil {
		return diff.DiffResult{}, fmt.Errorf("遍历差异失败: %w", err)
	}

	// 6. 关闭 tar 和 gzip，确保所有数据刷出
	if err := tarWriter.Close(); err != nil {
		return diff.DiffResult{}, fmt.Errorf("关闭 tar 写入器失败: %w", err)
	}
	if err := gzipWriter.Close(); err != nil {
		return diff.DiffResult{}, fmt.Errorf("关闭 gzip 写入器失败: %w", err)
	}

	// 7. 计算摘要
	diffID := fmt.Sprintf("sha256:%x", diffIDHasher.Sum(nil))
	compressedDigest := fmt.Sprintf("sha256:%x", compressedHasher.Sum(nil))

	// 8. 获取写入大小
	size, err := writer.Status()
	if err != nil {
		return diff.DiffResult{}, fmt.Errorf("获取写入状态失败: %w", err)
	}

	// 9. 提交到 Content Store
	if err := writer.Commit(ctx, compressedDigest); err != nil {
		return diff.DiffResult{}, fmt.Errorf("提交内容失败: %w", err)
	}

	// 10. 返回结果
	return diff.DiffResult{
		Digest:    compressedDigest,
		DiffID:    diffID,
		Size:      size,
		MediaType: cfg.MediaType,
		Labels:    cfg.Labels,
	}, nil
}

// walkDiff 遍历 upper 目录并与 lower 目录列表比较，将差异条目写入 tar
//
// 对齐 containerd: lowerDirs 是从 Mount 对象的 lowerdir 解析出的目录列表，
// 包含所有祖先层的 fs/ 目录（最近父层在前，最远祖先在后）。
// 这与 overlay 的 lowerdir 语义一致：上层覆盖下层。
//
// 算法：
//  1. 遍历 upper 目录中的每个条目：
//     - 如果是 whiteout 字符设备 (0,0)：写入 .wh.<name> 条目
//     - 如果目录有 opaque xattr：写入 .wh..wh..opq 条目，然后添加该目录下所有内容
//     - 如果在 lower 中不存在：添加到 tar（新增文件）
//     - 如果在 lower 中存在但内容不同：添加到 tar（修改文件）
//     - 如果在 lower 中存在且相同：跳过
//  2. 遍历 lower 目录中不在 upper 中存在的条目：
//     - 写入 .wh.<name> 条目（删除文件）
func walkDiff(tw *tar.Writer, lowerDirs []string, upperDir string) error {
	// 收集 lower 目录中的条目名称集合（用于检测删除）
	// 对齐 overlay 语义：上层覆盖下层，同一文件只取最上层版本
	lowerEntries := make(map[string]bool)
	if len(lowerDirs) > 0 {
		// 从最远祖先到最近父遍历，后遍历的覆盖先遍历的
		// 但这里只需要收集"存在"的文件名集合，不需要合并内容
		// 所以遍历顺序不影响结果
		for _, lowerDir := range lowerDirs {
			if err := filepath.Walk(lowerDir, func(path string, info os.FileInfo, err error) error {
				if err != nil {
					return nil
				}
				rel, err := filepath.Rel(lowerDir, path)
				if err != nil || rel == "." {
					return nil
				}
				lowerEntries[rel] = true
				return nil
			}); err != nil {
				return fmt.Errorf("遍历 lower 目录 %s 失败: %w", lowerDir, err)
			}
		}
	}

	// 遍历 upper 目录，构建差异条目
	upperEntries := make(map[string]bool)
	if err := filepath.Walk(upperDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}

		rel, err := filepath.Rel(upperDir, path)
		if err != nil || rel == "." {
			return nil
		}
		upperEntries[rel] = true

		// 检查是否是 overlay whiteout 字符设备 (major=0, minor=0)
		if info.Mode()&os.ModeCharDevice != 0 {
			if stat, ok := info.Sys().(*syscall.Stat_t); ok {
				if stat.Rdev == 0 {
					// 这是 overlay whiteout 字符设备
					// 在 tar 中生成 .wh.<name> 条目
					dir := filepath.Dir(rel)
					base := filepath.Base(rel)
					whiteoutName := filepath.Join(dir, ".wh."+base)
					if err := tw.WriteHeader(&tar.Header{
						Typeflag: tar.TypeReg,
						Name:     whiteoutName,
						Size:     0,
						Mode:     0644,
					}); err != nil {
						return fmt.Errorf("写入 whiteout 条目失败: %w", err)
					}
					return nil
				}
			}
		}

		// 检查目录是否有 opaque xattr
		if info.IsDir() {
			opaqueBuf := make([]byte, 1)
			n, err := unix.Getxattr(path, "trusted.overlay.opaque", opaqueBuf)
			if err == nil && n > 0 && string(opaqueBuf[:n]) == "y" {
				// 写入 .wh..wh..opq 条目
				opqName := filepath.Join(rel, ".wh..wh..opq")
				if err := tw.WriteHeader(&tar.Header{
					Typeflag: tar.TypeReg,
					Name:     opqName,
					Size:     0,
					Mode:     0644,
				}); err != nil {
					return fmt.Errorf("写入 opaque whiteout 条目失败: %w", err)
				}
				// opaque 目录需要添加该目录下所有内容
				// 继续正常遍历，下面的逻辑会处理
			}
		}

		// 判断与 lower 的差异
		if len(lowerDirs) == 0 {
			// 无 lower 层，所有 upper 内容都是新增
			return writeTarEntry(tw, path, rel, info)
		}

		// 在 lower 目录列表中查找对应文件（overlay 语义：上层覆盖下层）
		lowerPath, lowerInfo, found := findInLower(rel, lowerDirs)
		if !found {
			// lower 中不存在，新增条目
			return writeTarEntry(tw, path, rel, info)
		}

		// 两者都存在，判断是否相同
		same, err := isSameEntry(path, info, lowerPath, lowerInfo)
		if err != nil {
			return fmt.Errorf("比较条目失败 (%s): %w", rel, err)
		}
		if same {
			// 相同，跳过
			if info.IsDir() {
				// 目录即使相同也需要检查子条目，所以不跳过遍历
				// 但不写入 tar 条目
				return nil
			}
			return nil
		}

		// 有差异，写入 tar
		return writeTarEntry(tw, path, rel, info)
	}); err != nil {
		return fmt.Errorf("遍历 upper 目录失败: %w", err)
	}

	// 遍历 lower 目录中不在 upper 中的条目，生成 whiteout
	for rel := range lowerEntries {
		if upperEntries[rel] {
			continue
		}
		// 检查父目录是否已被 opaque whiteout 覆盖
		// 如果父目录有 opaque，则该条目的 whiteout 已被 .wh..wh..opq 隐含
		parentOpaque := false
		dir := filepath.Dir(rel)
		for dir != "." && dir != "" {
			opqPath := filepath.Join(upperDir, dir, ".wh..wh..opq")
			if _, err := os.Lstat(opqPath); err == nil {
				parentOpaque = true
				break
			}
			// 检查 upper 中的目录是否有 opaque xattr
			upperDirPath := filepath.Join(upperDir, dir)
			opaqueBuf := make([]byte, 1)
			n, err := unix.Getxattr(upperDirPath, "trusted.overlay.opaque", opaqueBuf)
			if err == nil && n > 0 && string(opaqueBuf[:n]) == "y" {
				parentOpaque = true
				break
			}
			dir = filepath.Dir(dir)
		}
		if parentOpaque {
			continue
		}

		// 生成 .wh.<name> 条目
		dirPath := filepath.Dir(rel)
		base := filepath.Base(rel)
		whiteoutName := filepath.Join(dirPath, ".wh."+base)
		if err := tw.WriteHeader(&tar.Header{
			Typeflag: tar.TypeReg,
			Name:     whiteoutName,
			Size:     0,
			Mode:     0644,
		}); err != nil {
			return fmt.Errorf("写入删除 whiteout 条目失败: %w", err)
		}
	}

	return nil
}

// findInLower 在多个 lower 目录中查找文件（overlay 语义：上层覆盖下层）
// lowerDirs 顺序：最近父层在前，最远祖先在后
// 返回找到的文件路径、FileInfo，以及是否找到
// 对齐 overlay: 前面的目录（更近的父层）优先级更高
func findInLower(relPath string, lowerDirs []string) (string, os.FileInfo, bool) {
	// 从最近父层往后查找，找到的第一个即为 overlay 合并视图中的版本
	for i := 0; i < len(lowerDirs); i++ {
		path := filepath.Join(lowerDirs[i], relPath)
		info, err := os.Lstat(path)
		if err == nil {
			return path, info, true
		}
	}
	return "", nil, false
}

// isSameEntry 判断两个文件系统条目是否相同
func isSameEntry(upperPath string, upperInfo os.FileInfo, lowerPath string, lowerInfo os.FileInfo) (bool, error) {
	// 类型不同则肯定不同
	upperMode := upperInfo.Mode()
	lowerMode := lowerInfo.Mode()
	if upperMode.Type() != lowerMode.Type() {
		return false, nil
	}

	// 符号链接：比较链接目标
	if upperMode&os.ModeSymlink != 0 {
		upperTarget, err := os.Readlink(upperPath)
		if err != nil {
			return false, err
		}
		lowerTarget, err := os.Readlink(lowerPath)
		if err != nil {
			return false, err
		}
		return upperTarget == lowerTarget, nil
	}

	// 目录：总是认为可能有差异（子条目可能有变化）
	// 但如果权限和时间戳都相同，可以跳过
	if upperInfo.IsDir() {
		// 目录本身需要包含在 tar 中，因为权限可能改变
		// 但如果权限完全相同，可以不单独写入目录条目
		// 子条目的差异由 walkDiff 的遍历逻辑处理
		return upperMode.Perm() == lowerMode.Perm(), nil
	}

	// 普通文件：使用 os.SameFile 检查（处理硬链接）
	if os.SameFile(upperInfo, lowerInfo) {
		return true, nil
	}

	// 比较大小和修改时间
	if upperInfo.Size() != lowerInfo.Size() {
		return false, nil
	}

	// 比较权限
	if upperMode.Perm() != lowerMode.Perm() {
		return false, nil
	}

	// 比较修改时间（允许微小差异）
	upperMod := upperInfo.ModTime()
	lowerMod := lowerInfo.ModTime()
	if upperMod.Unix() != lowerMod.Unix() || upperMod.Nanosecond() != lowerMod.Nanosecond() {
		return false, nil
	}

	return true, nil
}

// writeTarEntry 将一个文件系统条目写入 tar
func writeTarEntry(tw *tar.Writer, fullPath, relPath string, info os.FileInfo) error {
	// 构建 tar header
	header, err := tar.FileInfoHeader(info, "")
	if err != nil {
		return fmt.Errorf("创建 tar header 失败 (%s): %w", relPath, err)
	}
	header.Name = relPath

	// 修复符号链接的名称
	if info.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(fullPath)
		if err != nil {
			return fmt.Errorf("读取符号链接失败 (%s): %w", relPath, err)
		}
		header.Linkname = target
	}

	// 清除不必要的时间精度（OCI 规范要求）
	header.Format = tar.FormatPAX

	if err := tw.WriteHeader(header); err != nil {
		return fmt.Errorf("写入 tar header 失败 (%s): %w", relPath, err)
	}

	// 普通文件需要写入内容
	if info.Mode().IsRegular() {
		f, err := os.Open(fullPath)
		if err != nil {
			return fmt.Errorf("打开文件失败 (%s): %w", relPath, err)
		}
		defer f.Close()

		if _, err := io.CopyN(tw, f, info.Size()); err != nil {
			return fmt.Errorf("写入文件内容失败 (%s): %w", relPath, err)
		}
	}

	return nil
}
