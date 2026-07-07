package driver

import (
	"sync"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// inflight tracks objects with an operation in progress, so a duplicate request
// (a CO retry racing the still-running original after a deadline expiry) is
// refused with Aborted instead of running concurrently against the same object.
// CSI requires idempotency, and the backends provide it for *sequential*
// retries -- but two concurrent identical ops can interleave their backend
// commands (a clone's snap create/clone/rm, LUKS format/open, map/unmap) in
// ways no sequential retry ever sees. Aborted is the CSI-idiomatic answer: the
// sidecars back off and retry, and the retry lands after the original finishes
// (ceph-csi's VolumeLocks behave the same way).
type inflight struct {
	mu  sync.Mutex
	ops map[string]struct{}
}

func newInflight() *inflight { return &inflight{ops: map[string]struct{}{}} }

// tryLock claims key, reporting false when an operation on it is already running.
func (l *inflight) tryLock(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, busy := l.ops[key]; busy {
		return false
	}
	l.ops[key] = struct{}{}
	return true
}

func (l *inflight) unlock(key string) {
	l.mu.Lock()
	delete(l.ops, key)
	l.mu.Unlock()
}

// claim guards an operation on an object, returning Aborted while another
// operation on the same object is in flight. kind namespaces the key space
// (volume ids and CO-chosen names must not collide). On success the caller
// must defer the release func.
func (d *Driver) claim(kind, key string) (func(), error) {
	k := kind + ":" + key
	if !d.ops.tryLock(k) {
		return nil, status.Errorf(codes.Aborted, "an operation on %s is already in progress", k)
	}
	return func() { d.ops.unlock(k) }, nil
}
