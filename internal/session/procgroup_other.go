//go:build !unix

package session

import (
	"errors"
	"syscall"
)

// ProcessGroupAttr returns an empty SysProcAttr: OS process groups are only
// supported on unix platforms.
func ProcessGroupAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{}
}

// SignalProcessGroup is unsupported on non-unix platforms.
func SignalProcessGroup(int, syscall.Signal) error {
	return errors.ErrUnsupported
}
