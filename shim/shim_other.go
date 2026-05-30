//go:build !linux

package shim

import (
	"fmt"
	"os"
)

func Run(args []string) {
	fmt.Fprintln(os.Stderr, "shim 仅支持 Linux")
	os.Exit(1)
}
