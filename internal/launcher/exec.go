package launcher

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

// ExecStarter runs real processes attached to this process's terminal. The
// child shares the foreground process group, so the terminal delivers Ctrl+C
// and job-control signals to it directly.
type ExecStarter struct{}

type execProcess struct{ cmd *exec.Cmd }

// Start launches spec with inherited stdio.
func (ExecStarter) Start(spec Spec) (Process, error) {
	cmd := exec.Command(spec.Path, spec.Args...)
	cmd.Dir = spec.Dir
	cmd.Env = spec.Env
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &execProcess{cmd: cmd}, nil
}

func (p *execProcess) PID() int { return p.cmd.Process.Pid }

func (p *execProcess) Signal(sig os.Signal) error { return p.cmd.Process.Signal(sig) }

// Wait returns the exit code, or 128+signal when the child was killed by a signal (shell convention).
func (p *execProcess) Wait() (int, error) {
	err := p.cmd.Wait()
	if err == nil {
		return 0, nil
	}
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		return 1, fmt.Errorf("wait for child: %w", err)
	}
	if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return 128 + int(ws.Signal()), nil
	}
	return ee.ExitCode(), nil
}
