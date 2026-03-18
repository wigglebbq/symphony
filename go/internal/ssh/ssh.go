package ssh

import (
	"context"
	"os"
	"os/exec"
)

func CommandContext(ctx context.Context, host, script string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "ssh", host, "bash", "-lc", script)
	cmd.Stderr = os.Stderr
	return cmd
}
