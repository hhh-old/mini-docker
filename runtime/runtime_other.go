//go:build !linux

package runtime

import (
	"fmt"
	"os"
)

func Create(args []string) {
	fmt.Fprintln(os.Stderr, "runtime create 仅支持 Linux")
	os.Exit(1)
}

func Start(args []string) {
	fmt.Fprintln(os.Stderr, "runtime start 仅支持 Linux")
	os.Exit(1)
}

func Kill(args []string) {
	fmt.Fprintln(os.Stderr, "runtime kill 仅支持 Linux")
	os.Exit(1)
}

func Delete(args []string) {
	fmt.Fprintln(os.Stderr, "runtime delete 仅支持 Linux")
	os.Exit(1)
}

func State(args []string) {
	fmt.Fprintln(os.Stderr, "runtime state 仅支持 Linux")
	os.Exit(1)
}

func Pause(args []string) {
	fmt.Fprintln(os.Stderr, "runtime pause 仅支持 Linux")
	os.Exit(1)
}

func Resume(args []string) {
	fmt.Fprintln(os.Stderr, "runtime resume 仅支持 Linux")
	os.Exit(1)
}

func Exec(args []string) {
	fmt.Fprintln(os.Stderr, "runtime exec 仅支持 Linux")
	os.Exit(1)
}
