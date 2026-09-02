//go:build !windows

package doltserver

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
)

// setProcessGroup puts the command in its own process group so that signals
// sent to the parent process group (e.g. SIGHUP when the caller calls
// syscall.Exec to become tmux) don't reach the spawned process.
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// processIsAlive checks whether a process with the given PID is still running.
func processIsAlive(pid int) bool {
	process, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	if process.Signal(syscall.Signal(0)) != nil {
		return false
	}
	output, err := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
	return err != nil || processStatusIsAlive(string(output))
}

func processStatusIsAlive(status string) bool {
	return !strings.HasPrefix(strings.TrimSpace(status), "Z")
}

// gracefulTerminate sends SIGTERM for graceful shutdown on Unix.
func gracefulTerminate(p *os.Process) error {
	return p.Signal(syscall.SIGTERM)
}
