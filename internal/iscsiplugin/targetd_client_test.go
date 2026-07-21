package iscsiplugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kindacoolhamster/bard-csi/pkg/bardplugin"
)

// ---- fake JSON-RPC server ---------------------------------------------------
//
// tdFakeServer replays the shapes recorded live against targetd 0.10.4
// (.superpowers/sdd/targetd-probe.json): pool_list/export_list/vol_list row
// shapes, a duplicate vol_create returning code -50, a vol_destroy of a
// missing volume returning code -103, an export_destroy of a missing export
// returning code -151. Handlers are pluggable per method so each test only
// wires what it needs; an un-wired method fails the test loudly (via
// t.Fatalf from inside the handler goroutine) rather than silently 200ing.

type tdFakeCall struct {
	Method string
	Params map[string]interface{}
}

type tdFakeServer struct {
	mu         sync.Mutex
	calls      []tdFakeCall
	handlers   map[string]func(params map[string]interface{}) (interface{}, *tdRPCError)
	user, pass string // expected Basic Auth credentials; "" skips the check
	authFailed bool
	t          *testing.T
}

func newTdFakeServer(t *testing.T) *tdFakeServer {
	return &tdFakeServer{handlers: map[string]func(map[string]interface{}) (interface{}, *tdRPCError){}, t: t}
}

func (s *tdFakeServer) on(method string, fn func(params map[string]interface{}) (interface{}, *tdRPCError)) {
	s.handlers[method] = fn
}

func (s *tdFakeServer) start() *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/targetrpc" {
			http.NotFound(w, r)
			return
		}
		if s.user != "" {
			u, p, ok := r.BasicAuth()
			s.mu.Lock()
			if !ok || u != s.user || p != s.pass {
				s.authFailed = true
			}
			s.mu.Unlock()
		}
		var req tdRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		params, _ := req.Params.(map[string]interface{})
		s.mu.Lock()
		s.calls = append(s.calls, tdFakeCall{Method: req.Method, Params: params})
		s.mu.Unlock()
		fn, ok := s.handlers[req.Method]
		if !ok {
			s.t.Fatalf("tdFakeServer: no handler wired for method %q", req.Method)
			return
		}
		result, rpcErr := fn(params)
		resp := tdResponse{JSONRPC: "2.0", ID: req.ID, Error: rpcErr}
		if rpcErr == nil {
			data, err := json.Marshal(result)
			if err != nil {
				s.t.Fatalf("tdFakeServer: marshal result for %q: %v", req.Method, err)
				return
			}
			resp.Result = data
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	s.t.Cleanup(srv.Close)
	return srv
}

func (s *tdFakeServer) callOrder() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.calls))
	for i, c := range s.calls {
		out[i] = c.Method
	}
	return out
}

func indexOf(list []string, s string) int {
	for i, v := range list {
		if v == s {
			return i
		}
	}
	return -1
}

// newTestTdManager builds a tdManager pointed at a fake server's URL with a
// short (test-speed) timeout, default credentials, and its OWN real mutex (so
// tests exercising grantAccess/revokeAccess concurrency go through the real
// locking path, not the nil-mu no-op).
func newTestTdManager(baseURL, pool, user, pass string) *tdManager {
	return &tdManager{
		endpoint: baseURL + "/targetrpc",
		pool:     pool,
		creds:    &targetdCreds{User: user, Password: pass},
		client:   &http.Client{Timeout: 5 * time.Second},
		mu:       &sync.Mutex{},
	}
}

// ---- tdManager unit tests ---------------------------------------------------

// A duplicate vol_create (targetd code -50, "Volume with that name exists")
// against an existing volume that is ALREADY AT LEAST AS LARGE as requested
// must be treated as success -- the idempotent-retry case, classified by the
// NUMERIC code recorded in the probe, never by message text -- mirroring the
// local path's found+size-ok convergence.
func TestTdCreateVolumeIdempotentOnDuplicate(t *testing.T) {
	srv := newTdFakeServer(t)
	calls := 0
	srv.on("vol_create", func(map[string]interface{}) (interface{}, *tdRPCError) {
		calls++
		if calls == 1 {
			return nil, nil
		}
		return nil, &tdRPCError{Code: tdErrVolExists, Message: "Volume with that name exists"}
	})
	srv.on("vol_list", func(map[string]interface{}) (interface{}, *tdRPCError) {
		return []tdVolume{{Name: "bard-x", Size: 1 << 30}}, nil // exactly the requested size
	})
	httpSrv := srv.start()
	td := newTestTdManager(httpSrv.URL, "pool1", "u", "p")

	if err := td.createVolume(context.Background(), "bard-x", 1<<30); err != nil {
		t.Fatalf("first create: %v", err)
	}
	if err := td.createVolume(context.Background(), "bard-x", 1<<30); err != nil {
		t.Fatalf("duplicate create must be idempotent (code -50) when the existing volume is large enough, got %v", err)
	}
	if calls != 2 {
		t.Fatalf("expected 2 vol_create calls, got %d", calls)
	}
}

// A duplicate vol_create against an existing volume SMALLER than requested is
// a real conflict (a different request reusing this name, not a retry) --
// CodeAlreadyExists, mirroring the local path's found-but-smaller rejection.
func TestTdCreateVolumeSizeMismatchRejected(t *testing.T) {
	srv := newTdFakeServer(t)
	srv.on("vol_create", func(map[string]interface{}) (interface{}, *tdRPCError) {
		return nil, &tdRPCError{Code: tdErrVolExists, Message: "Volume with that name exists"}
	})
	srv.on("vol_list", func(map[string]interface{}) (interface{}, *tdRPCError) {
		return []tdVolume{{Name: "bard-x", Size: 1 << 20}}, nil // smaller than requested
	})
	httpSrv := srv.start()
	td := newTestTdManager(httpSrv.URL, "pool1", "u", "p")

	err := td.createVolume(context.Background(), "bard-x", 1<<30)
	var se *bardplugin.StatusError
	if err == nil || !errors.As(err, &se) || se.Code != bardplugin.CodeAlreadyExists {
		t.Fatalf("a duplicate create against a SMALLER existing volume must fail with CodeAlreadyExists, got %v", err)
	}
}

// A vol_destroy of an already-gone volume (code -103) must be treated as
// success.
func TestTdDeleteVolumeIdempotentOnMissing(t *testing.T) {
	srv := newTdFakeServer(t)
	srv.on("export_list", func(map[string]interface{}) (interface{}, *tdRPCError) {
		return []tdExport{}, nil
	})
	srv.on("vol_destroy", func(map[string]interface{}) (interface{}, *tdRPCError) {
		return nil, &tdRPCError{Code: tdErrVolNotFound, Message: "Volume bard-x not found in pool pool1"}
	})
	httpSrv := srv.start()
	td := newTestTdManager(httpSrv.URL, "pool1", "u", "p")

	if err := td.deleteVolume(context.Background(), "bard-x"); err != nil {
		t.Fatalf("delete of a missing volume must be idempotent (code -103), got %v", err)
	}
}

// deleteVolume must destroy every export of the volume BEFORE destroying the
// volume itself, and must never touch an export belonging to a DIFFERENT
// volume.
func TestTdDeleteVolumeDestroysExportsBeforeVolume(t *testing.T) {
	srv := newTdFakeServer(t)
	srv.on("export_list", func(map[string]interface{}) (interface{}, *tdRPCError) {
		return []tdExport{
			{InitiatorWWN: "iqn.init-a", LUN: 0, VolName: "bard-x"},
			{InitiatorWWN: "iqn.init-b", LUN: 3, VolName: "bard-x"},
			{InitiatorWWN: "iqn.init-a", LUN: 1, VolName: "bard-other"}, // different volume: must NOT be touched
		}, nil
	})
	var destroyed []string
	srv.on("export_destroy", func(p map[string]interface{}) (interface{}, *tdRPCError) {
		destroyed = append(destroyed, fmt.Sprintf("%v", p["initiator_wwn"]))
		if p["vol"] != "bard-x" {
			t.Errorf("export_destroy touched the wrong volume: %v", p["vol"])
		}
		return nil, nil
	})
	srv.on("vol_destroy", func(map[string]interface{}) (interface{}, *tdRPCError) {
		return nil, nil
	})
	httpSrv := srv.start()
	td := newTestTdManager(httpSrv.URL, "pool1", "u", "p")

	if err := td.deleteVolume(context.Background(), "bard-x"); err != nil {
		t.Fatal(err)
	}
	if len(destroyed) != 2 {
		t.Fatalf("expected 2 exports destroyed for bard-x, got %v", destroyed)
	}
	order := srv.callOrder()
	vdIdx := indexOf(order, "vol_destroy")
	if vdIdx < 0 {
		t.Fatalf("vol_destroy never called, order %v", order)
	}
	for i, m := range order {
		if m == "export_destroy" && i > vdIdx {
			t.Fatalf("every export_destroy must precede vol_destroy, got order %v", order)
		}
	}
}

func TestTdResizeVolume(t *testing.T) {
	srv := newTdFakeServer(t)
	var got map[string]interface{}
	srv.on("vol_resize", func(p map[string]interface{}) (interface{}, *tdRPCError) {
		got = p
		return nil, nil
	})
	httpSrv := srv.start()
	td := newTestTdManager(httpSrv.URL, "pool1", "u", "p")

	if err := td.resizeVolume(context.Background(), "bard-x", 2<<30); err != nil {
		t.Fatal(err)
	}
	if got["name"] != "bard-x" || got["pool"] != "pool1" {
		t.Fatalf("unexpected vol_resize params: %+v", got)
	}
	if size, ok := got["size"].(float64); !ok || int64(size) != 2<<30 {
		t.Fatalf("unexpected vol_resize size: %+v", got["size"])
	}
}

// LUN allocation is per-initiator, lowest unused: an initiator with holes in
// its used-LUN set gets the hole, and a second initiator with different usage
// gets its OWN lowest-unused LUN.
func TestTdGrantAccessLUNAllocation(t *testing.T) {
	srv := newTdFakeServer(t)
	srv.on("export_list", func(map[string]interface{}) (interface{}, *tdRPCError) {
		return []tdExport{
			{InitiatorWWN: "iqn.a", LUN: 0, VolName: "bard-other1"},
			{InitiatorWWN: "iqn.a", LUN: 2, VolName: "bard-other2"}, // hole at 1
			{InitiatorWWN: "iqn.b", LUN: 0, VolName: "bard-other3"},
		}, nil
	})
	var created []map[string]interface{}
	srv.on("export_create", func(p map[string]interface{}) (interface{}, *tdRPCError) {
		created = append(created, p)
		return nil, nil
	})
	httpSrv := srv.start()
	td := newTestTdManager(httpSrv.URL, "pool1", "u", "p")

	lunA, err := td.grantAccess(context.Background(), "bard-new", "iqn.a", nil)
	if err != nil {
		t.Fatal(err)
	}
	if lunA != 1 {
		t.Fatalf("initiator a should get the hole at LUN 1, got %d", lunA)
	}
	lunB, err := td.grantAccess(context.Background(), "bard-new", "iqn.b", nil)
	if err != nil {
		t.Fatal(err)
	}
	if lunB != 1 {
		t.Fatalf("initiator b should get LUN 1 (only 0 used), got %d", lunB)
	}
	if len(created) != 2 {
		t.Fatalf("expected 2 export_create calls, got %d", len(created))
	}
}

// grantAccess must be idempotent for an ALREADY-exported (vol, initiator)
// pair: it returns the existing LUN and does NOT call export_create again
// (which would risk a second LUN mapping for the same pair on a retried
// ControllerPublish).
func TestTdGrantAccessIdempotentOnExistingExport(t *testing.T) {
	srv := newTdFakeServer(t)
	srv.on("export_list", func(map[string]interface{}) (interface{}, *tdRPCError) {
		return []tdExport{{InitiatorWWN: "iqn.a", LUN: 3, VolName: "bard-x"}}, nil
	})
	created := 0
	srv.on("export_create", func(map[string]interface{}) (interface{}, *tdRPCError) {
		created++
		return nil, nil
	})
	httpSrv := srv.start()
	td := newTestTdManager(httpSrv.URL, "pool1", "u", "p")

	lun, err := td.grantAccess(context.Background(), "bard-x", "iqn.a", nil)
	if err != nil {
		t.Fatal(err)
	}
	if lun != 3 {
		t.Fatalf("must return the existing LUN, got %d", lun)
	}
	if created != 0 {
		t.Fatalf("must not call export_create for an already-exported (vol, initiator) pair, got %d calls", created)
	}
}

// initiator_set_auth (CHAP) must be called BEFORE export_create -- an
// initiator that logs in between the export and the auth set would see an
// unauthenticated target.
func TestTdGrantAccessSetsCHAPBeforeExport(t *testing.T) {
	srv := newTdFakeServer(t)
	srv.on("export_list", func(map[string]interface{}) (interface{}, *tdRPCError) {
		return []tdExport{}, nil
	})
	var gotAuth map[string]interface{}
	srv.on("initiator_set_auth", func(p map[string]interface{}) (interface{}, *tdRPCError) {
		gotAuth = p
		return nil, nil
	})
	srv.on("export_create", func(map[string]interface{}) (interface{}, *tdRPCError) {
		return nil, nil
	})
	httpSrv := srv.start()
	td := newTestTdManager(httpSrv.URL, "pool1", "u", "p")
	chap := &chapCreds{User: "chapuser", Password: "chappass"}

	if _, err := td.grantAccess(context.Background(), "bard-x", "iqn.a", chap); err != nil {
		t.Fatal(err)
	}
	order := srv.callOrder()
	authIdx, createIdx := indexOf(order, "initiator_set_auth"), indexOf(order, "export_create")
	if authIdx < 0 || createIdx < 0 || authIdx > createIdx {
		t.Fatalf("initiator_set_auth must precede export_create, got order %v", order)
	}
	if gotAuth["in_user"] != "chapuser" || gotAuth["in_pass"] != "chappass" {
		t.Fatalf("unexpected initiator_set_auth params: %+v", gotAuth)
	}
}

// An export_destroy of an already-gone export (code -151) must be treated as
// success.
func TestTdRevokeAccessIdempotentOnMissing(t *testing.T) {
	srv := newTdFakeServer(t)
	srv.on("export_destroy", func(map[string]interface{}) (interface{}, *tdRPCError) {
		return nil, &tdRPCError{Code: tdErrExportNotFound, Message: "Volume 'bard-x' not found in iqn.a exports"}
	})
	httpSrv := srv.start()
	td := newTestTdManager(httpSrv.URL, "pool1", "u", "p")

	if err := td.revokeAccess(context.Background(), "bard-x", "iqn.a"); err != nil {
		t.Fatalf("revoke of a missing export must be idempotent (code -151), got %v", err)
	}
}

func TestTdCapacity(t *testing.T) {
	srv := newTdFakeServer(t)
	srv.on("pool_list", func(map[string]interface{}) (interface{}, *tdRPCError) {
		return []tdPool{
			{Name: "other-pool", FreeSize: 999},
			{Name: "pool1", FreeSize: 12345, Size: 99999, Type: "block", UUID: "abc"},
		}, nil
	})
	httpSrv := srv.start()
	td := newTestTdManager(httpSrv.URL, "pool1", "u", "p")

	free, err := td.capacity(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if free != 12345 {
		t.Fatalf("expected free_size 12345 for pool1, got %d", free)
	}
}

func TestTdListVolumes(t *testing.T) {
	srv := newTdFakeServer(t)
	var gotPool interface{}
	srv.on("vol_list", func(p map[string]interface{}) (interface{}, *tdRPCError) {
		gotPool = p["pool"]
		return []tdVolume{{Name: "bard-a", Size: 100}, {Name: "bard-b", Size: 200}}, nil
	})
	httpSrv := srv.start()
	td := newTestTdManager(httpSrv.URL, "pool1", "u", "p")

	vols, err := td.listVolumes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if gotPool != "pool1" {
		t.Fatalf("expected vol_list to be scoped to pool1, got %v", gotPool)
	}
	if len(vols) != 2 || vols[0].Name != "bard-a" || vols[1].Size != 200 {
		t.Fatalf("unexpected volumes: %+v", vols)
	}
}

// Basic Auth must be present on every request, using the credentials the
// tdManager was built with.
func TestTdBasicAuthOnEveryRequest(t *testing.T) {
	srv := newTdFakeServer(t)
	srv.user, srv.pass = "svcuser", "svcpass"
	srv.on("pool_list", func(map[string]interface{}) (interface{}, *tdRPCError) {
		return []tdPool{{Name: "pool1", FreeSize: 100}}, nil
	})
	httpSrv := srv.start()
	td := newTestTdManager(httpSrv.URL, "pool1", "svcuser", "svcpass")

	if _, err := td.capacity(context.Background()); err != nil {
		t.Fatal(err)
	}
	if srv.authFailed {
		t.Fatal("Basic Auth header missing or incorrect on the request")
	}
}

// The tdManager's http.Client timeout must actually bound a call against an
// unresponsive server.
func TestTdRequestTimeoutRespected(t *testing.T) {
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-block
	}))
	t.Cleanup(func() {
		close(block)
		srv.Close()
	})

	td := &tdManager{
		endpoint: srv.URL + "/targetrpc",
		pool:     "pool1",
		creds:    &targetdCreds{User: "u", Password: "p"},
		client:   &http.Client{Timeout: 100 * time.Millisecond},
	}
	start := time.Now()
	err := td.createVolume(context.Background(), "bard-x", 1<<30)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected a timeout error against an unresponsive server")
	}
	if elapsed > 3*time.Second {
		t.Fatalf("call did not respect the client timeout, took %v", elapsed)
	}
}

// A targetd credential must NEVER appear in a returned error string, even on
// a transport-level failure (connection refused) where an http.Client error
// could otherwise embed request/target text.
func TestTdErrorNeverLeaksPassword(t *testing.T) {
	td := &tdManager{
		endpoint: "http://127.0.0.1:1/targetrpc", // reserved port: connection refused
		pool:     "pool1",
		creds:    &targetdCreds{User: "svcuser", Password: "super-secret-pw"},
		client:   &http.Client{Timeout: 2 * time.Second},
	}
	err := td.createVolume(context.Background(), "bard-x", 1<<30)
	if err == nil {
		t.Fatal("expected a connection error against a closed port")
	}
	if strings.Contains(err.Error(), "super-secret-pw") {
		t.Fatalf("targetd transport error must never leak the password, got %q", err.Error())
	}
}

// ---- Backend wiring (branch-per-op) ----------------------------------------

// wiredTargetdInst returns a targetd instance config pointed at a fake
// server's URL, and the targetd-dir credentials directory for it.
func wiredTargetdInst(t *testing.T, baseURL string) (map[string]InstanceConfig, string) {
	t.Helper()
	inst, dir := targetdCredsSetup(t, "svcuser\nsvcpass\n")
	remote := inst["remote"]
	remote.TargetdEndpoint = baseURL + "/targetrpc"
	inst["remote"] = remote
	return inst, dir
}

// CreateVolume and DeleteVolume on a targetd instance must go through the
// tdManager path -- never the local Runner (lvcreate/targetcli). CreateVolume's
// response Location must be MARKED (see tdLocation) so it stays
// distinguishable from a local VG name even after the instance config that
// created it is later removed, and feeding that EXACT handle back into
// DeleteVolume (instance still present) must round-trip through the
// tdManager path, not a local vg/lv interpretation of the marked string.
func TestBackendCreateAndDeleteVolumeTargetd(t *testing.T) {
	srv := newTdFakeServer(t)
	var createdName string
	srv.on("vol_create", func(p map[string]interface{}) (interface{}, *tdRPCError) {
		createdName = fmt.Sprintf("%v", p["name"])
		return nil, nil
	})
	srv.on("export_list", func(map[string]interface{}) (interface{}, *tdRPCError) {
		return []tdExport{}, nil
	})
	srv.on("vol_destroy", func(map[string]interface{}) (interface{}, *tdRPCError) {
		return nil, nil
	})
	httpSrv := srv.start()
	inst, dir := wiredTargetdInst(t, httpSrv.URL)
	run := &fakeRunner{}
	b := New(inst, "", "", "", dir, "", run)

	resp, err := b.CreateVolume(context.Background(), &bardplugin.CreateVolumeRequest{
		Name: "pvc-1", Instance: "remote", CapacityBytes: 1 << 30,
	})
	if err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
	if !isTdLocation(resp.Location) {
		t.Fatalf("a targetd volume's Location must be MARKED so it stays distinguishable if the instance config is later removed, got %q", resp.Location)
	}
	if tdPoolFromLocation(resp.Location) != "vg-targetd" || resp.Name != createdName || resp.CapacityBytes != 1<<30 {
		t.Fatalf("unexpected CreateVolume response %+v (targetd created %q)", resp, createdName)
	}
	if len(run.calls) != 0 {
		t.Fatalf("CreateVolume on a targetd instance must never invoke the local Runner, got %v", run.calls)
	}

	// Feed the EXACT returned handle back -- the round-trip proves the marked
	// Location still resolves through the tdManager path.
	if err := b.DeleteVolume(context.Background(), &bardplugin.DeleteVolumeRequest{
		Volume: bardplugin.VolumeRef{Instance: "remote", Location: resp.Location, Name: resp.Name},
	}); err != nil {
		t.Fatalf("DeleteVolume round-trip: %v", err)
	}
	if len(run.calls) != 0 {
		t.Fatalf("DeleteVolume on a targetd instance must never invoke the local Runner, got %v", run.calls)
	}
}

// ExpandVolume and GetCapacity must also branch to targetd and skip the local
// Runner entirely.
func TestBackendExpandVolumeAndGetCapacityTargetd(t *testing.T) {
	srv := newTdFakeServer(t)
	// Expand looks the volume up first so an already-big-enough volume is a
	// no-op: targetd's vol_resize hard-errors on a same-size request, which
	// would otherwise make the resizer's retry fail forever.
	curSize := int64(1 << 30)
	resizes := 0
	srv.on("vol_list", func(map[string]interface{}) (interface{}, *tdRPCError) {
		return []tdVolume{{Name: "bard-x", Size: curSize}}, nil
	})
	srv.on("vol_resize", func(p map[string]interface{}) (interface{}, *tdRPCError) {
		resizes++
		if sz, ok := p["size"].(float64); ok {
			if int64(sz) <= curSize {
				// Mirror real targetd 0.10.4, which refuses a non-growing resize.
				return nil, &tdRPCError{Code: -32602, Message: "Size need a larger than size in original volume"}
			}
			curSize = int64(sz)
		}
		return nil, nil
	})
	srv.on("pool_list", func(map[string]interface{}) (interface{}, *tdRPCError) {
		return []tdPool{{Name: "vg-targetd", FreeSize: 555}}, nil
	})
	httpSrv := srv.start()
	inst, dir := wiredTargetdInst(t, httpSrv.URL)
	run := &fakeRunner{}
	b := New(inst, "", "", "", dir, "", run)

	expResp, err := b.ExpandVolume(context.Background(), &bardplugin.ExpandVolumeRequest{
		Volume: bardplugin.VolumeRef{Instance: "remote", Location: "vg-targetd", Name: "bard-x"}, NewSizeBytes: 2 << 30,
	})
	if err != nil {
		t.Fatalf("ExpandVolume: %v", err)
	}
	if expResp.CapacityBytes != 2<<30 || !expResp.NodeExpansionRequired {
		t.Fatalf("unexpected ExpandVolume response %+v", expResp)
	}
	if resizes != 1 {
		t.Fatalf("first expand should issue exactly one vol_resize, got %d", resizes)
	}

	// The SAME expand again (what the external-resizer retries) must succeed as
	// a no-op and must NOT re-issue vol_resize -- real targetd rejects a
	// non-growing resize, so a re-issue would wedge the PVC forever.
	again, err := b.ExpandVolume(context.Background(), &bardplugin.ExpandVolumeRequest{
		Volume: bardplugin.VolumeRef{Instance: "remote", Location: "vg-targetd", Name: "bard-x"}, NewSizeBytes: 2 << 30,
	})
	if err != nil {
		t.Fatalf("repeated ExpandVolume to the same size must succeed (resizer retry): %v", err)
	}
	if again.CapacityBytes != 2<<30 {
		t.Fatalf("repeat expand should report the current size, got %+v", again)
	}
	if resizes != 1 {
		t.Fatalf("repeat expand must not re-issue vol_resize, got %d calls", resizes)
	}

	capResp, err := b.GetCapacity(context.Background(), &bardplugin.GetCapacityRequest{Instance: "remote"})
	if err != nil {
		t.Fatalf("GetCapacity: %v", err)
	}
	if capResp.AvailableBytes != 555 {
		t.Fatalf("unexpected GetCapacity response %+v", capResp)
	}
	if len(run.calls) != 0 {
		t.Fatalf("ExpandVolume/GetCapacity on a targetd instance must never invoke the local Runner, got %v", run.calls)
	}
}

// A targetd instance can be re-pointed to a different pool after volumes were
// created in the old one. Each volume's handle encodes the pool it was created
// in (tdLocation), so EVERY volume-scoped op -- delete, expand, publish,
// unpublish -- must act in the HANDLE's pool, not the instance's CURRENT pool.
// Routing by the current pool after a re-point would target a same-named volume
// in the wrong pool: a silent orphan on delete (the real volume never removed),
// or a wrong/failed expand or attach. Connection identity (endpoint, creds,
// target IQN) still comes from current config -- only the pool is handle-derived,
// mirroring GetVolumeHealth's branch.
func TestBackendTargetdVolumeOpsRouteByHandlePool(t *testing.T) {
	const handlePool = "old-pool" // where the volume was actually created

	seen := map[string]interface{}{}
	record := func(method string) func(map[string]interface{}) (interface{}, *tdRPCError) {
		return func(p map[string]interface{}) (interface{}, *tdRPCError) {
			seen[method] = p["pool"]
			switch method {
			case "vol_list":
				return []tdVolume{{Name: "bard-x", Size: 1 << 30}}, nil
			case "export_list":
				return []tdExport{}, nil
			}
			return nil, nil
		}
	}
	srv := newTdFakeServer(t)
	for _, m := range []string{"vol_destroy", "vol_list", "vol_resize", "export_list", "export_create", "export_destroy"} {
		srv.on(m, record(m))
	}
	httpSrv := srv.start()
	// wiredTargetdInst configures "remote" with TargetdPool "vg-targetd" -- the
	// pool the instance points at NOW, DIFFERENT from the handle's "old-pool".
	inst, dir := wiredTargetdInst(t, httpSrv.URL)
	run := &fakeRunner{}
	b := New(inst, "node-a", "", "", dir, "", run)

	handle := bardplugin.VolumeRef{Instance: "remote", Location: tdLocation(handlePool), Name: "bard-x"}

	if _, err := b.ExpandVolume(context.Background(), &bardplugin.ExpandVolumeRequest{Volume: handle, NewSizeBytes: 2 << 30}); err != nil {
		t.Fatalf("ExpandVolume: %v", err)
	}
	if _, err := b.ControllerPublish(context.Background(), &bardplugin.ControllerPublishRequest{Volume: handle, NodeID: "node-a"}); err != nil {
		t.Fatalf("ControllerPublish: %v", err)
	}
	if err := b.ControllerUnpublish(context.Background(), &bardplugin.ControllerUnpublishRequest{Volume: handle, NodeID: "node-a"}); err != nil {
		t.Fatalf("ControllerUnpublish: %v", err)
	}
	if err := b.DeleteVolume(context.Background(), &bardplugin.DeleteVolumeRequest{Volume: handle}); err != nil {
		t.Fatalf("DeleteVolume: %v", err)
	}

	// Every pool-scoped RPC must have been issued against the handle's pool.
	for _, m := range []string{"vol_list", "vol_resize", "export_create", "export_destroy", "vol_destroy"} {
		got, ok := seen[m]
		if !ok {
			t.Errorf("%s was never called -- op did not reach targetd", m)
			continue
		}
		if got != handlePool {
			t.Errorf("%s routed to pool %v, want the handle's pool %q (instance is re-pointed to vg-targetd)", m, got, handlePool)
		}
	}
	if len(run.calls) != 0 {
		t.Fatalf("targetd ops must never invoke the local Runner, got %v", run.calls)
	}
}

// ListVolumes must aggregate targetd instances alongside local ones with no
// changes to the local (VG-based) branch's behavior.
func TestBackendListVolumesTargetd(t *testing.T) {
	srv := newTdFakeServer(t)
	srv.on("vol_list", func(map[string]interface{}) (interface{}, *tdRPCError) {
		return []tdVolume{{Name: "bard-remote1", Size: 42}}, nil
	})
	httpSrv := srv.start()
	inst, dir := wiredTargetdInst(t, httpSrv.URL)
	run := &fakeRunner{}
	b := New(inst, "", "", "", dir, "", run)

	resp, err := b.ListVolumes(context.Background(), &bardplugin.ListVolumesRequest{})
	if err != nil {
		t.Fatalf("ListVolumes: %v", err)
	}
	if len(resp.Entries) != 1 || resp.Entries[0].Volume.Name != "bard-remote1" || resp.Entries[0].Volume.Instance != "remote" {
		t.Fatalf("unexpected ListVolumes entries: %+v", resp.Entries)
	}
}

// ControllerPublish/Unpublish on a targetd instance must grant/revoke through
// the tdManager and return a PublishContext shaped like the local path's
// (portal/targetIqn/lun), with the LUN reported by grantAccess.
func TestBackendControllerPublishAndUnpublishTargetd(t *testing.T) {
	srv := newTdFakeServer(t)
	srv.on("export_list", func(map[string]interface{}) (interface{}, *tdRPCError) {
		return []tdExport{}, nil
	})
	var exported map[string]interface{}
	srv.on("export_create", func(p map[string]interface{}) (interface{}, *tdRPCError) {
		exported = p
		return nil, nil
	})
	var revoked map[string]interface{}
	srv.on("export_destroy", func(p map[string]interface{}) (interface{}, *tdRPCError) {
		revoked = p
		return nil, nil
	})
	httpSrv := srv.start()
	inst, dir := wiredTargetdInst(t, httpSrv.URL)
	run := &fakeRunner{}
	b := New(inst, "", "", "", dir, "", run)

	pubResp, err := b.ControllerPublish(context.Background(), &bardplugin.ControllerPublishRequest{
		Volume: bardplugin.VolumeRef{Instance: "remote", Location: "vg-targetd", Name: "bard-x"},
		NodeID: "node-a",
	})
	if err != nil {
		t.Fatalf("ControllerPublish: %v", err)
	}
	pc := pubResp.PublishContext
	if pc[ctxPortal] != "10.0.0.5:3260" || pc[ctxIQN] != "iqn.2025-01.io.bard:remote" || pc[ctxLUN] != "0" {
		t.Fatalf("unexpected PublishContext: %+v", pc)
	}
	if _, ok := pc["userid"]; ok {
		t.Fatalf("PublishContext must never carry credentials, got %+v", pc)
	}
	if exported["initiator_wwn"] != "iqn.2025-01.io.bard:init-node-a" || exported["vol"] != "bard-x" {
		t.Fatalf("unexpected export_create params: %+v", exported)
	}

	if err := b.ControllerUnpublish(context.Background(), &bardplugin.ControllerUnpublishRequest{
		Volume: bardplugin.VolumeRef{Instance: "remote", Location: "vg-targetd", Name: "bard-x"},
		NodeID: "node-a",
	}); err != nil {
		t.Fatalf("ControllerUnpublish: %v", err)
	}
	if revoked["initiator_wwn"] != "iqn.2025-01.io.bard:init-node-a" || revoked["vol"] != "bard-x" {
		t.Fatalf("unexpected export_destroy params: %+v", revoked)
	}
	if len(run.calls) != 0 {
		t.Fatalf("ControllerPublish/Unpublish on a targetd instance must never invoke the local Runner, got %v", run.calls)
	}
}

// ---- concurrency: grantAccess/revokeAccess must be serialized -------------

// Two different volumes granted to the SAME initiator concurrently must never
// both compute the same "lowest unused LUN" -- core's inflight guard keys per
// VOLUME (internal/driver/inflight.go), not per initiator, so this is a real
// concurrent path (e.g. two ControllerPublish calls racing for one node). The
// fake server's export_list/export_create handlers share mutable state (its
// own mutex) and export_list sleeps briefly to widen any race window a
// missing client-side lock would otherwise hide.
func TestTdGrantAccessConcurrentDifferentVolumesGetDistinctLUNs(t *testing.T) {
	srv := newTdFakeServer(t)
	var stateMu sync.Mutex
	var exports []tdExport
	srv.on("export_list", func(map[string]interface{}) (interface{}, *tdRPCError) {
		stateMu.Lock()
		out := make([]tdExport, len(exports))
		copy(out, exports)
		stateMu.Unlock()
		time.Sleep(5 * time.Millisecond)
		return out, nil
	})
	srv.on("export_create", func(p map[string]interface{}) (interface{}, *tdRPCError) {
		lun, _ := p["lun"].(float64)
		stateMu.Lock()
		exports = append(exports, tdExport{
			InitiatorWWN: fmt.Sprintf("%v", p["initiator_wwn"]),
			LUN:          int(lun),
			VolName:      fmt.Sprintf("%v", p["vol"]),
		})
		stateMu.Unlock()
		return nil, nil
	})
	httpSrv := srv.start()
	td := newTestTdManager(httpSrv.URL, "pool1", "u", "p")

	vols := []string{"bard-a", "bard-b"}
	luns := make([]int, 2)
	errs := make([]error, 2)
	var wg sync.WaitGroup
	wg.Add(2)
	for i := 0; i < 2; i++ {
		go func(i int) {
			defer wg.Done()
			luns[i], errs[i] = td.grantAccess(context.Background(), vols[i], "iqn.shared", nil)
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("grantAccess[%d]: %v", i, err)
		}
	}
	if luns[0] == luns[1] {
		t.Fatalf("two different volumes granted to the same initiator concurrently must get DISTINCT LUNs, both got %d", luns[0])
	}

	final, err := td.listExports(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(final) != 2 {
		t.Fatalf("expected 2 exports to exist after both concurrent grants, got %d: %+v", len(final), final)
	}
}

// ---- missing-instance guard is MARKER-based, not blanket ------------------
//
// A volume's own recorded Location tells DeleteVolume/ControllerUnpublish
// whether an unknown instance's volume was ever targetd-managed (see
// tdLocation/isTdLocation in targetd.go): a MARKED Location needs the
// instance's endpoint+creds to reach remotely, which only live in CURRENT
// config, so there is nothing to derive -- refuse to guess (CodeInternal,
// retriable). An UNMARKED Location is a local (or pre-targetd) volume, whose
// derived-cleanup fallback is the PRE-EXISTING, deliberate design (the
// handle's own Location IS the VG) -- see
// TestControllerUnpublishUnknownInstanceStillRevokes (snapchap_test.go) and
// TestDeleteVolumeUnknownInstanceUnmarkedLocationAttemptsLocalCleanup below.

// A volume whose recorded Location is MARKED as targetd-managed needs the
// instance's endpoint+creds to reach it, which only live in current config --
// DeleteVolume must refuse to guess (CodeInternal, retriable) rather than
// silently no-op against local objects that never existed (targetcli/lvremove
// against a name that only exists on a remote targetd host all classify as
// not-found -- a real orphan reported as a clean delete).
func TestDeleteVolumeMarkedTargetdLocationMissingInstanceRejected(t *testing.T) {
	run := &fakeRunner{}
	b := New(eastInst(), "", "", "", "", "", run) // only "east" is configured
	err := b.DeleteVolume(context.Background(), &bardplugin.DeleteVolumeRequest{
		Volume: bardplugin.VolumeRef{Instance: "ghost", Location: tdLocation("vg-targetd"), Name: "bard-x"},
	})
	var se *bardplugin.StatusError
	if err == nil || !errors.As(err, &se) || se.Code != bardplugin.CodeInternal {
		t.Fatalf("DeleteVolume for a MARKED targetd location with an unknown instance must fail with CodeInternal, got %v", err)
	}
	if len(run.calls) != 0 {
		t.Fatalf("must never invoke the local Runner for a marked targetd location, got %v", run.calls)
	}
}

// Same reasoning as the DeleteVolume case above, for ControllerUnpublish.
func TestControllerUnpublishMarkedTargetdLocationMissingInstanceRejected(t *testing.T) {
	run := &fakeRunner{}
	b := New(eastInst(), "", "", "", "", "", run)
	err := b.ControllerUnpublish(context.Background(), &bardplugin.ControllerUnpublishRequest{
		Volume: bardplugin.VolumeRef{Instance: "ghost", Location: tdLocation("vg-targetd"), Name: "bard-x"},
		NodeID: "node-a",
	})
	var se *bardplugin.StatusError
	if err == nil || !errors.As(err, &se) || se.Code != bardplugin.CodeInternal {
		t.Fatalf("ControllerUnpublish for a MARKED targetd location with an unknown instance must fail with CodeInternal, got %v", err)
	}
	if len(run.calls) != 0 {
		t.Fatalf("must never invoke the local Runner for a marked targetd location, got %v", run.calls)
	}
}

// Pins the previously-IMPLICIT local design (explicit now that a marker
// exists to route on): an UNMARKED Location for an unknown instance is
// best-effort DERIVED cleanup, not an error -- the handle's own Location IS
// the VG, so lvremove genuinely targets it even with no instance config at
// all (mirrors NodeUnstage's documented derived-logout fallback, and
// ControllerUnpublish's identical, original
// TestControllerUnpublishUnknownInstanceStillRevokes in snapchap_test.go).
func TestDeleteVolumeUnknownInstanceUnmarkedLocationAttemptsLocalCleanup(t *testing.T) {
	fr := &fakeRunner{}
	b := New(map[string]InstanceConfig{}, "", "", "", "", "", fr) // nothing configured
	lv := lvName("pvc-1")
	if err := b.DeleteVolume(context.Background(), &bardplugin.DeleteVolumeRequest{
		Volume: bardplugin.VolumeRef{Instance: "gone", Location: "bard-vg", Name: lv},
	}); err != nil {
		t.Fatalf("delete of an unmarked/unknown-instance volume must attempt derived local cleanup, not error: %v", err)
	}
	if !fr.ran("lvremove", "bard-vg/"+lv) {
		t.Fatalf("expected a derived lvremove against the handle's own Location (VG); calls %v", fr.calls)
	}
	if !fr.ran("targetcli", "/iscsi", "delete", targetIQN(defaultIQNBase, lv)) {
		t.Fatalf("expected a derived target teardown using the default IQN base; calls %v", fr.calls)
	}
}
