//go:build !linux

package toolrun

import (
	"errors"
	"syscall"
)

func confine() (string, error) {
	return "", errors.New("conversion steps run only on Linux workers")
}

func credential(uint32) *syscall.SysProcAttr { return nil }
