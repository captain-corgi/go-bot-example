//go:build unix

package session

import (
	"fmt"
	"syscall"
)

// ProcessGroupAttr returns the SysProcAttr agent subprocess launchers must
// use so that each session's subprocess, and every tool process it spawns,
// runs in its own OS process group (pgid == pid) that can be signalled as a
// unit without touching the daemon.
func ProcessGroupAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true}
}

// SignalProcessGroup sends sig to every process in the process group pgid.
// It refuses pgid values that would address the caller's own group or, via
// kill(2) special cases, arbitrary processes.
func SignalProcessGroup(pgid int, sig syscall.Signal) error {
	if pgid <= 1 {
		return fmt.Errorf("session: refusing to signal process group %d", pgid)
	}
	if pgid == syscall.Getpgrp() {
		return fmt.Errorf("session: refusing to signal the daemon's own process group %d", pgid)
	}
	return syscall.Kill(-pgid, sig)
}
