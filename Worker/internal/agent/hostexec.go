package agent

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// RunHostCommand executes a command in the host's namespaces via nsenter.
//
// The Worker container runs privileged with pid=host (see deploy/stack.yml),
// so `nsenter -t 1 -m -u -i -n -p -- /bin/sh -c "<cmd>"` enters the host's
// PID 1 namespaces and runs the command with the host's full environment —
// equivalent to an SSH session, subject to the command policy.
//
// Returns merged stdout+stderr, the exit code, and the elapsed time. A
// non-zero exit code is NOT an error (the caller reports it as part of the
// result); only setup/timeout failures return an error.
func RunHostCommand(ctx context.Context, command string, timeout time.Duration) (output string, exitCode int, elapsed time.Duration, err error) {
	start := time.Now()
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// nsenter needs a shell in the host's rootfs; /bin/sh is universally
	// present. The command string is passed as a single argument so no extra
	// quoting is applied beyond what the host shell does.
	args := []string{"-t", "1", "-m", "-u", "-i", "-n", "-p", "--", "/bin/sh", "-c", command}
	cmd := exec.CommandContext(runCtx, "nsenter", args...)

	var buf strings.Builder
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err = cmd.Run()
	elapsed = time.Since(start)

	if runCtx.Err() == context.DeadlineExceeded {
		return "", -1, elapsed, fmt.Errorf("command timed out after %s", timeout)
	}
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return strings.TrimSpace(buf.String()), exitErr.ExitCode(), elapsed, nil
		}
		return strings.TrimSpace(buf.String()), -1, elapsed, fmt.Errorf("execute host command: %w", err)
	}
	return strings.TrimSpace(buf.String()), 0, elapsed, nil
}
