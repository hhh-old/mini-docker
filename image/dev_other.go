//go:build !linux

package image

import "os"

func createDevNull(path string) error {
	return os.WriteFile(path, nil, 0644)
}
