//go:build linux

package apply

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// extractLayerBlob 解压 tar.gz 层到目标目录，同时计算未压缩 tar 的 SHA-256
// 对齐 containerd: 将 blob 解压到 snapshots/<id>/fs/，并校验 DiffID
// 支持 tar.gz 和纯 tar 两种格式
// 返回值: 未压缩 tar 数据的 SHA-256 digest (sha256:...) 格式
func extractLayerBlob(blobPath, destDir string) (string, error) {
	f, err := os.Open(blobPath)
	if err != nil {
		return "", fmt.Errorf("打开 blob 失败: %w", err)
	}
	defer f.Close()

	var tarReader *tar.Reader
	h := sha256.New()

	magic := make([]byte, 2)
	if _, err := f.Read(magic); err != nil {
		return "", fmt.Errorf("读取 blob 头部失败: %w", err)
	}
	f.Seek(0, 0)

	if magic[0] == 0x1f && magic[1] == 0x8b {
		gzReader, err := gzip.NewReader(f)
		if err != nil {
			return "", fmt.Errorf("创建 gzip 读取器失败: %w", err)
		}
		defer gzReader.Close()
		// 对齐 containerd: DiffID 是未压缩 tar 的 SHA-256
		// 通过 io.TeeReader 在解压同时计算哈希，避免二次读取
		tarReader = tar.NewReader(io.TeeReader(gzReader, h))
	} else {
		// 纯 tar 格式：DiffID 就是文件本身的 SHA-256
		tarReader = tar.NewReader(io.TeeReader(f, h))
	}

	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", fmt.Errorf("读取 tar 条目失败: %w", err)
		}

		targetPath := filepath.Join(destDir, header.Name)

		if strings.Contains(header.Name, "..") {
			continue
		}

		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(targetPath, os.FileMode(header.Mode)); err != nil {
				return "", fmt.Errorf("创建目录 %s 失败: %w", header.Name, err)
			}

		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(targetPath), 0755); err != nil {
				return "", fmt.Errorf("创建父目录失败: %w", err)
			}
			outFile, err := os.OpenFile(targetPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(header.Mode))
			if err != nil {
				continue
			}
			bufWriter := bufio.NewWriterSize(outFile, 256*1024)
			if _, err := io.Copy(bufWriter, tarReader); err != nil {
				outFile.Close()
				continue
			}
			bufWriter.Flush()
			outFile.Close()

		case tar.TypeSymlink:
			if err := os.MkdirAll(filepath.Dir(targetPath), 0755); err != nil {
				continue
			}
			os.Remove(targetPath)
			if err := os.Symlink(header.Linkname, targetPath); err != nil {
				continue
			}

		case tar.TypeLink:
			if err := os.MkdirAll(filepath.Dir(targetPath), 0755); err != nil {
				continue
			}
			linkTarget := filepath.Join(destDir, header.Linkname)
			os.Remove(targetPath)
			if err := os.Link(linkTarget, targetPath); err != nil {
				continue
			}

		case tar.TypeChar:
			if err := os.MkdirAll(filepath.Dir(targetPath), 0755); err != nil {
				continue
			}
			os.Remove(targetPath)
			unix.Mknod(targetPath, syscall.S_IFCHR|uint32(header.Mode), int(mkdev(header.Devmajor, header.Devminor)))

		case tar.TypeBlock:
			if err := os.MkdirAll(filepath.Dir(targetPath), 0755); err != nil {
				continue
			}
			os.Remove(targetPath)
			unix.Mknod(targetPath, syscall.S_IFBLK|uint32(header.Mode), int(mkdev(header.Devmajor, header.Devminor)))
		}
	}

	return fmt.Sprintf("sha256:%x", h.Sum(nil)), nil
}

// processWhiteouts 处理 OCI whiteout 文件
// 对齐 containerd: diff.Applier 中的 whiteout 处理逻辑
//
// 在真实 containerd（使用 overlayfs 挂载）中，whiteout 通过 overlay 文件系统本身处理：
//   - 删除文件时 overlayfs 在 upperdir 中创建字符设备 (0,0)
//   - 这些字符设备是 overlayfs 在运行时理解的 "whiteout"
//
// mini-docker 直接写入 fs/ 目录（无 overlay 挂载），因此：
//   - .wh.<name> → 转换为字符设备 (major=0, minor=0)，命名为 <name>
//   - .wh..wh..opq → 在目录上设置 trusted.overlay.opaque xattr 为 "y"
//   - 不修改父层目录 —— 这是与旧实现的关键区别
//   - whiteout 字符设备和 opaque xattr 在运行时由 overlayfs 理解
func processWhiteouts(fsDir string, parentFSDirs []string) error {
	return filepath.Walk(fsDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}

		base := filepath.Base(path)
		dir := filepath.Dir(path)

		// 处理不透明白化 (.wh..wh..opq)
		// 表示该目录下的所有父层内容都应被隐藏
		if base == ".wh..wh..opq" {
			// 删除 whiteout 标记文件本身
			os.Remove(path)

			// 在目录上设置 trusted.overlay.opaque xattr
			// overlayfs 运行时会识别此属性，阻止查找 lowerdir
			// 注意：需要 CAP_SYS_ADMIN 或 root 权限，容器运行时通常具备
			if err := unix.Setxattr(dir, "trusted.overlay.opaque", []byte("y"), 0); err != nil {
				return fmt.Errorf("设置 opaque xattr 失败 (%s): %w", dir, err)
			}

			return nil
		}

		// 处理普通 whiteout (.wh.<name>)
		if strings.HasPrefix(base, ".wh.") && len(base) > 4 {
			targetName := base[4:] // 去掉 ".wh." 前缀
			os.Remove(path)        // 删除 whiteout 标记文件本身

			// 创建字符设备 (0,0) 作为 whiteout
			// overlayfs 运行时会识别此设备，表示对应文件已被删除
			whiteoutPath := filepath.Join(dir, targetName)
			os.Remove(whiteoutPath) // 先删除可能已存在的同名文件/目录
			if err := unix.Mknod(whiteoutPath, syscall.S_IFCHR|0644, int(mkdev(0, 0))); err != nil {
				return fmt.Errorf("创建 whiteout 设备失败 (%s): %w", whiteoutPath, err)
			}
		}

		return nil
	})
}

// mkdev 构造设备号（用于创建设备节点）
func mkdev(major, minor int64) uint32 {
	return uint32((minor & 0xff) | (major << 8) | ((minor & ^0xff) << 12))
}
