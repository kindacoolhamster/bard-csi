package cephplugin

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// Runner abstracts external command execution so the backend can be unit tested
// without a real Ceph cluster or root privileges.
type Runner interface {
	Run(ctx context.Context, name string, args ...string) (string, error)
}

// ExecRunner is the production Runner: it shells out to real binaries.
type ExecRunner struct{}

func (ExecRunner) Run(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return stdout.String(), fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// Error classifiers keep control-plane operations idempotent: deleting an
// already-gone image or creating an existing one must not fail the call.

func isNotFound(err error) bool {
	s := strings.ToLower(errString(err))
	return strings.Contains(s, "does not exist") || strings.Contains(s, "no such file") || strings.Contains(s, "not found")
}

func isAlreadyExists(err error) bool {
	s := strings.ToLower(errString(err))
	return strings.Contains(s, "exists") || strings.Contains(s, "file exists")
}

func isNotMounted(err error) bool {
	s := strings.ToLower(errString(err))
	return strings.Contains(s, "not mounted") || strings.Contains(s, "not currently mounted")
}

// errString returns the error text with the benign ceph "can't open ceph.conf:
// (2) No such file or directory" warning lines removed. The plugin runs rbd/ceph
// without a config file (it passes -m/--id/--keyfile and -c /dev/null instead), so
// that warning is harmless noise -- but its "No such file or directory" substring
// would otherwise make isNotFound() misread ANY failure (e.g. a watcher-blocked
// `rbd rm`, exit 16) as not-found, so the call would report false success and
// orphan the image. A genuine not-found ("error opening image ...: (2) No such
// file or directory") carries no "ceph.conf", so it still classifies correctly.
func errString(err error) string {
	if err == nil {
		return ""
	}
	lines := strings.Split(err.Error(), "\n")
	kept := lines[:0]
	for _, l := range lines {
		if strings.Contains(l, "ceph.conf") {
			continue
		}
		kept = append(kept, l)
	}
	return strings.Join(kept, "\n")
}
