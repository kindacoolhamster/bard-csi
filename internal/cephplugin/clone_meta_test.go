package cephplugin

import (
	"context"
	"errors"
	"testing"

	"github.com/kindacoolhamster/bard-csi/internal/fakerun"
	"github.com/kindacoolhamster/bard-csi/pkg/bardplugin"
)

// librbd copies a parent's image-meta onto every clone (`rbd clone` and `rbd cp`,
// live-verified on Ceph 20.2). Keys that describe the SOURCE must not survive on
// the new volume: bard.static would make the clone's DeleteVolume a silent no-op
// (leaking the image and its key), the owner labels would name the source's PVC,
// and snapshot-qualified keys name snapshots the clone does not have.

func newCloneMetaBackend() (*Backend, *fakerun.Runner) {
	run := fakerun.New()
	return New(map[string]ClusterConfig{"east": {Monitors: []string{"10.0.0.10:6789"}, Pool: "replicapool", UserID: "admin"}}, "", "", run), run
}

func metaList(t *testing.T, run *fakerun.Runner, spec string) string {
	t.Helper()
	out, err := run.Run(context.Background(), "rbd", "image-meta", "list", spec, "--format", "json")
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func setMeta(t *testing.T, run *fakerun.Runner, spec, key, value string) {
	t.Helper()
	if _, err := run.Run(context.Background(), "rbd", "image-meta", "set", spec, key, value); err != nil {
		t.Fatal(err)
	}
}

func TestCloneDoesNotInheritSourceOnlyMeta(t *testing.T) {
	for _, from := range []string{"snapshot", "volume"} {
		t.Run(from, func(t *testing.T) {
			ctx := context.Background()
			b, run := newCloneMetaBackend()
			params := map[string]string{"pool": "replicapool"}
			src, err := b.CreateVolume(ctx, &bardplugin.CreateVolumeRequest{Name: "src", CapacityBytes: 1 << 30, Instance: "east", Parameters: params})
			if err != nil {
				t.Fatal(err)
			}
			srcRef := bardplugin.VolumeRef{Instance: "east", Location: src.Location, Name: src.Name}
			srcSpec := src.Location + "/" + src.Name
			// An admin-owned (static) source, labelled with its PVC, carrying a
			// snapshot-qualified key.
			setMeta(t, run, srcSpec, imgMetaStatic, "true")
			setMeta(t, run, srcSpec, "bard.pvname", "pv-src")
			setMeta(t, run, srcSpec, "bard.encryptionKMSID@csi-snap-old", "k8s")

			req := &bardplugin.CreateVolumeRequest{Name: "copy", CapacityBytes: 1 << 30, Instance: "east", Parameters: params}
			if from == "snapshot" {
				snap, err := b.CreateSnapshot(ctx, &bardplugin.CreateSnapshotRequest{Name: "s", SourceVolume: srcRef})
				if err != nil {
					t.Fatal(err)
				}
				req.SourceSnapshot = snapRef(snap)
			} else {
				req.SourceVolume = &srcRef
			}
			cp, err := b.CreateVolume(ctx, req)
			if err != nil {
				t.Fatal(err)
			}
			cpSpec := cp.Location + "/" + cp.Name
			for _, k := range []string{imgMetaStatic, "bard.pvname", "bard.encryptionKMSID@csi-snap-old"} {
				if v := b.imageMeta(ctx, run, cpSpec, k); v != "" {
					t.Errorf("clone inherited %s=%q from its source; meta: %s", k, v, metaList(t, run, cpSpec))
				}
			}
			if got, want := b.imageMeta(ctx, run, cpSpec, imgMetaSource), sourceDesc(req); got != want {
				t.Errorf("bard.source = %q, want %q", got, want)
			}
			// The decisive consequence: the clone is CSI-owned, so deleting it reaps it.
			if err := b.DeleteVolume(ctx, &bardplugin.DeleteVolumeRequest{Volume: bardplugin.VolumeRef{Instance: "east", Location: cp.Location, Name: cp.Name}}); err != nil {
				t.Fatal(err)
			}
			if _, err := run.Run(ctx, "rbd", "info", cpSpec); err == nil {
				t.Fatalf("DeleteVolume of a clone of a static image left the image behind")
			}
			// ...while the static source itself is still protected.
			if v := b.imageMeta(ctx, run, srcSpec, imgMetaStatic); v != "true" {
				t.Fatalf("source lost its static mark: %q", v)
			}
		})
	}
}

// A create that died between `rbd clone` and the bard.source stamp leaves a clone
// carrying its PARENT's inherited stamp (the parent being itself a restore). The
// retry must recognise that stamp as inherited -- not as a conflicting content
// source -- or it fails AlreadyExists forever.
func TestResumedCloneIgnoresInheritedSourceStamp(t *testing.T) {
	ctx := context.Background()
	b, run := newCloneMetaBackend()
	params := map[string]string{"pool": "replicapool"}
	root, err := b.CreateVolume(ctx, &bardplugin.CreateVolumeRequest{Name: "root", CapacityBytes: 1 << 30, Instance: "east", Parameters: params})
	if err != nil {
		t.Fatal(err)
	}
	rootRef := bardplugin.VolumeRef{Instance: "east", Location: root.Location, Name: root.Name}
	s1, err := b.CreateSnapshot(ctx, &bardplugin.CreateSnapshotRequest{Name: "s1", SourceVolume: rootRef})
	if err != nil {
		t.Fatal(err)
	}
	// mid is itself a restore, so it carries bard.source = snap:<root@s1>.
	mid, err := b.CreateVolume(ctx, &bardplugin.CreateVolumeRequest{Name: "mid", CapacityBytes: 1 << 30, Instance: "east", Parameters: params, SourceSnapshot: snapRef(s1)})
	if err != nil {
		t.Fatal(err)
	}
	midRef := bardplugin.VolumeRef{Instance: "east", Location: mid.Location, Name: mid.Name}
	s2, err := b.CreateSnapshot(ctx, &bardplugin.CreateSnapshotRequest{Name: "s2", SourceVolume: midRef})
	if err != nil {
		t.Fatal(err)
	}

	req := &bardplugin.CreateVolumeRequest{Name: "leaf", CapacityBytes: 1 << 30, Instance: "east", Parameters: params, SourceSnapshot: snapRef(s2)}
	// First attempt dies right after the clone: run only the clone itself.
	leafSpec := "replicapool/" + shortName(volNamePrefix, req.Name)
	if _, err := run.Run(ctx, "rbd", "clone", s2.Location+"/"+s2.Name, leafSpec, "--rbd-default-clone-format", "2"); err != nil {
		t.Fatal(err)
	}
	if got := b.imageMeta(ctx, run, leafSpec, imgMetaSource); got == "" || got == sourceDesc(req) {
		t.Fatalf("test setup: expected the clone to carry its parent's stamp, got %q", got)
	}

	if _, err := b.CreateVolume(ctx, req); err != nil {
		t.Fatalf("retry of an interrupted restore failed: %v", err)
	}
	if got, want := b.imageMeta(ctx, run, leafSpec, imgMetaSource), sourceDesc(req); got != want {
		t.Fatalf("bard.source = %q after the retry, want %q", got, want)
	}
	// A retry naming a genuinely different source is still a conflict.
	other := &bardplugin.CreateVolumeRequest{Name: "leaf", CapacityBytes: 1 << 30, Instance: "east", Parameters: params, SourceSnapshot: snapRef(s1)}
	var se *bardplugin.StatusError
	if _, err := b.CreateVolume(ctx, other); !errors.As(err, &se) || se.Code != bardplugin.CodeAlreadyExists {
		t.Fatalf("different content source must be AlreadyExists, got %v", err)
	}
}

// A matching stamp alone is not proof of inheritance: a volume genuinely cloned
// from root@s1, retried as a restore of mid@s2 (mid itself restored from root@s1,
// so mid's stamp equals the volume's real one), has the wrong content and must be
// AlreadyExists, not an idempotent hit.
func TestRetryWithDifferentSourceSharingStampIsAlreadyExists(t *testing.T) {
	ctx := context.Background()
	b, _ := newCloneMetaBackend()
	params := map[string]string{"pool": "replicapool"}
	root, err := b.CreateVolume(ctx, &bardplugin.CreateVolumeRequest{Name: "root", CapacityBytes: 1 << 30, Instance: "east", Parameters: params})
	if err != nil {
		t.Fatal(err)
	}
	s1, err := b.CreateSnapshot(ctx, &bardplugin.CreateSnapshotRequest{Name: "s1", SourceVolume: bardplugin.VolumeRef{Instance: "east", Location: root.Location, Name: root.Name}})
	if err != nil {
		t.Fatal(err)
	}
	mid, err := b.CreateVolume(ctx, &bardplugin.CreateVolumeRequest{Name: "mid", CapacityBytes: 1 << 30, Instance: "east", Parameters: params, SourceSnapshot: snapRef(s1)})
	if err != nil {
		t.Fatal(err)
	}
	s2, err := b.CreateSnapshot(ctx, &bardplugin.CreateSnapshotRequest{Name: "s2", SourceVolume: bardplugin.VolumeRef{Instance: "east", Location: mid.Location, Name: mid.Name}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.CreateVolume(ctx, &bardplugin.CreateVolumeRequest{Name: "leaf", CapacityBytes: 1 << 30, Instance: "east", Parameters: params, SourceSnapshot: snapRef(s1)}); err != nil {
		t.Fatal(err)
	}
	var se *bardplugin.StatusError
	_, err = b.CreateVolume(ctx, &bardplugin.CreateVolumeRequest{Name: "leaf", CapacityBytes: 1 << 30, Instance: "east", Parameters: params, SourceSnapshot: snapRef(s2)})
	if !errors.As(err, &se) || se.Code != bardplugin.CodeAlreadyExists {
		t.Fatalf("retry naming a different source must be AlreadyExists, got %v", err)
	}
}

// The parent IMAGE matching is not enough either: a clone of mid@s2 whose create
// died before its stamp (so it carries mid's inherited stamp), retried as a
// restore of mid@s3, has s2's data. The COW link's parent SNAPSHOT decides. And an
// unstamped clone (died before the stamp, unstamped parent) retried with another
// source is likewise a conflict, not an idempotent hit.
func TestResumedCloneChecksParentSnapshot(t *testing.T) {
	for _, stamped := range []bool{true, false} {
		name := map[bool]string{true: "inherited-stamp", false: "unstamped"}[stamped]
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			b, run := newCloneMetaBackend()
			params := map[string]string{"pool": "replicapool"}
			src := "root"
			if stamped {
				// mid is itself a restore, so its stamp is copied onto its clones.
				root, err := b.CreateVolume(ctx, &bardplugin.CreateVolumeRequest{Name: "root", CapacityBytes: 1 << 30, Instance: "east", Parameters: params})
				if err != nil {
					t.Fatal(err)
				}
				s1, err := b.CreateSnapshot(ctx, &bardplugin.CreateSnapshotRequest{Name: "s1", SourceVolume: bardplugin.VolumeRef{Instance: "east", Location: root.Location, Name: root.Name}})
				if err != nil {
					t.Fatal(err)
				}
				if _, err := b.CreateVolume(ctx, &bardplugin.CreateVolumeRequest{Name: "mid", CapacityBytes: 1 << 30, Instance: "east", Parameters: params, SourceSnapshot: snapRef(s1)}); err != nil {
					t.Fatal(err)
				}
				src = "mid"
			} else if _, err := b.CreateVolume(ctx, &bardplugin.CreateVolumeRequest{Name: src, CapacityBytes: 1 << 30, Instance: "east", Parameters: params}); err != nil {
				t.Fatal(err)
			}
			srcRef := bardplugin.VolumeRef{Instance: "east", Location: "replicapool", Name: shortName(volNamePrefix, src)}
			s2, err := b.CreateSnapshot(ctx, &bardplugin.CreateSnapshotRequest{Name: "s2", SourceVolume: srcRef})
			if err != nil {
				t.Fatal(err)
			}
			s3, err := b.CreateSnapshot(ctx, &bardplugin.CreateSnapshotRequest{Name: "s3", SourceVolume: srcRef})
			if err != nil {
				t.Fatal(err)
			}
			// First attempt from s2 dies right after the clone.
			leafSpec := "replicapool/" + shortName(volNamePrefix, "leaf")
			if _, err := run.Run(ctx, "rbd", "clone", s2.Location+"/"+s2.Name, leafSpec, "--rbd-default-clone-format", "2"); err != nil {
				t.Fatal(err)
			}
			var se *bardplugin.StatusError
			_, err = b.CreateVolume(ctx, &bardplugin.CreateVolumeRequest{Name: "leaf", CapacityBytes: 1 << 30, Instance: "east", Parameters: params, SourceSnapshot: snapRef(s3)})
			if !errors.As(err, &se) || se.Code != bardplugin.CodeAlreadyExists {
				t.Fatalf("a clone of s2 retried as a restore of s3 must be AlreadyExists, got %v", err)
			}
			// ...while the genuine retry (from s2) resumes.
			if _, err := b.CreateVolume(ctx, &bardplugin.CreateVolumeRequest{Name: "leaf", CapacityBytes: 1 << 30, Instance: "east", Parameters: params, SourceSnapshot: snapRef(s2)}); err != nil {
				t.Fatalf("genuine retry from s2: %v", err)
			}
		})
	}
}

// imageMeta reads one key straight from the fake (test helper).
func (b *Backend) imageMeta(ctx context.Context, run *fakerun.Runner, spec, key string) string {
	v, _ := run.Run(ctx, "rbd", "image-meta", "get", spec, key)
	return v
}

func snapRef(r *bardplugin.CreateSnapshotResponse) *bardplugin.VolumeRef {
	return &bardplugin.VolumeRef{Instance: "east", Location: r.Location, Name: r.Name}
}
