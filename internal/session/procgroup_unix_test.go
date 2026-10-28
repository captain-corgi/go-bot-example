//go:build unix

package session_test

import (
	"errors"
	"os/exec"
	"syscall"
	"testing"

	"github.com/captain-corgi/go-bot-example/internal/session"
)

func TestProcessGroupAttrIsolatesAndSignalsGroup(t *testing.T) {
	sleep, err := exec.LookPath("sleep")
	if err != nil {
		t.Skip("sleep binary not available")
	}
	cmd := exec.Command(sleep, "30")
	cmd.SysProcAttr = session.ProcessGroupAttr()
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	pid := cmd.Process.Pid
	pgid, err := syscall.Getpgid(pid)
	if err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatalf("Getpgid: %v", err)
	}
	if pgid != pid || pgid == syscall.Getpgrp() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatalf("pgid = %d, want own group %d distinct from the daemon's %d", pgid, pid, syscall.Getpgrp())
	}

	if err := session.SignalProcessGroup(pgid, syscall.SIGINT); err != nil {
		t.Fatalf("SignalProcessGroup: %v", err)
	}
	var exitErr *exec.ExitError
	if err := cmd.Wait(); !errors.As(err, &exitErr) {
		t.Fatalf("Wait err = %v, want the process to be terminated by SIGINT", err)
	}
	if ws, ok := exitErr.Sys().(syscall.WaitStatus); !ok || !ws.Signaled() || ws.Signal() != syscall.SIGINT {
		t.Errorf("exit status = %v, want killed by SIGINT", exitErr)
	}
}

func TestSignalProcessGroupRefusesUnsafeTargets(t *testing.T) {
	for _, pgid := range []int{-5, 0, 1, syscall.Getpgrp()} {
		if err := session.SignalProcessGroup(pgid, syscall.SIGINT); err == nil {
			t.Errorf("SignalProcessGroup(%d) succeeded; want refusal", pgid)
		}
	}
}
