package collect

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const (
	systemctlTimeout   = 5 * time.Second
	commandWaitDelay   = time.Second
	systemdRuntimePath = "run/systemd/system"
)

// CommandRunner runs an external command and returns its standard output.
type CommandRunner func(ctx context.Context, name string, args ...string) ([]byte, error)

// ExecRunner is the CommandRunner that runs real processes. A child that
// ignores cancellation is abandoned after a one second grace period.
func ExecRunner(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.WaitDelay = commandWaitDelay
	return cmd.Output()
}

// FailedUnits lists the systemd units in the failed state. It returns nil, nil
// without running anything when systemd is not the init system under root. The
// result is otherwise non-nil, and empty when nothing has failed. Any error
// from the command is returned with a nil result.
func FailedUnits(ctx context.Context, root string, run CommandRunner) ([]string, error) {
	info, err := os.Stat(filepath.Join(root, systemdRuntimePath))
	if err != nil || !info.IsDir() {
		return nil, nil
	}

	ctx, cancel := context.WithTimeout(ctx, systemctlTimeout)
	defer cancel()
	out, err := run(ctx, "systemctl", "list-units", "--state=failed", "--plain", "--no-legend")
	if err != nil {
		return nil, err
	}

	units := []string{}
	for line := range strings.Lines(string(out)) {
		fields := strings.Fields(line)
		if len(fields) > 0 && fields[0] == "●" {
			fields = fields[1:]
		}
		if len(fields) > 0 {
			units = append(units, fields[0])
		}
	}
	return units, nil
}
