//go:build !linux

package container

import (
	"fmt"
)

// ApplyCapabilities 非 Linux 平台的 stub
func ApplyCapabilities(capAdd []string, capDrop []string) error {
	return fmt.Errorf("Capability 仅在 Linux 上可用，请在 WSL2 或 Linux 环境中运行")
}

// ApplyCapabilitiesFromEnv 非 Linux 平台的 stub
func ApplyCapabilitiesFromEnv() {
}
