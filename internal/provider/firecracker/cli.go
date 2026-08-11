package firecracker

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
)

type commandRunner interface {
	Run(ctx context.Context, command string, args ...string) ([]byte, error)
}

type execCommandRunner struct{}

func (execCommandRunner) Run(ctx context.Context, command string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, command, args...) //nolint:gosec // trusted operator configuration only.
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("%s: %w: %s", command, err, stderr.String())
	}
	return out, nil
}

type process interface {
	PID() int
	Signal(signal os.Signal) error
	Wait() error
}

type processLauncher interface {
	Start(command string, args []string, stdout, stderr io.Writer) (process, error)
}

type execProcessLauncher struct{}

func (execProcessLauncher) Start(command string, args []string, stdout, stderr io.Writer) (process, error) {
	cmd := exec.Command(command, args...) //nolint:gosec // trusted operator configuration only.
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &execProcess{cmd: cmd}, nil
}

type execProcess struct{ cmd *exec.Cmd }

func (p *execProcess) PID() int { return p.cmd.Process.Pid }

func (p *execProcess) Signal(signal os.Signal) error { return p.cmd.Process.Signal(signal) }

func (p *execProcess) Wait() error { return p.cmd.Wait() }
