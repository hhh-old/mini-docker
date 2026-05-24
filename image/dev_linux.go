//go:build linux

package image

import (
	"os"

	"golang.org/x/sys/unix"
)

func createDevNull(path string) error {
	if err := unix.Mknod(path, unix.S_IFCHR|0666, 3); err != nil {
		return os.WriteFile(path, nil, 0644)
	}
	return nil
}
