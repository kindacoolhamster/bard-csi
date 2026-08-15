package metrics

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
)

// A panicking handler must still decrement the in-flight gauge. Before the
// decrement was deferred, a single panicking RPC raised the gauge permanently,
// so the "in-flight requests" panel drifted upward forever and never came back
// down -- and it looked like a load problem rather than a crash.
func TestInterceptorInFlightSurvivesPanic(t *testing.T) {
	before := def.inFlight.Load()

	func() {
		defer func() {
			if recover() == nil {
				t.Fatalf("expected the handler panic to propagate")
			}
		}()
		_, _ = Interceptor(context.Background(), nil,
			&grpc.UnaryServerInfo{FullMethod: "/m/Panic"},
			func(context.Context, any) (any, error) { panic("boom") },
		)
	}()

	if got := def.inFlight.Load(); got != before {
		t.Fatalf("in-flight leaked across a panic: before=%d after=%d", before, got)
	}
}

// blockingWriter blocks in Write until released, standing in for a stalled
// Prometheus scrape client.
type blockingWriter struct {
	entered chan struct{}
	release chan struct{}
	once    bool
}

func (b *blockingWriter) Write(p []byte) (int, error) {
	if !b.once {
		b.once = true
		close(b.entered)
		<-b.release
	}
	return len(p), nil
}

// Exposition must not hold the collector lock while writing to the client.
// It previously did, so one hung scrape blocked every observe() call -- and
// observe() runs inside the gRPC interceptor, meaning a stuck Prometheus
// connection could wedge the driver's request handling.
func TestWriteDoesNotBlockObservation(t *testing.T) {
	c := newCollector()
	c.observe("/m/Seed", "OK", time.Millisecond)

	bw := &blockingWriter{entered: make(chan struct{}), release: make(chan struct{})}
	go c.write(bw)

	select {
	case <-bw.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("writer never entered Write")
	}

	// The scrape is now parked mid-write. Recording must still complete.
	done := make(chan struct{})
	go func() {
		c.observe("/m/During", "OK", time.Millisecond)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		close(bw.release)
		t.Fatal("observe() blocked while a scrape was in progress -- the collector lock is held across the write")
	}
	close(bw.release)
}

// Plugin calls are recorded per backend/instance/endpoint/outcome -- the labels
// that make Bard's multi-backend dispatch observable.
func TestPluginCallExposition(t *testing.T) {
	c := newCollector()
	// Swap the package collector so the exported recorder writes to ours.
	old := def
	def = c
	defer func() { def = old }()

	ObservePluginCall("ceph-rbd", "galileo", "/volume/create", ResultSuccess, 20*time.Millisecond)
	ObservePluginCall("ceph-rbd", "galileo", "/volume/create", ResultAlreadyExists, 5*time.Millisecond)
	ObservePluginCall("ceph-rbd", "", "/volume/list", ResultSuccess, time.Millisecond)

	release := PluginCallStarted("ceph-rbd")

	var sb strings.Builder
	c.write(&sb)
	out := sb.String()

	for _, want := range []string{
		`bard_csi_plugin_requests_total{backend="ceph-rbd",instance="galileo",endpoint="/volume/create",result="success"} 1`,
		`bard_csi_plugin_requests_total{backend="ceph-rbd",instance="galileo",endpoint="/volume/create",result="already_exists"} 1`,
		// An empty instance on a cross-instance route renders as "all", not "".
		`bard_csi_plugin_requests_total{backend="ceph-rbd",instance="all",endpoint="/volume/list",result="success"} 1`,
		`bard_csi_plugin_request_duration_seconds_count{backend="ceph-rbd",instance="galileo",endpoint="/volume/create"} 2`,
		`bard_csi_plugin_requests_in_flight{backend="ceph-rbd"} 1`,
		"# TYPE bard_csi_plugin_request_duration_seconds histogram",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("exposition missing %q\n---\n%s", want, out)
		}
	}

	release()
	sb.Reset()
	c.write(&sb)
	if !strings.Contains(sb.String(), `bard_csi_plugin_requests_in_flight{backend="ceph-rbd"} 0`) {
		t.Fatalf("in-flight did not return to 0\n---\n%s", sb.String())
	}
}

// Placements carry the dispatch decision, so a fleet silently falling back to
// the default instance is distinguishable from topology actually routing.
func TestPlacementExposition(t *testing.T) {
	c := newCollector()
	old := def
	def = c
	defer func() { def = old }()

	ObservePlacement("ceph-rbd", "galileo", "zone-a", "preferred", ResultSuccess)
	ObservePlacement("ceph-rbd", "galileo", "", "default", ResultSuccess)
	ObservePlacement("lvm", InstanceNone, ZoneNone, DecisionUnresolved, PlacementDispatchError)

	var sb strings.Builder
	c.write(&sb)
	out := sb.String()

	for _, want := range []string{
		`bard_csi_volume_placement_attempts_total{backend="ceph-rbd",instance="galileo",zone="zone-a",decision="preferred",result="success"} 1`,
		// An instance serving no zone is labelled explicitly, never "".
		`bard_csi_volume_placement_attempts_total{backend="ceph-rbd",instance="galileo",zone="none",decision="default",result="success"} 1`,
		`bard_csi_volume_placement_attempts_total{backend="lvm",instance="none",zone="none",decision="unresolved",result="dispatch_error"} 1`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("exposition missing %q\n---\n%s", want, out)
		}
	}
}

// Label values are escaped per the exposition format's three defined escapes,
// so a hostile or merely odd instance id cannot produce unparseable output.
func TestLabelEscaping(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{`plain`, `plain`},
		{`has"quote`, `has\"quote`},
		{`has\backslash`, `has\\backslash`},
		{"has\nnewline", `has\nnewline`},
	} {
		if got := escLabel(tc.in); got != tc.want {
			t.Errorf("escLabel(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}

	c := newCollector()
	c.observe(`/m/A"B`, "OK", time.Millisecond)
	var sb strings.Builder
	c.write(&sb)
	if !strings.Contains(sb.String(), `method="/m/A\"B"`) {
		t.Fatalf("quote not escaped in exposition\n---\n%s", sb.String())
	}
}

// Histogram buckets are cumulative and consistent with _count.
func TestHistogramCumulative(t *testing.T) {
	c := newCollector()
	for _, d := range []time.Duration{time.Millisecond, 100 * time.Millisecond, 3 * time.Second} {
		c.observe("/m/H", "OK", d)
	}
	var sb strings.Builder
	c.write(&sb)
	out := sb.String()

	// 1ms <= .005 so every bucket from .005 up includes it; by +Inf all three.
	for _, want := range []string{
		`bard_csi_grpc_request_duration_seconds_bucket{method="/m/H",le="0.005"} 1`,
		`bard_csi_grpc_request_duration_seconds_bucket{method="/m/H",le="0.25"} 2`,
		`bard_csi_grpc_request_duration_seconds_bucket{method="/m/H",le="5"} 3`,
		`bard_csi_grpc_request_duration_seconds_bucket{method="/m/H",le="+Inf"} 3`,
		`bard_csi_grpc_request_duration_seconds_count{method="/m/H"} 3`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("exposition missing %q\n---\n%s", want, out)
		}
	}
}

// Plugin calls use a much longer bucket set than the gRPC ones. Backend work is
// legitimately slow (`rbd sparsify`, a dm-integrity wipe) and the client carries
// no flat timeout, so with the gRPC buckets' 10s ceiling every such call lands in
// +Inf and histogram_quantile reports exactly 10s -- whether the truth is 11
// seconds or eleven minutes.
func TestPluginHistogramResolvesBeyondTenSeconds(t *testing.T) {
	c := newCollector()
	old := def
	def = c
	defer func() { def = old }()

	ObservePluginCall("ceph-rbd", "galileo", "/volume/reclaimspace", ResultSuccess, 120*time.Second)

	var sb strings.Builder
	c.write(&sb)
	out := sb.String()

	// 120s must be distinguishable: above the 60 bound, at or below 180.
	for _, want := range []string{
		`bard_csi_plugin_request_duration_seconds_bucket{backend="ceph-rbd",instance="galileo",endpoint="/volume/reclaimspace",le="60"} 0`,
		`bard_csi_plugin_request_duration_seconds_bucket{backend="ceph-rbd",instance="galileo",endpoint="/volume/reclaimspace",le="180"} 1`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("exposition missing %q\n---\n%s", want, out)
		}
	}

	// The gRPC families must keep their original, shorter bounds.
	c.observe("/m/G", "OK", time.Millisecond)
	sb.Reset()
	c.write(&sb)
	if strings.Contains(sb.String(), `bard_csi_grpc_request_duration_seconds_bucket{method="/m/G",le="180"}`) {
		t.Fatal("gRPC histogram picked up the plugin bucket set")
	}
}

var _ io.Writer = (*blockingWriter)(nil)
