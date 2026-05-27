//go:build !linux

package libcontainer

import (
	"fmt"

	"mini-docker/libcontainer/configs"
)

type linuxContainer struct {
	id     string
	config *configs.Config
	pid    int
	status Status
}

func newLinuxContainer(id string, config *configs.Config) (*linuxContainer, error) {
	return nil, fmt.Errorf("libcontainer 仅支持 Linux")
}

func loadLinuxContainer(id string) (*linuxContainer, error) {
	return nil, fmt.Errorf("libcontainer 仅支持 Linux")
}

func (c *linuxContainer) ID() string                         { return c.id }
func (c *linuxContainer) Status() (Status, error)            { return c.status, nil }
func (c *linuxContainer) Config() configs.Config             { return *c.config }
func (c *linuxContainer) Pid() int                           { return c.pid }
func (c *linuxContainer) Start(process *Process) error       { return fmt.Errorf("不支持") }
func (c *linuxContainer) Run(process *Process) error         { return fmt.Errorf("不支持") }
func (c *linuxContainer) Destroy() error                     { return fmt.Errorf("不支持") }
func (c *linuxContainer) Pause() error                       { return fmt.Errorf("不支持") }
func (c *linuxContainer) Resume() error                      { return fmt.Errorf("不支持") }
func (c *linuxContainer) Signal(sig int) error               { return fmt.Errorf("不支持") }
func (c *linuxContainer) Exec(process *Process) error        { return fmt.Errorf("不支持") }
func (c *linuxContainer) Stats() (*Stats, error)             { return nil, fmt.Errorf("不支持") }
func (c *linuxContainer) Set(config configs.Resources) error { return fmt.Errorf("不支持") }
func (c *linuxContainer) SetStatus(status Status) error      { return fmt.Errorf("不支持") }
