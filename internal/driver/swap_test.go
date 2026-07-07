package driver

import (
	"testing"

	"github.com/kindacoolhamster/bard-csi/internal/backend"
	"github.com/kindacoolhamster/bard-csi/internal/dispatch"
)

func mustDisp(t *testing.T) *dispatch.Dispatcher {
	t.Helper()
	d, err := dispatch.New(dispatch.Config{Instances: map[string]map[string]string{}})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// TestSetBackendsSwaps verifies SetBackends atomically replaces the registry +
// dispatcher snapshot the CSI services resolve through (the live-reload path).
func TestSetBackendsSwaps(t *testing.T) {
	r1, d1 := backend.NewRegistry(), mustDisp(t)
	drv := New(Options{Registry: r1, Dispatch: d1})

	if got := drv.snapshot(); got.registry != r1 || got.disp != d1 {
		t.Fatal("initial snapshot does not match Options")
	}

	r2, d2 := backend.NewRegistry(), mustDisp(t)
	drv.SetBackends(r2, d2)

	got := drv.snapshot()
	if got.registry != r2 || got.disp != d2 {
		t.Fatal("snapshot did not swap to the new registry/dispatcher")
	}
	if got.registry == r1 || got.disp == d1 {
		t.Fatal("snapshot still points at the old pair")
	}
}
