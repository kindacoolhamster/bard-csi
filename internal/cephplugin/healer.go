package cephplugin

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
)

// Heal reattaches rbd-nbd devices whose userspace map died with a previous
// node-plugin process. When the node plugin restarts, krbd maps survive (the
// kernel holds them), but every `rbd-nbd map` daemon is a child of the plugin
// container and is killed with it -- leaving /dev/nbdN with no server, so the
// mount (or dm-crypt layer) on top fails I/O. Heal scans the persisted device
// records and, for each rbd-nbd volume that is still in use but no longer has a
// live userspace map, runs `rbd-nbd attach --device` to restore it in place
// (same device path, so the mount/dm layer recovers transparently).
//
// It is self-contained -- it heals from the node's own records, with no
// Kubernetes API access, keeping the plugin cluster-agnostic. Safe to call once
// at startup: krbd records and live maps are skipped, and a record whose device
// is cleanly released (the volume was unstaged) is dropped rather than revived.
// A no-op on a krbd-only or fresh node, and on the controller (which stages
// nothing), so the rbd-nbd tooling is only invoked when there is work to do.
func (b *Backend) Heal(ctx context.Context) {
	if b.stateDir == "" {
		return
	}
	entries, err := os.ReadDir(b.stateDir)
	if err != nil {
		return // no records yet
	}
	type recAt struct {
		path string
		rec  deviceRecord
	}
	var nbd []recAt
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		path := filepath.Join(b.stateDir, e.Name())
		rec := readRecordFile(path)
		if rec.Mounter == mounterNBD && rec.Device != "" && rec.Image != "" {
			nbd = append(nbd, recAt{path, rec})
		}
	}
	if len(nbd) == 0 {
		return // nothing rbd-nbd to heal; don't touch the rbd-nbd tooling
	}
	live, err := b.mappedNBDDevices(ctx)
	if err != nil {
		log.Printf("ceph-rbd healer: list rbd-nbd maps: %v (skipping heal)", err)
		return
	}
	for _, r := range nbd {
		switch {
		case live[r.rec.Device]:
			// Healthy -- a live userspace map already backs the device.
		case !b.deviceMapped(ctx, r.rec.Device):
			// Cleanly released (size 0): the volume was unstaged but its record
			// leaked. Drop it so the healer never revives an unused image (which
			// would re-open an rbd watcher and could block DeleteVolume).
			_ = os.Remove(r.path)
		default:
			// Dead but still held by a mount/dm layer -- reattach in place.
			if err := b.reattachNBD(ctx, r.rec); err != nil {
				log.Printf("ceph-rbd healer: reattach %s/%s on %s: %v", r.rec.Pool, r.rec.Image, r.rec.Device, err)
				continue
			}
			log.Printf("ceph-rbd healer: reattached %s/%s on %s", r.rec.Pool, r.rec.Image, r.rec.Device)
		}
	}
}

// readRecordFile reads a deviceRecord from an absolute path (zero value on any
// error). Legacy bare-device-path records decode to a zero record, so they are
// simply skipped by Heal -- they predate the fields a reattach needs.
func readRecordFile(path string) deviceRecord {
	data, err := os.ReadFile(path)
	if err != nil {
		return deviceRecord{}
	}
	var rec deviceRecord
	if json.Unmarshal(data, &rec) == nil {
		return rec
	}
	return deviceRecord{}
}

// mappedNBDDevices returns the set of /dev/nbdN devices that currently have a
// live rbd-nbd userspace map. `rbd-nbd list-mapped` reads local kernel state, so
// it needs no cluster connection.
func (b *Backend) mappedNBDDevices(ctx context.Context) (map[string]bool, error) {
	out, err := b.run.Run(ctx, "rbd-nbd", "list-mapped", "--format", "json")
	if err != nil {
		return nil, err
	}
	set := map[string]bool{}
	if strings.TrimSpace(out) == "" {
		return set, nil
	}
	var maps []struct {
		Device string `json:"device"`
	}
	if err := json.Unmarshal([]byte(out), &maps); err != nil {
		return nil, fmt.Errorf("parse rbd-nbd list-mapped: %w", err)
	}
	for _, m := range maps {
		if m.Device != "" {
			set[m.Device] = true
		}
	}
	return set, nil
}

// reattachNBD reconnects a userspace rbd-nbd server to an existing nbd device,
// restoring I/O without changing the device path. Credentials come from the
// plugin's per-instance key dir (no CSI secret at startup).
func (b *Backend) reattachNBD(ctx context.Context, rec deviceRecord) error {
	cc, err := b.cluster(rec.Instance)
	if err != nil {
		return err
	}
	conn, cleanup, err := b.connArgs(cc, rec.Instance, nil)
	if err != nil {
		return err
	}
	defer cleanup()
	spec := rec.Pool + "/" + rec.Image
	// Reattach over netlink with the cookie the device was mapped with; both are
	// required for `attach` to bind to the existing nbd device.
	args := appendArgs(conn, "attach", "--device", rec.Device, "--cookie", rec.Cookie, "--try-netlink", spec)
	_, err = b.run.Run(ctx, "rbd-nbd", args...)
	return err
}
