//go:build !linux

package pty

import (
	"fmt"
	"os"
)

type PTY struct {
	Master *os.File
	Slave  *os.File
	Name   string
}

func Open() (*PTY, error) {
	return nil, fmt.Errorf("pty 仅支持 Linux")
}

func (p *PTY) Close() {}

func (p *PTY) SetWinsize(rows, cols uint16) error {
	return fmt.Errorf("pty 仅支持 Linux")
}
