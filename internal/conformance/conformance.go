// Package conformance drives a Bard backend plugin over its unix socket, the
// way Bard core would, and verifies it honors the wire contract
// (pkg/bardplugin): the required semantics every plugin must implement
// (idempotency, error codes, identity rules) plus every optional capability
// the plugin declares. It is the acceptance bar for a third-party plugin: one
// that passes is indistinguishable from a first-party backend as far as core
// can see.
//
// The checks create real resources on the backend (volumes, snapshots) under a
// unique name prefix and delete them again; run it against a disposable
// instance. Control-plane checks need no privileges beyond what the plugin
// itself needs; node-plane checks (Config.Node) mount real filesystems and
// normally need root.
package conformance

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/kindacoolhamster/bard-csi/internal/volumeid"
	"github.com/kindacoolhamster/bard-csi/pkg/bardplugin"
)

// Config selects the plugin and the backend-specific inputs the checks need.
type Config struct {
	Socket        string            // plugin unix socket path (required)
	Instance      string            // instance id present in the plugin's own config (required)
	Parameters    map[string]string // StorageClass-style parameters for create/capacity
	FsType        string            // fsType for created volumes ("" = plugin default)
	CapacityBytes int64             // requested size of test volumes (default 16 MiB)
	NamePrefix    string            // prefix for created names (default "conf-<random>")
	Node          bool              // also drive the node plane (stage/publish/mount; usually needs root)
	// NodeID is the CSI node id the attach checks publish to. For an attach
	// backend (RequiresControllerPublish) whose node plane derives its identity
	// from the plugin's own node id (e.g. the iSCSI initiator IQN), pass the
	// SAME value the plugin under test was started with, or the node-plane login
	// will be rejected. Default "conformance-node".
	NodeID     string
	StagingDir string        // where staging/target dirs are made (default: a temp dir)
	OpTimeout  time.Duration // per-operation timeout (default 2m)
	Logf       func(format string, args ...any)
}

// Status is a check outcome. Fail means the plugin violates the contract; Warn
// marks a SHOULD the plugin doesn't honor; Skip covers undeclared capabilities
// and checks whose prerequisite failed.
type Status string

const (
	Pass Status = "PASS"
	Fail Status = "FAIL"
	Warn Status = "WARN"
	Skip Status = "SKIP"
)

// Result is one check's outcome.
type Result struct {
	Name   string
	Status Status
	Detail string
}

// Failed reports whether any result is a Fail.
func Failed(rs []Result) bool {
	for _, r := range rs {
		if r.Status == Fail {
			return true
		}
	}
	return false
}

// PluginError is a structured plugin error (a non-200 response with an Error
// body), preserved so checks can assert on the code.
type PluginError struct {
	Code       bardplugin.ErrorCode
	HTTPStatus int
	Message    string
}

func (e *PluginError) Error() string {
	return fmt.Sprintf("%s (HTTP %d): %s", e.Code, e.HTTPStatus, e.Message)
}

// errCode extracts the plugin error code, or "" for transport/other errors.
func errCode(err error) bardplugin.ErrorCode {
	var pe *PluginError
	if errors.As(err, &pe) {
		return pe.Code
	}
	return ""
}

// Run executes the conformance checks and returns one Result per check. A
// non-nil error means the harness could not run at all (e.g. the socket never
// answered /info) -- distinct from the plugin failing checks.
func Run(ctx context.Context, cfg Config) ([]Result, error) {
	if cfg.Socket == "" || cfg.Instance == "" {
		return nil, errors.New("conformance: Socket and Instance are required")
	}
	if cfg.CapacityBytes <= 0 {
		cfg.CapacityBytes = 16 << 20
	}
	if cfg.OpTimeout <= 0 {
		cfg.OpTimeout = 2 * time.Minute
	}
	if cfg.NamePrefix == "" {
		b := make([]byte, 3)
		if _, err := rand.Read(b); err != nil {
			return nil, fmt.Errorf("conformance: rand: %w", err)
		}
		cfg.NamePrefix = "conf-" + hex.EncodeToString(b)
	}
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	r := &runner{
		cfg: cfg,
		hc: &http.Client{Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", cfg.Socket)
			},
		}},
	}
	if err := r.run(ctx); err != nil {
		return nil, err
	}
	return r.res, nil
}

type runner struct {
	cfg Config
	hc  *http.Client
	res []Result

	// live resources for the final best-effort cleanup, newest first.
	liveVols  []bardplugin.VolumeRef
	liveSnaps []bardplugin.VolumeRef
}

func (r *runner) record(name string, st Status, format string, args ...any) {
	res := Result{Name: name, Status: st, Detail: fmt.Sprintf(format, args...)}
	r.res = append(r.res, res)
	r.cfg.Logf("%-4s %-28s %s", res.Status, res.Name, res.Detail)
}

// call POSTs req as JSON and decodes the response into out (may be nil).
func (r *runner) call(ctx context.Context, path string, req, out any) error {
	body, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	return r.callRaw(ctx, path, body, out)
}

func (r *runner) callRaw(ctx context.Context, path string, body []byte, out any) error {
	cctx, cancel := context.WithTimeout(ctx, r.cfg.OpTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodPost, "http://plugin"+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := r.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		var e bardplugin.Error
		_ = json.Unmarshal(data, &e)
		if e.Message == "" {
			e.Message = fmt.Sprintf("HTTP %d", resp.StatusCode)
		}
		return &PluginError{Code: e.Code, HTTPStatus: resp.StatusCode, Message: e.Message}
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("decode response: %w", err)
		}
	}
	return nil
}

// identity validates a plugin-chosen (location, name) the way core does before
// encoding it into a CSI volume id.
func (r *runner) identity(backendType, location, name string) error {
	if name == "" {
		return errors.New("empty name")
	}
	h := volumeid.Handle{Backend: backendType, Instance: r.cfg.Instance, Location: location, Name: name}
	return h.Validate()
}

func (r *runner) volRef(location, name string) bardplugin.VolumeRef {
	return bardplugin.VolumeRef{Instance: r.cfg.Instance, Location: location, Name: name}
}

func (r *runner) trackVol(ref bardplugin.VolumeRef)   { r.liveVols = append(r.liveVols, ref) }
func (r *runner) trackSnap(ref bardplugin.VolumeRef)  { r.liveSnaps = append(r.liveSnaps, ref) }
func (r *runner) untrackVol(ref bardplugin.VolumeRef) { r.liveVols = removeRef(r.liveVols, ref) }
func (r *runner) untrackSnap(ref bardplugin.VolumeRef) {
	r.liveSnaps = removeRef(r.liveSnaps, ref)
}

func removeRef(refs []bardplugin.VolumeRef, ref bardplugin.VolumeRef) []bardplugin.VolumeRef {
	out := refs[:0]
	for _, x := range refs {
		if x != ref {
			out = append(out, x)
		}
	}
	return out
}

func (r *runner) deleteVol(ctx context.Context, ref bardplugin.VolumeRef) error {
	err := r.call(ctx, bardplugin.PathDeleteVolume, bardplugin.DeleteVolumeRequest{Volume: ref}, nil)
	if err == nil {
		r.untrackVol(ref)
	}
	return err
}

func (r *runner) deleteSnap(ctx context.Context, ref bardplugin.VolumeRef) error {
	err := r.call(ctx, bardplugin.PathDeleteSnapshot, bardplugin.DeleteSnapshotRequest{Snapshot: ref}, nil)
	if err == nil {
		r.untrackSnap(ref)
	}
	return err
}

func (r *runner) createReq(name string) bardplugin.CreateVolumeRequest {
	return bardplugin.CreateVolumeRequest{
		Name:          name,
		CapacityBytes: r.cfg.CapacityBytes,
		Instance:      r.cfg.Instance,
		FsType:        r.cfg.FsType,
		Parameters:    r.cfg.Parameters,
	}
}

func (r *runner) run(ctx context.Context) error {
	// ---- /info (with a short retry so the plugin can come up) -------------
	var info bardplugin.Info
	deadline := time.Now().Add(10 * time.Second)
	for {
		err := r.call(ctx, bardplugin.PathInfo, struct{}{}, &info)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("conformance: /info never answered: %w", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	defer r.cleanup(ctx)

	caps := info.Capabilities
	major, minor, verr := bardplugin.ParseContractVersion(info.ContractVersion)
	switch {
	case info.Type == "":
		r.record("info", Fail, "type must be non-empty")
	case verr != nil:
		r.record("info", Fail, "contractVersion: %v", verr)
	case major != bardplugin.ContractMajor:
		r.record("info", Fail, "contract major %d not supported (core speaks v%d.x)", major, bardplugin.ContractMajor)
	default:
		r.record("info", Pass, "type=%s contract=%d.%d", info.Type, major, minor)
	}

	var info2 bardplugin.Info
	if err := r.call(ctx, bardplugin.PathInfo, struct{}{}, &info2); err != nil {
		r.record("info/stable", Fail, "second /info failed: %v", err)
	} else if info2 != info {
		r.record("info/stable", Fail, "/info changed between calls: %+v vs %+v", info, info2)
	} else {
		r.record("info/stable", Pass, "identical on repeat")
	}

	// ---- create + required create semantics --------------------------------
	v1name := r.cfg.NamePrefix + "-v1"
	var v1 bardplugin.CreateVolumeResponse
	haveV1 := false
	if err := r.call(ctx, bardplugin.PathCreateVolume, r.createReq(v1name), &v1); err != nil {
		r.record("volume/create", Fail, "create %s: %v", v1name, err)
	} else if err := r.identity(info.Type, v1.Location, v1.Name); err != nil {
		r.record("volume/create", Fail, "returned identity unusable as a CSI id: %v", err)
	} else if v1.CapacityBytes != 0 && v1.CapacityBytes < r.cfg.CapacityBytes {
		r.record("volume/create", Fail, "capacityBytes %d < requested %d", v1.CapacityBytes, r.cfg.CapacityBytes)
	} else {
		haveV1 = true
		r.trackVol(r.volRef(v1.Location, v1.Name))
		r.record("volume/create", Pass, "name=%s location=%s cap=%d", v1.Name, v1.Location, v1.CapacityBytes)
	}
	v1ref := r.volRef(v1.Location, v1.Name)

	if !haveV1 {
		r.record("volume/create-idempotent", Skip, "prerequisite volume/create failed")
	} else {
		var again bardplugin.CreateVolumeResponse
		if err := r.call(ctx, bardplugin.PathCreateVolume, r.createReq(v1name), &again); err != nil {
			r.record("volume/create-idempotent", Fail,
				"re-create of %s failed: %v -- the external-provisioner retries CreateVolume, so an identical repeat must succeed", v1name, err)
		} else if again.Name != v1.Name || again.Location != v1.Location {
			r.record("volume/create-idempotent", Fail,
				"re-create returned a different volume: %s/%s vs %s/%s", again.Location, again.Name, v1.Location, v1.Name)
		} else {
			r.record("volume/create-idempotent", Pass, "identical repeat returns the same volume")
		}

		conflict := r.createReq(v1name)
		conflict.CapacityBytes = r.cfg.CapacityBytes * 2
		err := r.call(ctx, bardplugin.PathCreateVolume, conflict, nil)
		switch {
		case errCode(err) == bardplugin.CodeAlreadyExists:
			r.record("volume/create-conflict", Pass, "same name + incompatible size => AlreadyExists")
		case err == nil:
			r.record("volume/create-conflict", Warn,
				"same name + incompatible size succeeded; SHOULD be AlreadyExists (acceptable only for backends with no recorded size)")
		default:
			r.record("volume/create-conflict", Fail, "want AlreadyExists, got: %v", err)
		}
	}

	// Unknown-field tolerance: the compatibility promise adds fields within a
	// major, so a plugin must ignore JSON fields it does not know.
	v2name := r.cfg.NamePrefix + "-v2"
	rawReq, _ := json.Marshal(r.createReq(v2name))
	var asMap map[string]any
	_ = json.Unmarshal(rawReq, &asMap)
	asMap["xConformanceUnknownField"] = "future contract minors add fields; ignore me"
	rawReq, _ = json.Marshal(asMap)
	var v2 bardplugin.CreateVolumeResponse
	if err := r.callRaw(ctx, bardplugin.PathCreateVolume, rawReq, &v2); err != nil {
		r.record("volume/unknown-field", Fail,
			"create with an extra unknown field failed: %v -- plugins must ignore unknown JSON fields (forward compatibility)", err)
	} else {
		r.record("volume/unknown-field", Pass, "unknown request field ignored")
		v2ref := r.volRef(v2.Location, v2.Name)
		r.trackVol(v2ref)
		if err := r.deleteVol(ctx, v2ref); err != nil {
			r.record("volume/unknown-field", Warn, "cleanup delete of %s failed: %v", v2.Name, err)
		}
	}

	// ---- optional control-plane capabilities -------------------------------
	if !caps.Expand {
		r.record("volume/expand", Skip, "capability not declared")
	} else if !haveV1 {
		r.record("volume/expand", Skip, "prerequisite volume/create failed")
	} else {
		newSize := r.cfg.CapacityBytes * 2
		var er bardplugin.ExpandVolumeResponse
		if err := r.call(ctx, bardplugin.PathExpandVolume, bardplugin.ExpandVolumeRequest{Volume: v1ref, NewSizeBytes: newSize}, &er); err != nil {
			r.record("volume/expand", Fail, "expand to %d: %v", newSize, err)
		} else if er.CapacityBytes < newSize {
			r.record("volume/expand", Fail, "capacityBytes %d < requested %d", er.CapacityBytes, newSize)
		} else if err := r.call(ctx, bardplugin.PathExpandVolume, bardplugin.ExpandVolumeRequest{Volume: v1ref, NewSizeBytes: newSize}, &er); err != nil {
			r.record("volume/expand", Fail, "repeated expand to the same size must succeed (kubelet/resizer retry): %v", err)
		} else {
			r.record("volume/expand", Pass, "grew to %d, idempotent on repeat (nodeExpansionRequired=%v)", er.CapacityBytes, er.NodeExpansionRequired)
		}
	}

	if !caps.GetCapacity {
		r.record("capacity", Skip, "capability not declared")
	} else {
		var cr bardplugin.GetCapacityResponse
		if err := r.call(ctx, bardplugin.PathGetCapacity, bardplugin.GetCapacityRequest{Instance: r.cfg.Instance, Parameters: r.cfg.Parameters}, &cr); err != nil {
			r.record("capacity", Fail, "%v", err)
		} else if cr.AvailableBytes < 0 {
			r.record("capacity", Fail, "negative availableBytes %d", cr.AvailableBytes)
		} else {
			r.record("capacity", Pass, "availableBytes=%d", cr.AvailableBytes)
		}
	}

	if !caps.VolumeHealth {
		r.record("volume/health", Skip, "capability not declared")
	} else if !haveV1 {
		r.record("volume/health", Skip, "prerequisite volume/create failed")
	} else {
		var hr bardplugin.GetVolumeHealthResponse
		if err := r.call(ctx, bardplugin.PathVolumeHealth, bardplugin.GetVolumeHealthRequest{Volume: v1ref}, &hr); err != nil {
			r.record("volume/health", Fail, "%v", err)
		} else if hr.Abnormal {
			r.record("volume/health", Fail, "fresh volume reported abnormal: %s", hr.Message)
		} else {
			r.record("volume/health", Pass, "fresh volume healthy")
		}
	}

	if !caps.ListVolumes {
		r.record("volume/list", Skip, "capability not declared")
	} else if !haveV1 {
		r.record("volume/list", Skip, "prerequisite volume/create failed")
	} else {
		var lr bardplugin.ListVolumesResponse
		if err := r.call(ctx, bardplugin.PathListVolumes, bardplugin.ListVolumesRequest{}, &lr); err != nil {
			r.record("volume/list", Fail, "%v", err)
		} else if !containsVol(lr.Entries, v1ref) {
			r.record("volume/list", Fail, "created volume %s/%s missing from %d entries", v1ref.Location, v1ref.Name, len(lr.Entries))
		} else {
			r.record("volume/list", Pass, "created volume listed (%d entries)", len(lr.Entries))
		}
	}

	// ---- snapshots ----------------------------------------------------------
	s1name := r.cfg.NamePrefix + "-s1"
	var s1 bardplugin.CreateSnapshotResponse
	haveS1 := false
	if !caps.Snapshots {
		r.record("snapshot/create", Skip, "capability not declared")
	} else if !haveV1 {
		r.record("snapshot/create", Skip, "prerequisite volume/create failed")
	} else {
		snapReq := bardplugin.CreateSnapshotRequest{Name: s1name, SourceVolume: v1ref, Parameters: r.cfg.Parameters}
		if err := r.call(ctx, bardplugin.PathCreateSnapshot, snapReq, &s1); err != nil {
			r.record("snapshot/create", Fail, "%v", err)
		} else if err := r.identity(info.Type, s1.Location, s1.Name); err != nil {
			r.record("snapshot/create", Fail, "returned identity unusable as a CSI id: %v", err)
		} else {
			haveS1 = true
			r.trackSnap(r.volRef(s1.Location, s1.Name))
			r.record("snapshot/create", Pass, "name=%s location=%s readyToUse=%v", s1.Name, s1.Location, s1.ReadyToUse)

			var again bardplugin.CreateSnapshotResponse
			if err := r.call(ctx, bardplugin.PathCreateSnapshot, snapReq, &again); err != nil {
				r.record("snapshot/create-idempotent", Fail, "identical repeat must succeed (snapshotter retries): %v", err)
			} else if again.Name != s1.Name || again.Location != s1.Location {
				r.record("snapshot/create-idempotent", Fail, "repeat returned a different snapshot: %s/%s", again.Location, again.Name)
			} else {
				r.record("snapshot/create-idempotent", Pass, "identical repeat returns the same snapshot")
			}
		}
	}
	s1ref := r.volRef(s1.Location, s1.Name)

	if !caps.ListSnapshots {
		r.record("snapshot/list", Skip, "capability not declared")
	} else if !haveS1 {
		r.record("snapshot/list", Skip, "prerequisite snapshot/create failed or skipped")
	} else {
		var lr bardplugin.ListSnapshotsResponse
		if err := r.call(ctx, bardplugin.PathListSnapshots, bardplugin.ListSnapshotsRequest{}, &lr); err != nil {
			r.record("snapshot/list", Fail, "%v", err)
		} else if e := findSnap(lr.Entries, s1ref); e == nil {
			r.record("snapshot/list", Fail, "created snapshot %s/%s missing from %d entries", s1ref.Location, s1ref.Name, len(lr.Entries))
		} else if e.SourceVolume != v1ref {
			r.record("snapshot/list", Fail, "listed source %+v does not match created source %+v", e.SourceVolume, v1ref)
		} else {
			r.record("snapshot/list", Pass, "created snapshot listed with correct source (%d entries)", len(lr.Entries))
		}
	}

	if !caps.Snapshots {
		r.record("snapshot/restore", Skip, "capability not declared")
	} else if !haveS1 {
		r.record("snapshot/restore", Skip, "prerequisite snapshot/create failed or skipped")
	} else {
		restore := r.createReq(r.cfg.NamePrefix + "-v3")
		restore.SourceSnapshot = &s1ref
		var v3 bardplugin.CreateVolumeResponse
		if err := r.call(ctx, bardplugin.PathCreateVolume, restore, &v3); err != nil {
			r.record("snapshot/restore", Fail, "create from sourceSnapshot: %v", err)
		} else if err := r.identity(info.Type, v3.Location, v3.Name); err != nil {
			r.record("snapshot/restore", Fail, "returned identity unusable as a CSI id: %v", err)
		} else {
			r.trackVol(r.volRef(v3.Location, v3.Name))
			r.record("snapshot/restore", Pass, "restored to %s/%s", v3.Location, v3.Name)
		}
	}

	if !haveV1 {
		r.record("volume/clone", Skip, "prerequisite volume/create failed")
	} else {
		clone := r.createReq(r.cfg.NamePrefix + "-v4")
		clone.SourceVolume = &v1ref
		var v4 bardplugin.CreateVolumeResponse
		err := r.call(ctx, bardplugin.PathCreateVolume, clone, &v4)
		switch {
		case err == nil:
			r.trackVol(r.volRef(v4.Location, v4.Name))
			r.record("volume/clone", Pass, "cloned to %s/%s", v4.Location, v4.Name)
		case errCode(err) == bardplugin.CodeInvalidArg:
			// There is no capability flag for clone; rejecting it with
			// InvalidArgument is the contract's way of saying "unsupported".
			r.record("volume/clone", Skip, "not supported (InvalidArgument): %v", err)
		default:
			r.record("volume/clone", Fail, "unsupported clone must be InvalidArgument, got: %v", err)
		}
	}

	if !caps.ReclaimSpace {
		r.record("volume/reclaimspace", Skip, "capability not declared")
	} else if !haveV1 {
		r.record("volume/reclaimspace", Skip, "prerequisite volume/create failed")
	} else if err := r.call(ctx, bardplugin.PathReclaimSpace, bardplugin.ReclaimSpaceRequest{Volume: v1ref}, nil); err != nil {
		r.record("volume/reclaimspace", Fail, "%v", err)
	} else {
		r.record("volume/reclaimspace", Pass, "reclaim on a fresh volume succeeds")
	}

	// ---- controller attach (ControllerPublish/Unpublish) --------------------
	// For an attach backend the publish MUST precede the node plane: without it
	// the node's access is not provisioned (e.g. no ACL for its initiator), so a
	// stage attempt is correctly rejected by the backend.
	nodeID := r.cfg.NodeID
	if nodeID == "" {
		nodeID = "conformance-node"
	}
	var pubCtx map[string]string
	published := false
	if caps.RequiresControllerPublish {
		if !haveV1 {
			r.record("controller/publish", Skip, "prerequisite volume/create failed")
		} else {
			pubReq := bardplugin.ControllerPublishRequest{Volume: v1ref, NodeID: nodeID}
			var resp bardplugin.ControllerPublishResponse
			if err := r.call(ctx, bardplugin.PathControllerPublish, pubReq, &resp); err != nil {
				r.record("controller/publish", Fail, "%v", err)
			} else {
				published = true
				pubCtx = resp.PublishContext
				if err := r.call(ctx, bardplugin.PathControllerPublish, pubReq, &resp); err != nil {
					r.record("controller/publish", Fail, "repeated publish must succeed (the attacher retries): %v", err)
				} else {
					r.record("controller/publish", Pass, "published to node %q (%d publishContext keys), idempotent on repeat", nodeID, len(pubCtx))
				}
			}
		}
	}

	// ---- node plane ---------------------------------------------------------
	if !r.cfg.Node {
		r.record("node", Skip, "node-plane checks disabled (enable with -node; usually needs root)")
	} else if !haveV1 {
		r.record("node", Skip, "prerequisite volume/create failed")
	} else if caps.RequiresControllerPublish && !published {
		r.record("node", Skip, "prerequisite controller/publish failed")
	} else {
		r.nodeChecks(ctx, caps, v1ref, pubCtx)
	}

	// ---- controller detach ---------------------------------------------------
	if caps.RequiresControllerPublish && haveV1 {
		unpubReq := bardplugin.ControllerUnpublishRequest{Volume: v1ref, NodeID: nodeID}
		if !published {
			r.record("controller/unpublish", Skip, "prerequisite controller/publish failed")
		} else if err := r.call(ctx, bardplugin.PathControllerUnpublish, unpubReq, nil); err != nil {
			r.record("controller/unpublish", Fail, "%v", err)
		} else if err := r.call(ctx, bardplugin.PathControllerUnpublish, unpubReq, nil); err != nil {
			r.record("controller/unpublish", Fail,
				"second unpublish must succeed, got: %v -- ControllerUnpublish must be idempotent (the attacher retries forever)", err)
		} else {
			r.record("controller/unpublish", Pass, "detached; repeat detach is a successful no-op")
		}
		if published {
			if err := r.call(ctx, bardplugin.PathControllerUnpublish,
				bardplugin.ControllerUnpublishRequest{Volume: v1ref, NodeID: nodeID + "-never-published"}, nil); err != nil {
				r.record("controller/unpublish-absent", Fail, "unpublishing a never-published node must succeed, got: %v", err)
			} else {
				r.record("controller/unpublish-absent", Pass, "unpublish of a never-published node is a successful no-op")
			}
		}
	}

	// ---- delete + required delete semantics ---------------------------------
	if haveS1 {
		if err := r.deleteSnap(ctx, s1ref); err != nil {
			r.record("snapshot/delete", Fail, "%v", err)
		} else if err := r.deleteSnap(ctx, s1ref); err != nil {
			r.record("snapshot/delete", Fail,
				"second delete must succeed, got: %v -- DeleteSnapshot must be idempotent (core forwards errors to Kubernetes, which retries forever)", err)
		} else {
			r.record("snapshot/delete", Pass, "deleted; repeat delete is a successful no-op")
		}
	} else {
		r.record("snapshot/delete", Skip, "no snapshot created")
	}

	if haveV1 {
		// Delete restore/clone volumes first (a backend may require children
		// gone before the parent).
		for _, ref := range append([]bardplugin.VolumeRef{}, r.liveVols...) {
			if ref != v1ref {
				if err := r.deleteVol(ctx, ref); err != nil {
					r.record("volume/delete", Warn, "cleanup delete of %s/%s failed: %v", ref.Location, ref.Name, err)
				}
			}
		}
		if err := r.deleteVol(ctx, v1ref); err != nil {
			r.record("volume/delete", Fail, "%v", err)
		} else if err := r.deleteVol(ctx, v1ref); err != nil {
			r.record("volume/delete", Fail,
				"second delete must succeed, got: %v -- DeleteVolume must be idempotent (core forwards errors to Kubernetes, which retries forever)", err)
		} else {
			r.record("volume/delete", Pass, "deleted; repeat delete is a successful no-op")
		}

		ghost := r.volRef(v1.Location, r.cfg.NamePrefix+"-never-created")
		if err := r.deleteVol(ctx, ghost); err != nil {
			r.record("volume/delete-absent", Fail, "deleting a never-created volume must succeed, got: %v", err)
		} else {
			r.record("volume/delete-absent", Pass, "deleting an absent volume is a successful no-op")
		}
	} else {
		r.record("volume/delete", Skip, "prerequisite volume/create failed")
		r.record("volume/delete-absent", Skip, "prerequisite volume/create failed")
	}

	// ---- declared capabilities this tool does not exercise ------------------
	for _, c := range []struct {
		name     string
		declared bool
	}{
		{"volume/modify", caps.ModifyVolume},
		{"networkfence", caps.NetworkFence},
		{"replication", caps.Replication},
		{"volumegroup", caps.VolumeGroup},
		{"node/rotate-key", caps.EncryptionKeyRotation},
		{"node/reclaimspace", caps.NodeReclaimSpace && !r.cfg.Node},
	} {
		if c.declared {
			r.record(c.name, Skip, "declared but not exercised by this tool version")
		}
	}
	return nil
}

// nodeChecks stages, publishes, writes through, and tears down v1 on this node.
// pubCtx is the PublishContext from a preceding ControllerPublish (attach
// backends; nil otherwise), threaded into NodeStage exactly as core does.
func (r *runner) nodeChecks(ctx context.Context, caps bardplugin.Capabilities, v1ref bardplugin.VolumeRef, pubCtx map[string]string) {
	base := r.cfg.StagingDir
	if base == "" {
		d, err := os.MkdirTemp("", "bard-conformance-")
		if err != nil {
			r.record("node/stage", Fail, "mktemp: %v", err)
			return
		}
		defer os.RemoveAll(d)
		base = d
	}
	staging := filepath.Join(base, "staging")
	target := filepath.Join(base, "target")
	for _, d := range []string{staging, target} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			r.record("node/stage", Fail, "mkdir %s: %v", d, err)
			return
		}
	}

	stageReq := bardplugin.NodeStageRequest{Volume: v1ref, StagingPath: staging, FsType: r.cfg.FsType, PublishContext: pubCtx}
	if err := r.call(ctx, bardplugin.PathNodeStage, stageReq, nil); err != nil {
		r.record("node/stage", Fail, "%v", err)
		return
	}
	staged := true
	defer func() {
		if staged {
			_ = r.call(ctx, bardplugin.PathNodeUnstage, bardplugin.NodeUnstageRequest{Volume: v1ref, StagingPath: staging}, nil)
		}
	}()
	if err := r.call(ctx, bardplugin.PathNodeStage, stageReq, nil); err != nil {
		r.record("node/stage", Fail, "repeated stage must succeed (kubelet retries): %v", err)
		return
	}
	r.record("node/stage", Pass, "staged at %s, idempotent on repeat", staging)

	pubReq := bardplugin.NodePublishRequest{Volume: v1ref, StagingPath: staging, TargetPath: target, FsType: r.cfg.FsType}
	if err := r.call(ctx, bardplugin.PathNodePublish, pubReq, nil); err != nil {
		r.record("node/publish", Fail, "%v", err)
		return
	}
	published := true
	defer func() {
		if published {
			_ = r.call(ctx, bardplugin.PathNodeUnpublish, bardplugin.NodeUnpublishRequest{Volume: v1ref, TargetPath: target}, nil)
		}
	}()
	if err := r.call(ctx, bardplugin.PathNodePublish, pubReq, nil); err != nil {
		r.record("node/publish", Fail, "repeated publish must succeed (kubelet retries): %v", err)
		return
	}
	r.record("node/publish", Pass, "published at %s, idempotent on repeat", target)

	proof := make([]byte, 32)
	_, _ = rand.Read(proof)
	proofPath := filepath.Join(target, "bard-conformance-proof")
	if err := os.WriteFile(proofPath, proof, 0o644); err != nil {
		r.record("node/data-path", Fail, "write through published volume: %v", err)
	} else if got, err := os.ReadFile(proofPath); err != nil || !bytes.Equal(got, proof) {
		r.record("node/data-path", Fail, "read back mismatch (err=%v)", err)
	} else {
		r.record("node/data-path", Pass, "wrote and read back through the published mount")
	}

	if caps.NodeReclaimSpace {
		req := bardplugin.NodeReclaimSpaceRequest{Volume: v1ref, VolumePath: target, StagingPath: staging}
		if err := r.call(ctx, bardplugin.PathNodeReclaimSpace, req, nil); err != nil {
			r.record("node/reclaimspace", Fail, "%v", err)
		} else {
			r.record("node/reclaimspace", Pass, "reclaim on a published volume succeeds")
		}
	}

	unpub := bardplugin.NodeUnpublishRequest{Volume: v1ref, TargetPath: target}
	if err := r.call(ctx, bardplugin.PathNodeUnpublish, unpub, nil); err != nil {
		r.record("node/unpublish", Fail, "%v", err)
		return
	}
	published = false
	if err := r.call(ctx, bardplugin.PathNodeUnpublish, unpub, nil); err != nil {
		r.record("node/unpublish", Fail, "repeated unpublish must succeed (kubelet retries): %v", err)
	} else {
		r.record("node/unpublish", Pass, "unpublished, idempotent on repeat")
	}

	unstage := bardplugin.NodeUnstageRequest{Volume: v1ref, StagingPath: staging}
	if err := r.call(ctx, bardplugin.PathNodeUnstage, unstage, nil); err != nil {
		r.record("node/unstage", Fail, "%v", err)
		return
	}
	staged = false
	if err := r.call(ctx, bardplugin.PathNodeUnstage, unstage, nil); err != nil {
		r.record("node/unstage", Fail, "repeated unstage must succeed (kubelet retries): %v", err)
	} else {
		r.record("node/unstage", Pass, "unstaged, idempotent on repeat")
	}
}

// cleanup best-effort deletes anything the checks left behind (snapshots
// before volumes -- a snapshot's source must usually outlive it).
func (r *runner) cleanup(ctx context.Context) {
	for _, ref := range append([]bardplugin.VolumeRef{}, r.liveSnaps...) {
		if err := r.deleteSnap(ctx, ref); err != nil {
			r.cfg.Logf("cleanup: leftover snapshot %s/%s: %v", ref.Location, ref.Name, err)
		}
	}
	for _, ref := range append([]bardplugin.VolumeRef{}, r.liveVols...) {
		if err := r.deleteVol(ctx, ref); err != nil {
			r.cfg.Logf("cleanup: leftover volume %s/%s: %v", ref.Location, ref.Name, err)
		}
	}
}

func containsVol(entries []bardplugin.VolumeListEntry, ref bardplugin.VolumeRef) bool {
	for _, e := range entries {
		if e.Volume == ref {
			return true
		}
	}
	return false
}

func findSnap(entries []bardplugin.SnapshotListEntry, ref bardplugin.VolumeRef) *bardplugin.SnapshotListEntry {
	for i := range entries {
		if entries[i].Snapshot == ref {
			return &entries[i]
		}
	}
	return nil
}
