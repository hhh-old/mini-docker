//go:build !linux

package runtime

import "fmt"

var errLinuxOnly = fmt.Errorf("runtime 仅支持 Linux 平台")

func Create(args []string) error { return errLinuxOnly }
func Start(args []string) error  { return errLinuxOnly }
func Kill(args []string) error   { return errLinuxOnly }
func Delete(args []string) error { return errLinuxOnly }
func State(args []string) error  { return errLinuxOnly }
func Pause(args []string) error  { return errLinuxOnly }
func Resume(args []string) error { return errLinuxOnly }
func Exec(args []string) error   { return errLinuxOnly }
