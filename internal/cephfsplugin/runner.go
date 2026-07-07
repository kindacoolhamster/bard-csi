package cephfsplugin

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// Runner abstracts external command execution for testing.
type Runner interface {
	Run(ctx context.Context, name string, args ...string) (string, error)
}

// ExecRunner shells out to real binaries (ceph, ceph-fuse, mount, umount).
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

func isNotFound(err error) bool {
	s := strings.ToLower(errString(err))
	return strings.Contains(s, "does not exist") || strings.Contains(s, "not found") || strings.Contains(s, "no such")
}

func isAlreadyExists(err error) bool {
	s := strings.ToLower(errString(err))
	return strings.Contains(s, "already exists") || strings.Contains(s, "file exists")
}

func isNotMounted(err error) bool {
	s := strings.ToLower(errString(err))
	return strings.Contains(s, "not mounted") || strings.Contains(s, "not currently mounted")
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
