// Command kubectl-bard is the kubectl plugin front-end for Bard's day-2
// tooling: put it on PATH and `kubectl bard inspect` runs the consistency
// scanner inside the controller pod (the only place the backend plugin
// sockets are reachable) and streams the report back.
//
// It shells out to kubectl for discovery and exec, so it inherits the user's
// kubeconfig/context/auth handling and needs no Kubernetes client library.
package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// version is overridden at build time via -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "inspect":
		os.Exit(runInspect(os.Args[2:]))
	case "version":
		fmt.Printf("kubectl-bard %s\n", version)
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "kubectl bard: unknown command %q\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `Bard CSI day-2 tooling (kubectl plugin).

Usage:
  kubectl bard inspect [flags]   run the consistency scanner in the controller pod
  kubectl bard version

The scanner joins Kubernetes state (PVs, snapshots, attachments, topology)
against what actually exists on every configured backend and reports drift.
Read-only. Exit code: 0 = consistent, 1 = ERROR findings, 2 = could not scan.

Run "kubectl bard inspect -h" for its flags.
`)
}

func runInspect(args []string) int {
	fs := flag.NewFlagSet("kubectl bard inspect", flag.ExitOnError)
	var namespace, kctx, kubeconfig, selector, container, binary, output string
	fs.StringVar(&namespace, "namespace", "kube-system", "namespace the Bard controller runs in")
	fs.StringVar(&namespace, "n", "kube-system", "shorthand for --namespace")
	fs.StringVar(&kctx, "context", "", "kubeconfig context (passed to kubectl)")
	fs.StringVar(&kubeconfig, "kubeconfig", "", "kubeconfig path (passed to kubectl)")
	fs.StringVar(&selector, "selector", "app=bard-csi-controller", "label selector locating the controller pod (the Helm chart labels it app=<fullname>-controller)")
	fs.StringVar(&container, "container", "bard-csi", "controller pod container to exec in")
	fs.StringVar(&binary, "binary", "/usr/local/bin/bard-csi", "path of the bard-csi binary inside the container")
	fs.StringVar(&output, "output", "table", "report format: table or json")
	fs.Parse(args)

	base := []string{}
	if kctx != "" {
		base = append(base, "--context", kctx)
	}
	if kubeconfig != "" {
		base = append(base, "--kubeconfig", kubeconfig)
	}

	// Find a running controller pod. -o name over a field selector is safe on
	// an empty result (unlike a jsonpath index).
	getArgs := append(append([]string{}, base...),
		"-n", namespace, "get", "pods",
		"-l", selector, "--field-selector=status.phase=Running", "-o", "name")
	out, err := exec.Command("kubectl", getArgs...).Output()
	if err != nil {
		fmt.Fprintf(os.Stderr, "kubectl bard: locate controller pod: %v\n%s", err, execStderr(err))
		return 2
	}
	pods := strings.Fields(strings.TrimSpace(string(out)))
	if len(pods) == 0 {
		fmt.Fprintf(os.Stderr, "kubectl bard: no running pod matches -l %q in namespace %q; adjust --namespace/--selector\n", selector, namespace)
		return 2
	}
	pod := strings.TrimPrefix(pods[0], "pod/")

	execArgs := append(append([]string{}, base...),
		"-n", namespace, "exec", pod, "-c", container, "--",
		binary, "--inspect", "--output="+output)
	cmd := exec.Command("kubectl", execArgs...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return ee.ExitCode() // the scanner's own exit code (1 = ERROR findings)
		}
		fmt.Fprintf(os.Stderr, "kubectl bard: exec: %v\n", err)
		return 2
	}
	return 0
}

// execStderr surfaces kubectl's stderr from an exec.ExitError, since Output()
// swallows it.
func execStderr(err error) string {
	if ee, ok := err.(*exec.ExitError); ok {
		return string(ee.Stderr)
	}
	return ""
}
