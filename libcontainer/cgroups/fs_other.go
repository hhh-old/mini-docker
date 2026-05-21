//go:build !linux

package cgroups

import (
	"fmt"
	"os"
	"strings"

	"mini-docker/libcontainer/configs"
)

type managerV1 struct {
	config *configs.Resources
}

func newManagerV1(config *configs.Resources) (Manager, error) {
	return nil, fmt.Errorf("cgroups 仅支持 Linux")
}

func (m *managerV1) Apply(pid int) error                    { return fmt.Errorf("不支持") }
func (m *managerV1) Destroy() error                         { return fmt.Errorf("不支持") }
func (m *managerV1) GetPaths() map[string]string            { return nil }
func (m *managerV1) GetStats() (*Stats, error)              { return nil, fmt.Errorf("不支持") }
func (m *managerV1) Freeze() error                          { return fmt.Errorf("不支持") }
func (m *managerV1) Thaw() error                            { return fmt.Errorf("不支持") }
func (m *managerV1) Set(container *configs.Resources) error { return fmt.Errorf("不支持") }

func writeFile(path, data string) error {
	return os.WriteFile(path, []byte(data), 0644)
}

func readFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

func mkdirAll_(path string) error {
	return os.MkdirAll(path, 0755)
}

func readUint64(path string) (uint64, error) {
	return 0, fmt.Errorf("不支持")
}
