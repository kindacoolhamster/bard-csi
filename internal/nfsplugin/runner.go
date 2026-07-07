package nfsplugin

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// Runner runs the external commands the NFS plugin shells out to (mount, umount,
// findmnt). The node plane is exercised in tests with a fake Runner; production
// uses ExecRunner.
type Runner interface {
	Run(ctx context.Context, name string, args ...string) (string, error)
}

// ExecRunner runs commands for real.
type ExecRunner struct{}

func (ExecRunner) Run(ctx context.Context, name string, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

// isNotMounted classifies a umount error for an already-unmounted target, so
// NodeUnstage/NodeUnpublish stay idempotent on a CSI retry.
func isNotMounted(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "not mounted") || strings.Contains(s, "not currently mounted")
}
