package libvirt

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
)

type commandRunner interface {
	Run(ctx context.Context, stdin []byte, command string, args ...string) ([]byte, error)
}

type execCommandRunner struct{}

func (execCommandRunner) Run(ctx context.Context, stdin []byte, command string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, command, args...) //nolint:gosec // binaries and arguments come from trusted operator configuration.
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("%s: %w: %s", command, err, stderr.String())
	}
	return out, nil
}
