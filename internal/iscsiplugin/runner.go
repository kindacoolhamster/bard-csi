package iscsiplugin

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// Runner abstracts external command execution for testing (lvcreate, targetcli,
// iscsiadm, mkfs, mount, ...).
type Runner interface {
	Run(ctx context.Context, name string, args ...string) (string, error)
}

// ExecRunner shells out to real binaries.
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

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// isNotFound classifies a delete/lookup of an object that is already gone -- the
// idempotent case. Covers lvm ("Failed to find"), targetcli ("No such",
// "does not exist") and iscsiadm ("no records"/"no session") phrasings.
func isNotFound(err error) bool {
	s := strings.ToLower(errString(err))
	return strings.Contains(s, "failed to find") || strings.Contains(s, "not found") ||
		strings.Contains(s, "does not exist") || strings.Contains(s, "no such") ||
		strings.Contains(s, "no records") || strings.Contains(s, "could not find")
}

// isExists classifies a create of an object that already exists -- so CreateVolume
// and ControllerPublish converge on a retry without erroring.
func isExists(err error) bool {
	s := strings.ToLower(errString(err))
	return strings.Contains(s, "already exists") || strings.Contains(s, "already in") ||
		strings.Contains(s, "exists in configfs")
}

func isNotMounted(err error) bool {
	s := strings.ToLower(errString(err))
	return strings.Contains(s, "not mounted") || strings.Contains(s, "not currently mounted")
}

// isNotLarger reports an lvextend that asked for a size not exceeding the current
// one -- the idempotent grow case.
func isNotLarger(err error) bool {
	s := strings.ToLower(errString(err))
	// "No size change." is what current lvm2 (2.03+) says for an equal-size
	// lvextend; older releases say "matches existing size" / "not larger".
	return strings.Contains(s, "matches existing size") || strings.Contains(s, "not larger") ||
		strings.Contains(s, "no size change")
}

// isAlreadyLoggedIn classifies an iscsiadm --login to a session that already
// exists (idempotent stage retry).
func isAlreadyLoggedIn(err error) bool {
	s := strings.ToLower(errString(err))
	return strings.Contains(s, "already present") || strings.Contains(s, "already exists") ||
		strings.Contains(s, "already logged in")
}
