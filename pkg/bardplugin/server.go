package bardplugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
)

// Serve runs a plugin Backend as an HTTP+JSON server on a unix socket until ctx
// is cancelled. A stale socket file at socketPath is removed first. This is all
// a Go plugin's main() needs:
//
//	func main() {
//	    bardplugin.Serve(context.Background(), "/var/lib/bard/plugins/nfs.sock", &nfsBackend{})
//	}
func Serve(ctx context.Context, socketPath string, b Backend) error {
	if err := os.Remove(socketPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("bardplugin: remove stale socket: %w", err)
	}
	lis, err := net.Listen("unix", socketPath)
	if err != nil {
		return fmt.Errorf("bardplugin: listen %s: %w", socketPath, err)
	}
	// Make the socket connectable by core even when core and the plugin run as
	// different UIDs. net.Listen creates it ~0755 (minus umask); connect() needs
	// write on the socket, so a core under a different, non-root UID gets EACCES.
	// That happens with a hardened/distroless base whose default user is nonroot
	// (e.g. cgr.dev/chainguard/static is 65532) dialing a plugin that runs as root.
	//
	// 0660, deliberately NOT 0666: the socket drives the full plugin API (create/
	// delete/mount), so it must never be world-writable. The pod sets a shared
	// fsGroup; the plugins dir is a per-pod emptyDir, which Kubernetes chowns to
	// that fsGroup with the setgid bit, so this socket is created group-owned by
	// fsGroup and every container in the pod (which gets fsGroup as a supplemental
	// group) can connect -- while "other" (a non-pod, non-root principal, e.g. were
	// the dir ever a shared hostPath) cannot. Node-root can always reach it, but
	// node-root is already omnipotent. See docs/hardened-images.md.
	if err := os.Chmod(socketPath, 0o660); err != nil {
		return fmt.Errorf("bardplugin: chmod socket %s: %w", socketPath, err)
	}
	srv := &http.Server{Handler: newMux(b)}
	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()
	if err := srv.Serve(lis); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func newMux(b Backend) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(PathInfo, func(w http.ResponseWriter, r *http.Request) {
		info := b.Info()
		if info.ContractVersion == "" {
			info.ContractVersion = ContractVersion
		}
		// Advertise optional capabilities from the interfaces the backend
		// implements, so a plugin author only implements CapacityReporter.
		if _, ok := b.(CapacityReporter); ok {
			info.Capabilities.GetCapacity = true
		}
		if _, ok := b.(HealthReporter); ok {
			info.Capabilities.VolumeHealth = true
		}
		if _, ok := b.(VolumeModifier); ok {
			info.Capabilities.ModifyVolume = true
		}
		if _, ok := b.(SpaceReclaimer); ok {
			info.Capabilities.ReclaimSpace = true
		}
		if _, ok := b.(NodeSpaceReclaimer); ok {
			info.Capabilities.NodeReclaimSpace = true
		}
		if _, ok := b.(ControllerPublisher); ok {
			info.Capabilities.RequiresControllerPublish = true
		}
		if _, ok := b.(VolumeLister); ok {
			info.Capabilities.ListVolumes = true
		}
		if _, ok := b.(SnapshotLister); ok {
			info.Capabilities.ListSnapshots = true
		}
		if _, ok := b.(NetworkFencer); ok {
			info.Capabilities.NetworkFence = true
		}
		if _, ok := b.(VolumeReplicator); ok {
			info.Capabilities.Replication = true
		}
		if _, ok := b.(EncryptionKeyRotator); ok {
			info.Capabilities.EncryptionKeyRotation = true
		}
		if _, ok := b.(VolumeGrouper); ok {
			info.Capabilities.VolumeGroup = true
		}
		writeJSON(w, info)
	})
	handle(mux, PathCreateVolume, func(ctx context.Context, req *CreateVolumeRequest) (any, error) {
		return b.CreateVolume(ctx, req)
	})
	handle(mux, PathDeleteVolume, func(ctx context.Context, req *DeleteVolumeRequest) (any, error) {
		return empty{}, b.DeleteVolume(ctx, req)
	})
	handle(mux, PathExpandVolume, func(ctx context.Context, req *ExpandVolumeRequest) (any, error) {
		return b.ExpandVolume(ctx, req)
	})
	handle(mux, PathCreateSnapshot, func(ctx context.Context, req *CreateSnapshotRequest) (any, error) {
		return b.CreateSnapshot(ctx, req)
	})
	handle(mux, PathDeleteSnapshot, func(ctx context.Context, req *DeleteSnapshotRequest) (any, error) {
		return empty{}, b.DeleteSnapshot(ctx, req)
	})
	handle(mux, PathNodeStage, func(ctx context.Context, req *NodeStageRequest) (any, error) {
		return empty{}, b.NodeStage(ctx, req)
	})
	handle(mux, PathNodeUnstage, func(ctx context.Context, req *NodeUnstageRequest) (any, error) {
		return empty{}, b.NodeUnstage(ctx, req)
	})
	handle(mux, PathNodePublish, func(ctx context.Context, req *NodePublishRequest) (any, error) {
		return empty{}, b.NodePublish(ctx, req)
	})
	handle(mux, PathNodeUnpublish, func(ctx context.Context, req *NodeUnpublishRequest) (any, error) {
		return empty{}, b.NodeUnpublish(ctx, req)
	})
	handle(mux, PathNodeExpand, func(ctx context.Context, req *NodeExpandRequest) (any, error) {
		return b.NodeExpand(ctx, req)
	})
	// Optional: only wired when the backend implements CapacityReporter.
	if cr, ok := b.(CapacityReporter); ok {
		handle(mux, PathGetCapacity, func(ctx context.Context, req *GetCapacityRequest) (any, error) {
			return cr.GetCapacity(ctx, req)
		})
	}
	if hr, ok := b.(HealthReporter); ok {
		handle(mux, PathVolumeHealth, func(ctx context.Context, req *GetVolumeHealthRequest) (any, error) {
			return hr.GetVolumeHealth(ctx, req)
		})
	}
	if vm, ok := b.(VolumeModifier); ok {
		handle(mux, PathModifyVolume, func(ctx context.Context, req *ModifyVolumeRequest) (any, error) {
			return vm.ModifyVolume(ctx, req)
		})
	}
	if sr, ok := b.(SpaceReclaimer); ok {
		handle(mux, PathReclaimSpace, func(ctx context.Context, req *ReclaimSpaceRequest) (any, error) {
			return sr.ReclaimSpace(ctx, req)
		})
	}
	if nsr, ok := b.(NodeSpaceReclaimer); ok {
		handle(mux, PathNodeReclaimSpace, func(ctx context.Context, req *NodeReclaimSpaceRequest) (any, error) {
			return nsr.NodeReclaimSpace(ctx, req)
		})
	}
	if cp, ok := b.(ControllerPublisher); ok {
		handle(mux, PathControllerPublish, func(ctx context.Context, req *ControllerPublishRequest) (any, error) {
			return cp.ControllerPublish(ctx, req)
		})
		handle(mux, PathControllerUnpublish, func(ctx context.Context, req *ControllerUnpublishRequest) (any, error) {
			return empty{}, cp.ControllerUnpublish(ctx, req)
		})
	}
	if vl, ok := b.(VolumeLister); ok {
		handle(mux, PathListVolumes, func(ctx context.Context, req *ListVolumesRequest) (any, error) {
			return vl.ListVolumes(ctx, req)
		})
	}
	if sl, ok := b.(SnapshotLister); ok {
		handle(mux, PathListSnapshots, func(ctx context.Context, req *ListSnapshotsRequest) (any, error) {
			return sl.ListSnapshots(ctx, req)
		})
	}
	if nf, ok := b.(NetworkFencer); ok {
		handle(mux, PathFenceClusterNetwork, func(ctx context.Context, req *FenceClusterNetworkRequest) (any, error) {
			return empty{}, nf.FenceClusterNetwork(ctx, req)
		})
		handle(mux, PathUnfenceClusterNetwork, func(ctx context.Context, req *UnfenceClusterNetworkRequest) (any, error) {
			return empty{}, nf.UnfenceClusterNetwork(ctx, req)
		})
		handle(mux, PathListClusterFence, func(ctx context.Context, req *ListClusterFenceRequest) (any, error) {
			return nf.ListClusterFence(ctx, req)
		})
		handle(mux, PathGetFenceClients, func(ctx context.Context, req *GetFenceClientsRequest) (any, error) {
			return nf.GetFenceClients(ctx, req)
		})
	}
	if vr, ok := b.(VolumeReplicator); ok {
		handle(mux, PathEnableReplication, func(ctx context.Context, req *EnableReplicationRequest) (any, error) {
			return empty{}, vr.EnableVolumeReplication(ctx, req)
		})
		handle(mux, PathDisableReplication, func(ctx context.Context, req *DisableReplicationRequest) (any, error) {
			return empty{}, vr.DisableVolumeReplication(ctx, req)
		})
		handle(mux, PathPromoteVolume, func(ctx context.Context, req *PromoteVolumeRequest) (any, error) {
			return empty{}, vr.PromoteVolume(ctx, req)
		})
		handle(mux, PathDemoteVolume, func(ctx context.Context, req *DemoteVolumeRequest) (any, error) {
			return empty{}, vr.DemoteVolume(ctx, req)
		})
		handle(mux, PathResyncVolume, func(ctx context.Context, req *ResyncVolumeRequest) (any, error) {
			return vr.ResyncVolume(ctx, req)
		})
		handle(mux, PathReplicationInfo, func(ctx context.Context, req *ReplicationInfoRequest) (any, error) {
			return vr.GetVolumeReplicationInfo(ctx, req)
		})
	}
	if kr, ok := b.(EncryptionKeyRotator); ok {
		handle(mux, PathRotateEncryptionKey, func(ctx context.Context, req *RotateEncryptionKeyRequest) (any, error) {
			return empty{}, kr.RotateEncryptionKey(ctx, req)
		})
	}
	if vg, ok := b.(VolumeGrouper); ok {
		handle(mux, PathCreateVolumeGroup, func(ctx context.Context, req *CreateVolumeGroupRequest) (any, error) {
			return vg.CreateVolumeGroup(ctx, req)
		})
		handle(mux, PathModifyVolumeGroup, func(ctx context.Context, req *ModifyVolumeGroupRequest) (any, error) {
			return vg.ModifyVolumeGroup(ctx, req)
		})
		handle(mux, PathDeleteVolumeGroup, func(ctx context.Context, req *DeleteVolumeGroupRequest) (any, error) {
			return empty{}, vg.DeleteVolumeGroup(ctx, req)
		})
		handle(mux, PathGetVolumeGroup, func(ctx context.Context, req *GetVolumeGroupRequest) (any, error) {
			return vg.GetVolumeGroup(ctx, req)
		})
		handle(mux, PathListVolumeGroups, func(ctx context.Context, req *ListVolumeGroupsRequest) (any, error) {
			return vg.ListVolumeGroups(ctx, req)
		})
	}
	return mux
}

type empty struct{}

// maxRequestBody caps a decoded request body. The only client is the core driver
// over the local unix socket, so this is defense-in-depth (a buggy or compromised
// caller cannot exhaust node memory with an unbounded body); CSI requests --
// volume contexts, secrets, topology -- are kilobytes, so 1 MiB is generous.
const maxRequestBody = 1 << 20

// handle wires a typed request->response func to a path, decoding the JSON body
// and encoding the result or a structured Error.
func handle[Req any](mux *http.ServeMux, path string, fn func(context.Context, *Req) (any, error)) {
	mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
		var req Req
		if r.Body != nil {
			if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBody)).Decode(&req); err != nil {
				writeErr(w, &StatusError{Code: CodeInvalidArg, Message: "decode request: " + err.Error()})
				return
			}
		}
		resp, err := fn(r.Context(), &req)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, resp)
	})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, err error) {
	e := Error{Code: CodeInternal, Message: err.Error()}
	var se *StatusError
	if errors.As(err, &se) {
		e.Code = se.Code
		e.Message = se.Message
	}
	status := http.StatusInternalServerError
	switch e.Code {
	case CodeNotFound:
		status = http.StatusNotFound
	case CodeAlreadyExists:
		status = http.StatusConflict
	case CodeInvalidArg:
		status = http.StatusBadRequest
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(e)
}
