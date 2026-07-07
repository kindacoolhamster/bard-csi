// bard-plugin-conformance drives a Bard backend plugin over its unix socket
// the way Bard core would and verifies it honors the wire contract: required
// semantics (idempotent create/delete, error codes, identity rules, unknown-
// field tolerance) plus every optional capability the plugin declares. It is
// the acceptance bar for a new backend plugin; see docs/writing-a-plugin.md.
//
//	bard-plugin-conformance -instance my-east [flags] <socket>
//
// Control-plane checks need no privileges beyond the plugin's own; -node also
// stages and publishes a real mount and usually needs root. The checks create
// and delete real volumes/snapshots -- run against a disposable instance.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/kindacoolhamster/bard-csi/internal/conformance"
)

type paramFlag map[string]string

func (p paramFlag) String() string { return fmt.Sprint(map[string]string(p)) }
func (p paramFlag) Set(s string) error {
	k, v, ok := strings.Cut(s, "=")
	if !ok || k == "" {
		return fmt.Errorf("want key=value, got %q", s)
	}
	p[k] = v
	return nil
}

func main() {
	params := paramFlag{}
	var (
		instance  = flag.String("instance", "", "instance id from the plugin's config to provision against (required)")
		fsType    = flag.String("fstype", "", "fsType for created volumes (default: plugin default)")
		size      = flag.Int64("size", 16<<20, "requested size in bytes for test volumes")
		node      = flag.Bool("node", false, "also run node-plane checks (stage/publish real mounts; usually needs root)")
		staging   = flag.String("staging-dir", "", "directory for staging/target paths (default: a temp dir)")
		prefix    = flag.String("prefix", "", "name prefix for created resources (default: conf-<random>)")
		opTimeout = flag.Duration("op-timeout", 2*time.Minute, "per-operation timeout")
		verbose   = flag.Bool("v", false, "log each check as it runs")
	)
	flag.Var(params, "param", "StorageClass-style parameter key=value (repeatable)")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: %s -instance <id> [flags] <socket>\n\nflags:\n", os.Args[0])
		flag.PrintDefaults()
	}
	flag.Parse()
	socket := flag.Arg(0)
	if socket == "" || *instance == "" {
		flag.Usage()
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	cfg := conformance.Config{
		Socket:        socket,
		Instance:      *instance,
		Parameters:    params,
		FsType:        *fsType,
		CapacityBytes: *size,
		NamePrefix:    *prefix,
		Node:          *node,
		StagingDir:    *staging,
		OpTimeout:     *opTimeout,
	}
	if *verbose {
		cfg.Logf = func(format string, args ...any) { fmt.Fprintf(os.Stderr, format+"\n", args...) }
	}

	results, err := conformance.Run(ctx, cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "bard-plugin-conformance: %v\n", err)
		os.Exit(2)
	}

	counts := map[conformance.Status]int{}
	for _, res := range results {
		counts[res.Status]++
		fmt.Printf("%-4s %-28s %s\n", res.Status, res.Name, res.Detail)
	}
	fmt.Printf("\n%d passed, %d failed, %d warnings, %d skipped\n",
		counts[conformance.Pass], counts[conformance.Fail], counts[conformance.Warn], counts[conformance.Skip])
	if conformance.Failed(results) {
		os.Exit(1)
	}
}
