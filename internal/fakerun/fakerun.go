// Package fakerun is an in-memory simulation of the external commands the
// Ceph RBD backend shells out to (rbd, mount, blkid, findmnt, mkfs, ...).
//
// It implements cephrbd.Runner so the driver can be exercised end-to-end —
// including the node-plane map/format/mount path — without a real Ceph cluster
// or root privileges. It tracks just enough state (which images exist, which
// devices are mapped, which are formatted, what is mounted where) to keep the
// simulated cluster self-consistent across a CreateVolume → NodeStage →
// NodePublish → ... → DeleteVolume sequence.
package fakerun

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// Runner is a fake cephrbd.Runner.
type Runner struct {
	mu         sync.Mutex
	images     map[string]int64  // "pool[/namespace]/image" -> size in MiB
	namespaces map[string]bool   // "pool/namespace"
	snaps      map[string]bool   // "pool/image@snap"
	devBySpec  map[string]string // "pool/image" -> "/dev/rbdN"
	specByDev  map[string]string // reverse
	formatted  map[string]bool   // device -> has filesystem
	mounts     map[string]string // mountpoint -> device
	luks       map[string]bool   // device -> is a LUKS container
	luksOpen   map[string]string // mapper name -> backing device
	imageMeta  map[string]string // "spec\x00key" -> value
	parents    map[string]string // clone "pool[/ns]/image" -> parent "pool[/ns]/image"
	// trashedSnaps models clone-v2 snapshot trash: `snap rm` on a snapshot with
	// linked clones succeeds but moves the snap to the trash namespace, where it
	// still blocks `rbd rm` of its image until every clone is flattened or removed
	// (live-verified on Ceph 20.2: "image has snapshots with linked clones").
	trashedSnaps map[string]bool // "spec@snap"
	nextDev      int
}

// New returns an empty fake runner.
func New() *Runner {
	return &Runner{
		images:     map[string]int64{},
		namespaces: map[string]bool{},
		snaps:      map[string]bool{},
		devBySpec:  map[string]string{},
		specByDev:  map[string]string{},
		formatted:  map[string]bool{},
		mounts:     map[string]string{},
		luks:       map[string]bool{},
		luksOpen:   map[string]string{},
		imageMeta:  map[string]string{},
		parents:    map[string]string{},

		trashedSnaps: map[string]bool{},
	}
}

// Run dispatches on the command name and simulates its effect.
func (r *Runner) Run(_ context.Context, name string, args ...string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	switch name {
	case "rbd", "rbd-nbd":
		return r.rbd(args)
	case "cryptsetup":
		return r.cryptsetup(args)
	case "mount":
		return r.mount(args)
	case "umount":
		return r.umount(args)
	case "blkid":
		return r.blkid(args)
	case "findmnt":
		return r.findmnt(args)
	case "mkfs.ext4", "mkfs.xfs":
		return r.mkfs(args)
	case "blockdev":
		// --getsize64 <dev>: non-zero only while the device is mapped.
		dev := args[len(args)-1]
		if _, ok := r.specByDev[dev]; ok {
			return "1073741824\n", nil
		}
		return "0\n", nil
	case "resize2fs", "xfs_growfs":
		return "", nil
	default:
		return "", fmt.Errorf("fakerun: unexpected command %q", name)
	}
}

// rbd handles create/rm/resize/clone/map/unmap and snap create/rm. Connection
// flags (-m, --id, --keyfile) are skipped; positional args carry the intent.
func (r *Runner) rbd(args []string) (string, error) {
	pos := positional(args)
	if len(pos) == 0 {
		return "", fmt.Errorf("fakerun: rbd: no subcommand")
	}
	switch pos[0] {
	case "info":
		size, ok := r.images[pos[1]]
		if !ok {
			return "", fmt.Errorf("rbd: error opening image %s: (2) No such file or directory", pos[1])
		}
		// Emit the parent object (pool[/namespace]/image) for a clone, so the backend
		// can walk the clone-depth chain. A flattened/base image has no parent.
		if parent, ok := r.parents[pos[1]]; ok {
			pool, ns, img := splitSpec(parent)
			return fmt.Sprintf(
				`{"size":%d,"parent":{"pool":%q,"pool_namespace":%q,"image":%q,"snapshot":"snap"}}`,
				size*(1<<20), pool, ns, img), nil
		}
		return fmt.Sprintf(`{"size":%d}`, size*(1<<20)), nil
	case "status":
		// A fresh image has no watchers; a mapped one has a single synthetic
		// watcher. The single-writer fence path only runs before this fake has
		// mapped the image, so it sees no watcher and never blocklists -- the
		// correct behaviour for a self-consistent single-client simulation.
		if _, ok := r.devBySpec[pos[1]]; ok {
			return `{"watchers":[{"address":"10.0.0.1:0/1"}]}`, nil
		}
		return `{"watchers":[]}`, nil
	case "create":
		r.images[pos[1]] = flagValueMiB(args, "--size")
		return "", nil
	case "clone", "cp":
		// clone/cp <parent[@snap]> <dest>: inherit the parent's size. A `clone` is a
		// COW child (records the parent for clone-depth tracking, inheriting the
		// parent's own parent depth); a `cp` is an independent full copy (no parent).
		base := strings.SplitN(pos[1], "@", 2)[0]
		r.images[pos[2]] = r.images[base]
		if pos[0] == "clone" {
			r.parents[pos[2]] = base
		}
		return "", nil
	case "flatten":
		// flatten <image>: copy the parent's data in and sever the parent link;
		// the ex-parent's trashed snaps are released once no clones remain.
		parent := r.parents[pos[1]]
		delete(r.parents, pos[1])
		r.releaseTrashedSnaps(parent)
		return "", nil
	case "rm":
		img := pos[1]
		if _, ok := r.images[img]; !ok {
			return "", fmt.Errorf("rbd: error opening image %s: (2) No such file or directory", img)
		}
		// Live semantics: an image cannot be removed while it has snapshots --
		// live ones, or trashed ones that still have linked clones.
		for spec := range r.snaps {
			if base, _, ok := strings.Cut(spec, "@"); ok && base == img {
				return "", fmt.Errorf("rbd: image has snapshots - not removing")
			}
		}
		for spec := range r.trashedSnaps {
			if base, _, ok := strings.Cut(spec, "@"); ok && base == img {
				return "", fmt.Errorf("rbd: image has snapshots with linked clones - these must be deleted or flattened before the image can be removed")
			}
		}
		parent := r.parents[img]
		delete(r.images, img)
		delete(r.parents, img)
		r.releaseTrashedSnaps(parent)
		return "", nil
	case "resize":
		r.images[pos[1]] = flagValueMiB(args, "--size")
		return "", nil
	case "map":
		spec := pos[1]
		if dev, ok := r.devBySpec[spec]; ok {
			return dev, nil
		}
		dev := fmt.Sprintf("/dev/rbd%d", r.nextDev)
		r.nextDev++
		r.devBySpec[spec] = dev
		r.specByDev[dev] = spec
		return dev, nil
	case "unmap":
		// The argument may be a device (/dev/...) or an image spec (pool/image).
		arg := pos[1]
		if spec, ok := r.specByDev[arg]; ok { // arg is a device
			delete(r.devBySpec, spec)
			delete(r.specByDev, arg)
		} else if dev, ok := r.devBySpec[arg]; ok { // arg is an image spec
			delete(r.specByDev, dev)
			delete(r.devBySpec, arg)
		}
		return "", nil
	case "config":
		// config image set|remove <pool/image> <key> [value]: QoS overrides.
		// Succeeds for any image (the fake does not track config values).
		return "", nil
	case "children":
		// children --all <spec> [--format json]: the clone children of an image's
		// snapshots, including children of trashed snapshots.
		img := pos[1]
		type childJSON struct {
			Pool          string `json:"pool"`
			PoolNamespace string `json:"pool_namespace"`
			Image         string `json:"image"`
		}
		out := []childJSON{}
		for child, parent := range r.parents {
			if parent == img {
				pool, ns, name := splitSpec(child)
				out = append(out, childJSON{Pool: pool, PoolNamespace: ns, Image: name})
			}
		}
		sort.Slice(out, func(i, j int) bool { return out[i].Image < out[j].Image })
		b, _ := json.Marshal(out)
		return string(b), nil
	case "image-meta":
		// image-meta set|get <pool/image> <key> [value].
		switch pos[1] {
		case "set":
			r.imageMeta[pos[2]+"\x00"+pos[3]] = pos[4]
			return "", nil
		case "get":
			v, ok := r.imageMeta[pos[2]+"\x00"+pos[3]]
			if !ok {
				return "", fmt.Errorf("rbd: failed to get metadata %s of image : (2) No such file or directory", pos[3])
			}
			return v, nil
		}
		return "", nil
	case "ls":
		// ls <pool> [--namespace ns] [--format json]: JSON array of image names in the
		// pool's default namespace, or in the named rados namespace.
		pool := pos[1]
		prefix := pool + "/"
		if ns := flagValue(args, "--namespace"); ns != "" {
			prefix = pool + "/" + ns + "/"
		}
		var names []string
		for spec := range r.images {
			rest, ok := strings.CutPrefix(spec, prefix)
			if !ok || strings.Contains(rest, "/") { // not this pool/namespace level
				continue
			}
			names = append(names, rest)
		}
		sort.Strings(names)
		out, _ := json.Marshal(names)
		return string(out), nil
	case "namespace":
		// namespace ls <pool> [--format json] | namespace create <pool/namespace>
		switch pos[1] {
		case "ls":
			pool := pos[2]
			type nsJSON struct {
				Name string `json:"name"`
			}
			var out []nsJSON
			for ns := range r.namespaces {
				if p, n, ok := strings.Cut(ns, "/"); ok && p == pool {
					out = append(out, nsJSON{Name: n})
				}
			}
			sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
			b, _ := json.Marshal(out)
			return string(b), nil
		case "create":
			r.namespaces[pos[2]] = true
		}
		return "", nil
	case "snap":
		// snap create|rm|ls <pool/image[@snap]>
		switch pos[1] {
		case "create":
			r.snaps[pos[2]] = true
		case "rm":
			if !r.snaps[pos[2]] {
				return "", fmt.Errorf("rbd: failed to remove snapshot: (2) No such file or directory")
			}
			delete(r.snaps, pos[2])
			// Clone v2: removing a snapshot with linked clones succeeds, but the
			// snap moves to the trash namespace and keeps blocking `rbd rm` of the
			// image until every clone is flattened or removed.
			if base, _, ok := strings.Cut(pos[2], "@"); ok && r.hasCloneChildren(base) {
				r.trashedSnaps[pos[2]] = true
			}
		case "ls":
			// ls <pool/image>: JSON array of {name,size} for that image's snaps.
			img := pos[2]
			type snapJSON struct {
				Name string `json:"name"`
				Size int64  `json:"size"`
			}
			var out []snapJSON
			for spec := range r.snaps {
				if base, snap, ok := strings.Cut(spec, "@"); ok && base == img {
					out = append(out, snapJSON{Name: snap, Size: r.images[img] * (1 << 20)})
				}
			}
			sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
			b, _ := json.Marshal(out)
			return string(b), nil
		}
		return "", nil
	default:
		return "", fmt.Errorf("fakerun: rbd: unhandled subcommand %q", pos[0])
	}
}

// hasCloneChildren reports whether any clone still references img as its parent.
func (r *Runner) hasCloneChildren(img string) bool {
	for _, p := range r.parents {
		if p == img {
			return true
		}
	}
	return false
}

// releaseTrashedSnaps drops img's trashed snapshots once its last clone is gone
// (flattened or removed) -- the v2 trash auto-purge.
func (r *Runner) releaseTrashedSnaps(img string) {
	if img == "" || r.hasCloneChildren(img) {
		return
	}
	for spec := range r.trashedSnaps {
		if base, _, ok := strings.Cut(spec, "@"); ok && base == img {
			delete(r.trashedSnaps, spec)
		}
	}
}

// cryptsetup models the LUKS lifecycle: isLuks/luksFormat tag a device as a LUKS
// container; open/close/status manage a dm-crypt mapping over it. cryptsetup
// signals "is LUKS" / "is active" through the exit code, so absence returns an
// error (which the backend treats as the negative answer).
func (r *Runner) cryptsetup(args []string) (string, error) {
	pos := positional(args)
	if len(pos) == 0 {
		return "", fmt.Errorf("fakerun: cryptsetup: no subcommand")
	}
	switch pos[0] {
	case "isLuks":
		if r.luks[pos[1]] {
			return "", nil
		}
		return "", fmt.Errorf("Device %s is not a valid LUKS device.", pos[1])
	case "luksFormat":
		r.luks[pos[1]] = true
		return "", nil
	case "open": // open <dev> <mapper>
		r.luksOpen[pos[2]] = pos[1]
		return "", nil
	case "status": // status <mapper>
		if dev, ok := r.luksOpen[pos[1]]; ok {
			return fmt.Sprintf("/dev/mapper/%s is active.\n  device: %s\n", pos[1], dev), nil
		}
		return "", fmt.Errorf("/dev/mapper/%s is inactive.", pos[1])
	case "close": // close <mapper>
		delete(r.luksOpen, pos[1])
		return "", nil
	default:
		return "", fmt.Errorf("fakerun: cryptsetup: unhandled subcommand %q", pos[0])
	}
}

// mount handles both "-t fs -o flags <dev> <dst>" and "--bind <src> <dst>".
func (r *Runner) mount(args []string) (string, error) {
	pos := positional(args)
	if len(pos) < 2 {
		return "", fmt.Errorf("fakerun: mount: need source and target")
	}
	src, dst := pos[len(pos)-2], pos[len(pos)-1]
	r.mounts[dst] = src
	return "", nil
}

func (r *Runner) umount(args []string) (string, error) {
	pos := positional(args)
	if len(pos) == 0 {
		return "", fmt.Errorf("fakerun: umount: need target")
	}
	dst := pos[len(pos)-1]
	if _, ok := r.mounts[dst]; !ok {
		return "", fmt.Errorf("umount: %s: not mounted", dst)
	}
	delete(r.mounts, dst)
	return "", nil
}

func (r *Runner) blkid(args []string) (string, error) {
	dev := args[len(args)-1]
	if r.formatted[dev] {
		return "ext4\n", nil
	}
	// Real blkid exits non-zero on an unformatted device; the backend ignores
	// the error and treats empty output as "needs formatting".
	return "", fmt.Errorf("blkid: %s: not a filesystem", dev)
}

func (r *Runner) mkfs(args []string) (string, error) {
	dev := args[len(args)-1]
	r.formatted[dev] = true
	return "", nil
}

func (r *Runner) findmnt(args []string) (string, error) {
	target := args[len(args)-1]
	dev, ok := r.mounts[target]
	if !ok {
		return "", fmt.Errorf("findmnt: %s not found", target)
	}
	// Distinguish the two fields the backend asks for.
	if contains(args, "FSTYPE") {
		return "ext4\n", nil
	}
	// For a bind mount the source is another path; resolve to the device.
	if d, ok := r.mounts[dev]; ok {
		dev = d
	}
	return dev + "\n", nil
}

// positional returns the non-flag arguments, dropping flags and their values
// for the connection/option flags the fake does not care about.
// splitSpec splits "pool/image" or "pool/namespace/image" into pool, namespace
// (empty if none), image.
func splitSpec(spec string) (pool, ns, image string) {
	parts := strings.Split(spec, "/")
	switch len(parts) {
	case 3:
		return parts[0], parts[1], parts[2]
	case 2:
		return parts[0], "", parts[1]
	default:
		return "", "", spec
	}
}

func positional(args []string) []string {
	var out []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch a {
		case "-c", "--conf", "-m", "--id", "--keyfile", "--key-file", "--type", "-t", "-o", "--size", "--namespace":
			i++ // skip this flag's value
			continue
		}
		if strings.HasPrefix(a, "-") {
			continue
		}
		out = append(out, a)
	}
	return out
}

// flagValueMiB returns the integer value following flag in args (e.g. --size).
func flagValueMiB(args []string, flag string) int64 {
	for i := 0; i < len(args)-1; i++ {
		if args[i] == flag {
			n, _ := strconv.ParseInt(args[i+1], 10, 64)
			return n
		}
	}
	return 0
}

// flagValue returns the value following flag in args (e.g. --namespace ns), or "".
func flagValue(args []string, flag string) string {
	for i := 0; i < len(args)-1; i++ {
		if args[i] == flag {
			return args[i+1]
		}
	}
	return ""
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}
