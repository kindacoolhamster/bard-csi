package iscsiplugin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kindacoolhamster/bard-csi/pkg/bardplugin"
)

// tdLocationPrefix marks a targetd volume's Location so it stays
// distinguishable from a local volume's Location (a bare VG name) even after
// the instance config that created it is later removed -- see isTdLocation.
// This is what lets DeleteVolume/ControllerUnpublish tell "unknown instance,
// but this volume was always local -- derive and clean up anyway" apart from
// "unknown instance, and this volume needs a remote targetd RPC we cannot
// build without it" (see the missing-instance guards in iscsiplugin.go).
const tdLocationPrefix = "targetd:"

// tdLocation marks pool as a targetd volume's Location.
func tdLocation(pool string) string { return tdLocationPrefix + pool }

// isTdLocation reports whether a volume's Location marks it as targetd-managed.
func isTdLocation(loc string) bool { return strings.HasPrefix(loc, tdLocationPrefix) }

// tdPoolFromLocation strips the marker, returning the bare pool name.
func tdPoolFromLocation(loc string) string { return strings.TrimPrefix(loc, tdLocationPrefix) }

// tdRequestTimeout is the HTTP timeout for a targetd JSON-RPC call -- generous
// because a remote host adds network latency the LOCAL (targetcli) path never
// sees, and vol_create/vol_resize can take a few seconds on some backends.
const tdRequestTimeout = 30 * time.Second

// targetd's stable, numeric JSON-RPC error codes for the idempotent-retry
// cases this plugin cares about -- recorded live against targetd 0.10.4
// (.superpowers/sdd/targetd-probe.json): a duplicate vol_create, a vol_destroy
// of an already-gone volume, and an export_destroy of an already-gone export.
// Classification is BY CODE, never by message text: targetd's message wording
// is not a documented, stable API (the local targetcli/iscsiadm path has to
// classify by message because those tools have no error codes at all -- here
// we have codes, so message-matching would be a strictly worse choice, not
// just a stylistic one).
const (
	tdErrVolExists      = -50
	tdErrVolNotFound    = -103
	tdErrExportNotFound = -151
)

// tdError is a targetd JSON-RPC error response.
type tdError struct {
	Code    int
	Message string
}

func (e *tdError) Error() string { return fmt.Sprintf("targetd error %d: %s", e.Code, e.Message) }

// tdCode extracts a targetd error's numeric code, or 0 if err is nil or does
// not wrap a *tdError (0 is never a real targetd error code -- JSON-RPC error
// codes are always negative in this API).
func tdCode(err error) int {
	var te *tdError
	if errors.As(err, &te) {
		return te.Code
	}
	return 0
}

// tdManager is a JSON-RPC 2.0 client for one targetd-managed instance's remote
// LIO host: the endpoint + pool it manages and the Basic Auth credentials it
// authenticates with. targetIQN is carried for callers that need it (it is not
// itself part of any RPC call -- targetd exposes one fixed target per host, so
// nothing in the API names it).
type tdManager struct {
	endpoint  string
	pool      string
	targetIQN string
	creds     *targetdCreds
	client    *http.Client
	nextID    int64
	// mu is a POINTER to the owning Backend's tdAccessMu (see newTdManager) --
	// shared and persistent across every tdManager built for any targetd
	// instance, unlike a plain mutex FIELD here would be (a fresh tdManager is
	// built per RPC call). grantAccess/revokeAccess hold it for their whole
	// read-compute-write sequence so two concurrent calls (e.g. two different
	// volumes granted to the same initiator) cannot both compute the same
	// lowest-unused LUN. nil is a valid, lock-free zero value for tests that
	// construct a tdManager directly.
	mu *sync.Mutex
}

// lock acquires t.mu if set (a no-op when nil, e.g. a directly-constructed
// tdManager in a test that never touches grantAccess/revokeAccess
// concurrently) and returns the matching unlock func -- callers do
// `defer t.lock()()`.
func (t *tdManager) lock() func() {
	if t.mu == nil {
		return func() {}
	}
	t.mu.Lock()
	return t.mu.Unlock
}

// newTdManager builds a tdManager for instance's targetd config + credentials.
func (b *Backend) newTdManager(instance string, ic InstanceConfig) (*tdManager, error) {
	creds, err := b.targetdCredsFor(instance)
	if err != nil {
		return nil, err
	}
	return &tdManager{
		endpoint:  ic.TargetdEndpoint,
		pool:      ic.TargetdPool,
		targetIQN: ic.TargetIQN,
		creds:     creds,
		client:    &http.Client{Timeout: tdRequestTimeout},
		mu:        &b.tdAccessMu,
	}, nil
}

// ---- JSON-RPC transport -----------------------------------------------------

type tdRequest struct {
	JSONRPC string      `json:"jsonrpc"`
	ID      int64       `json:"id"`
	Method  string      `json:"method"`
	Params  interface{} `json:"params"`
}

type tdRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type tdResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int64           `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *tdRPCError     `json:"error"`
}

// call issues one targetd JSON-RPC 2.0 request over HTTP Basic Auth and decodes
// the result into out (nil to discard it). A JSON-RPC error response becomes a
// *tdError so callers can classify it by numeric code (see tdCode); every other
// failure (transport, decode) is wrapped with the method name for context.
//
// Credentials never appear in argv the way the local targetcli/iscsiadm calls'
// CHAP secrets do -- http.Request.SetBasicAuth puts them only in the
// Authorization header, which this method never logs or echoes back into an
// error. redactSecrets still wraps the transport-error path as defense in
// depth: an http.Client error can legitimately embed request/target text, and
// there is no reason a password should ever be in scope for it.
func (t *tdManager) call(ctx context.Context, method string, params interface{}, out interface{}) error {
	id := atomic.AddInt64(&t.nextID, 1)
	body, err := json.Marshal(tdRequest{JSONRPC: "2.0", ID: id, Method: method, Params: params})
	if err != nil {
		return fmt.Errorf("iscsi: targetd %s: marshal request: %w", method, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("iscsi: targetd %s: build request: %w", method, err)
	}
	req.Header.Set("Content-Type", "application/json")
	var pass string
	if t.creds != nil {
		req.SetBasicAuth(t.creds.User, t.creds.Password)
		pass = t.creds.Password
	}
	resp, err := t.client.Do(req)
	if err != nil {
		return redactSecrets(fmt.Errorf("iscsi: targetd %s: %w", method, err), pass)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return redactSecrets(fmt.Errorf("iscsi: targetd %s: read response: %w", method, err), pass)
	}
	var rpcResp tdResponse
	if err := json.Unmarshal(data, &rpcResp); err != nil {
		return fmt.Errorf("iscsi: targetd %s: decode response (status %d): %w", method, resp.StatusCode, err)
	}
	if rpcResp.Error != nil {
		return &tdError{Code: rpcResp.Error.Code, Message: rpcResp.Error.Message}
	}
	if out != nil && len(rpcResp.Result) > 0 {
		if err := json.Unmarshal(rpcResp.Result, out); err != nil {
			return fmt.Errorf("iscsi: targetd %s: decode result: %w", method, err)
		}
	}
	return nil
}

// ---- domain methods ---------------------------------------------------------

// tdExport is one row of targetd's export_list.
type tdExport struct {
	InitiatorWWN string `json:"initiator_wwn"`
	LUN          int    `json:"lun"`
	VolName      string `json:"vol_name"`
	Pool         string `json:"pool"`
	VolSize      int64  `json:"vol_size"`
}

// tdPool is one row of targetd's pool_list.
type tdPool struct {
	Name     string `json:"name"`
	Size     int64  `json:"size"`
	FreeSize int64  `json:"free_size"`
	Type     string `json:"type"`
	UUID     string `json:"uuid"`
}

// tdVolume is one row of targetd's vol_list.
type tdVolume struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
	UUID string `json:"uuid,omitempty"`
}

// createVolume provisions a volume in this instance's pool, sized bytes, with
// the SAME idempotent-retry-vs-real-conflict semantics as the local path's
// found+size check (CreateVolume, iscsiplugin.go): a duplicate create (code
// -50, "Volume with that name exists") is success when the EXISTING volume is
// already at least as large as requested (an idempotent retry), but a real
// conflict -- CodeAlreadyExists -- when it is SMALLER (a different request
// reusing this name, not a retry).
func (t *tdManager) createVolume(ctx context.Context, name string, size int64) error {
	err := t.call(ctx, "vol_create", map[string]interface{}{
		"pool": t.pool, "name": name, "size": size,
	}, nil)
	if err == nil {
		return nil
	}
	if tdCode(err) != tdErrVolExists {
		return err
	}
	existing, ok, ferr := t.findVolume(ctx, name)
	if ferr != nil {
		return fmt.Errorf("iscsi: targetd verify existing volume %q: %w", name, ferr)
	}
	if !ok {
		return fmt.Errorf("iscsi: targetd volume %q reported as existing (vol_create code %d) but not found in vol_list", name, tdErrVolExists)
	}
	if existing.Size < size {
		return bardplugin.Errorf(bardplugin.CodeAlreadyExists,
			"iscsi: targetd volume %q exists at %d bytes, smaller than requested %d", name, existing.Size, size)
	}
	return nil
}

// findVolume looks up one volume by name in this instance's pool, or (zero,
// false, nil) if it does not exist.
func (t *tdManager) findVolume(ctx context.Context, name string) (tdVolume, bool, error) {
	vols, err := t.listVolumes(ctx)
	if err != nil {
		return tdVolume{}, false, err
	}
	for _, v := range vols {
		if v.Name == name {
			return v, true, nil
		}
	}
	return tdVolume{}, false, nil
}

// deleteVolume destroys every export of name (via export_list, filtered to
// this volume) before destroying the volume itself -- targetd, like the local
// targetcli path, refuses to remove a volume that is still exported.
// Idempotent: a vol_destroy of an already-gone volume (code -103) is success.
func (t *tdManager) deleteVolume(ctx context.Context, name string) error {
	exports, err := t.listExports(ctx)
	if err != nil {
		return err
	}
	for _, e := range exports {
		if e.VolName != name {
			continue
		}
		if err := t.revokeAccess(ctx, name, e.InitiatorWWN); err != nil {
			return err
		}
	}
	err = t.call(ctx, "vol_destroy", map[string]interface{}{"pool": t.pool, "name": name}, nil)
	if tdCode(err) == tdErrVolNotFound {
		return nil
	}
	return err
}

// resizeVolume grows a volume to size bytes.
func (t *tdManager) resizeVolume(ctx context.Context, name string, size int64) error {
	return t.call(ctx, "vol_resize", map[string]interface{}{"pool": t.pool, "name": name, "size": size}, nil)
}

// listVolumes enumerates every volume in this instance's pool.
func (t *tdManager) listVolumes(ctx context.Context) ([]tdVolume, error) {
	var vols []tdVolume
	if err := t.call(ctx, "vol_list", map[string]interface{}{"pool": t.pool}, &vols); err != nil {
		return nil, err
	}
	return vols, nil
}

// capacity returns this instance's pool's free bytes (pool_list, filtered to
// t.pool).
func (t *tdManager) capacity(ctx context.Context) (int64, error) {
	var pools []tdPool
	if err := t.call(ctx, "pool_list", map[string]interface{}{}, &pools); err != nil {
		return 0, err
	}
	for _, p := range pools {
		if p.Name == t.pool {
			return p.FreeSize, nil
		}
	}
	return 0, fmt.Errorf("iscsi: targetd pool %q not found in pool_list", t.pool)
}

// listExports returns every export on the target (all volumes, all
// initiators) -- targetd's API has no per-volume or per-initiator filter, so
// callers filter client-side.
func (t *tdManager) listExports(ctx context.Context) ([]tdExport, error) {
	var exports []tdExport
	if err := t.call(ctx, "export_list", map[string]interface{}{}, &exports); err != nil {
		return nil, err
	}
	return exports, nil
}

// grantAccess exports vol to initiatorIQN, setting CHAP credentials on the
// initiator FIRST when chap is non-nil (targetd's initiator_set_auth is
// per-initiator, not per-export, so it must be in place before the export is
// usable -- an initiator that logs in between the export and the auth set
// would see an unauthenticated target). LUN is the lowest number not already
// used by ANY of this initiator's exports on this target (targetd assigns
// LUNs per initiator, not per volume, unlike the local path's fixed LUN 0
// per-volume target).
//
// Idempotent: an existing export of vol to initiatorIQN returns its
// already-assigned LUN with no new export_create call, so a retried
// ControllerPublish converges instead of risking a second LUN mapping for the
// same (volume, initiator) pair (targetd's duplicate-export phrasing/code was
// never recorded in the probe, so this is checked structurally instead of by
// error classification).
func (t *tdManager) grantAccess(ctx context.Context, vol, initiatorIQN string, chap *chapCreds) (int, error) {
	defer t.lock()()
	if chap != nil {
		params := map[string]interface{}{
			"initiator_wwn": initiatorIQN,
			"in_user":       chap.User,
			"in_pass":       chap.Password,
			"out_user":      chap.MutualUser,
			"out_pass":      chap.MutualPassword,
		}
		if err := t.call(ctx, "initiator_set_auth", params, nil); err != nil {
			return 0, redactSecrets(fmt.Errorf("iscsi: targetd set auth for %s: %w", initiatorIQN, err),
				chap.Password, chap.MutualPassword)
		}
	}
	exports, err := t.listExports(ctx)
	if err != nil {
		return 0, err
	}
	used := map[int]bool{}
	for _, e := range exports {
		if e.InitiatorWWN != initiatorIQN {
			continue
		}
		if e.VolName == vol {
			return e.LUN, nil // already exported to this initiator -- idempotent retry
		}
		used[e.LUN] = true
	}
	lun := 0
	for used[lun] {
		lun++
	}
	if err := t.call(ctx, "export_create", map[string]interface{}{
		"pool": t.pool, "vol": vol, "initiator_wwn": initiatorIQN, "lun": lun,
	}, nil); err != nil {
		return 0, err
	}
	return lun, nil
}

// revokeAccess un-exports vol from initiatorIQN. Idempotent: an export_destroy
// of an already-gone export (code -151) is success.
func (t *tdManager) revokeAccess(ctx context.Context, vol, initiatorIQN string) error {
	defer t.lock()()
	err := t.call(ctx, "export_destroy", map[string]interface{}{
		"pool": t.pool, "vol": vol, "initiator_wwn": initiatorIQN,
	}, nil)
	if tdCode(err) == tdErrExportNotFound {
		return nil
	}
	return err
}

// volSize floors a requested capacity to a minimum of 1MiB, mirroring lvBytes'
// floor for the local path -- a zero/negative CapacityBytes is not meaningful
// to targetd either.
func volSize(b int64) int64 {
	if b <= 0 {
		return 1 << 20
	}
	return b
}

// ---- Backend wiring (branch-per-op from the local/targetcli methods) -------

// createVolumeTargetd is CreateVolume's targetd branch: provision through the
// remote targetd JSON-RPC API instead of local lvcreate/targetcli. Reuses
// lvName so a volume's identity is derived the SAME way regardless of
// management mode -- ListVolumes/DeleteVolume/etc. never need to know which
// mode created a given name.
func (b *Backend) createVolumeTargetd(ctx context.Context, instance string, ic InstanceConfig, req *bardplugin.CreateVolumeRequest) (*bardplugin.CreateVolumeResponse, error) {
	td, err := b.newTdManager(instance, ic)
	if err != nil {
		return nil, err
	}
	name := lvName(req.Name)
	size := volSize(req.CapacityBytes)
	if err := td.createVolume(ctx, name, size); err != nil {
		var se *bardplugin.StatusError
		if errors.As(err, &se) {
			return nil, err // already CSI-shaped (e.g. a size-mismatch CodeAlreadyExists) -- don't double-wrap
		}
		return nil, fmt.Errorf("iscsi: targetd create volume %s: %w", name, err)
	}
	return &bardplugin.CreateVolumeResponse{
		Location:      tdLocation(ic.TargetdPool),
		Name:          name,
		CapacityBytes: size,
	}, nil
}

// deleteVolumeTargetd is DeleteVolume's targetd branch.
func (b *Backend) deleteVolumeTargetd(ctx context.Context, instance string, ic InstanceConfig, name string) error {
	td, err := b.newTdManager(instance, ic)
	if err != nil {
		return err
	}
	if err := td.deleteVolume(ctx, name); err != nil {
		return fmt.Errorf("iscsi: targetd delete volume %s: %w", name, err)
	}
	return nil
}

// expandVolumeTargetd is ExpandVolume's targetd branch. Unlike the local
// path's block backstore (which follows the LV's size automatically once
// lvextend runs), targetd's export is a separate object -- but the initiator
// still must rescan the session either way, so NodeExpand needs no
// targetd-specific change.
func (b *Backend) expandVolumeTargetd(ctx context.Context, instance string, ic InstanceConfig, name string, newSize int64) (*bardplugin.ExpandVolumeResponse, error) {
	td, err := b.newTdManager(instance, ic)
	if err != nil {
		return nil, err
	}
	size := volSize(newSize)
	if err := td.resizeVolume(ctx, name, size); err != nil {
		return nil, fmt.Errorf("iscsi: targetd resize volume %s: %w", name, err)
	}
	return &bardplugin.ExpandVolumeResponse{CapacityBytes: size, NodeExpansionRequired: true}, nil
}

// getCapacityTargetd is GetCapacity's targetd branch.
func (b *Backend) getCapacityTargetd(ctx context.Context, instance string, ic InstanceConfig) (*bardplugin.GetCapacityResponse, error) {
	td, err := b.newTdManager(instance, ic)
	if err != nil {
		return nil, err
	}
	free, err := td.capacity(ctx)
	if err != nil {
		return nil, fmt.Errorf("iscsi: targetd capacity: %w", err)
	}
	return &bardplugin.GetCapacityResponse{AvailableBytes: free}, nil
}

// listVolumesTargetd is ListVolumes' per-instance targetd branch.
func (b *Backend) listVolumesTargetd(ctx context.Context, instance string, ic InstanceConfig) ([]bardplugin.VolumeListEntry, error) {
	td, err := b.newTdManager(instance, ic)
	if err != nil {
		return nil, err
	}
	vols, err := td.listVolumes(ctx)
	if err != nil {
		return nil, fmt.Errorf("iscsi: targetd list volumes: %w", err)
	}
	entries := make([]bardplugin.VolumeListEntry, 0, len(vols))
	for _, v := range vols {
		entries = append(entries, bardplugin.VolumeListEntry{
			Volume:        bardplugin.VolumeRef{Instance: instance, Location: tdLocation(ic.TargetdPool), Name: v.Name},
			CapacityBytes: v.Size,
		})
	}
	return entries, nil
}

// controllerPublishTargetd is ControllerPublish's targetd branch: grants the
// node's derived initiator access (CHAP first, if enforced) and returns the
// connection context NodeStage needs. As with the local path, credentials are
// never part of PublishContext.
func (b *Backend) controllerPublishTargetd(ctx context.Context, instance string, ic InstanceConfig, req *bardplugin.ControllerPublishRequest) (*bardplugin.ControllerPublishResponse, error) {
	td, err := b.newTdManager(instance, ic)
	if err != nil {
		return nil, err
	}
	chap, err := b.chapFor(instance)
	if err != nil {
		return nil, err
	}
	initIQN := initiatorIQN(ic.IQNBase, req.NodeID)
	lun, err := td.grantAccess(ctx, req.Volume.Name, initIQN, chap)
	if err != nil {
		return nil, fmt.Errorf("iscsi: targetd grant access for %s: %w", initIQN, err)
	}
	portals := ic.portalList()
	pc := map[string]string{
		ctxPortal: portals[0],
		ctxIQN:    ic.TargetIQN,
		ctxLUN:    strconv.Itoa(lun),
	}
	if len(portals) >= 2 {
		pc[ctxPortals] = strings.Join(portals, ",")
	}
	return &bardplugin.ControllerPublishResponse{PublishContext: pc}, nil
}

// controllerUnpublishTargetd is ControllerUnpublish's targetd branch. Like the
// local path, IQNBase falls back to the default when the instance config is
// missing/underspecified, so a node's access is still revocable even then.
func (b *Backend) controllerUnpublishTargetd(ctx context.Context, instance string, ic InstanceConfig, req *bardplugin.ControllerUnpublishRequest) error {
	td, err := b.newTdManager(instance, ic)
	if err != nil {
		return err
	}
	base := ic.IQNBase
	if base == "" {
		base = defaultIQNBase
	}
	initIQN := initiatorIQN(base, req.NodeID)
	if err := td.revokeAccess(ctx, req.Volume.Name, initIQN); err != nil {
		return fmt.Errorf("iscsi: targetd revoke access for %s: %w", initIQN, err)
	}
	return nil
}
