package driver

import (
	"context"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/container-storage-interface/spec/lib/go/csi"
)

// A duplicate request for an object with an operation still in flight must be
// refused with Aborted (the CO backs off and retries after the original
// finishes), and the object must be claimable again once the operation returns.
func TestDuplicateInFlightOperationAborts(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	cs, id := controllerWith(t, &fakeBackend{
		expand: func() (int64, bool, error) {
			close(started)
			<-release
			return 2 << 30, true, nil
		},
	})
	req := &csi.ControllerExpandVolumeRequest{
		VolumeId:      id,
		CapacityRange: &csi.CapacityRange{RequiredBytes: 2 << 30},
	}

	firstDone := make(chan error, 1)
	go func() {
		_, err := cs.ControllerExpandVolume(context.Background(), req)
		firstDone <- err
	}()
	<-started

	// The duplicate must not reach the backend: Aborted, immediately.
	if _, err := cs.ControllerExpandVolume(context.Background(), req); status.Code(err) != codes.Aborted {
		t.Fatalf("duplicate in-flight expand must be Aborted, got %v", err)
	}
	// A different object is unaffected.
	if _, err := cs.DeleteVolume(context.Background(), &csi.DeleteVolumeRequest{VolumeId: id + "x"}); err != nil {
		t.Fatalf("an operation on another object must not be blocked: %v", err)
	}

	close(release)
	if err := <-firstDone; err != nil {
		t.Fatalf("original operation must succeed: %v", err)
	}
	// The object is free again: the retry goes through. (Re-arm the channels the
	// expand closure captured so it doesn't re-close a closed channel or block.)
	started = make(chan struct{}, 1)
	release = make(chan struct{})
	close(release)
	if _, err := cs.ControllerExpandVolume(context.Background(), req); err != nil {
		t.Fatalf("retry after the original finished must succeed: %v", err)
	}
}

func TestInflightTryLock(t *testing.T) {
	l := newInflight()
	if !l.tryLock("a") {
		t.Fatal("first claim must succeed")
	}
	if l.tryLock("a") {
		t.Fatal("second claim of a held key must fail")
	}
	if !l.tryLock("b") {
		t.Fatal("an unrelated key must be claimable")
	}
	l.unlock("a")
	if !l.tryLock("a") {
		t.Fatal("a released key must be claimable again")
	}
}
