// Package metrics exposes Prometheus-format metrics for the Bard CSI driver over
// a stdlib HTTP server -- deliberately NO prometheus/client_golang dependency, the
// same lean-image discipline as the rest of the driver (hand-rolled SigV4, KMIP as
// the only sanctioned dep).
//
// Three groups are recorded:
//
//   - gRPC serving metrics, via a unary interceptor: per-method request counts
//     (labelled by gRPC status code), handler latency, and in-flight requests.
//     The interceptor is chained onto all three gRPC servers (controller, node,
//     csi-addons).
//   - Plugin client metrics, recorded at the single core->plugin socket
//     chokepoint: per backend/instance/endpoint latency and outcome. This is the
//     only place the *instance* is visible, which is what makes Bard's
//     multi-backend dispatch observable at all. Note these measure the plugin
//     HTTP round trip, NOT the backend command inside the plugin -- a plugin that
//     blocks on its own lock for 8s and runs `rbd` for 100ms reports 8.1s here.
//   - Volume placement, recorded at the CreateVolume call site (deliberately NOT
//     inside dispatch.Resolve, which GetCapacity also calls -- capacity polling
//     would otherwise dominate a metric that is supposed to mean "a volume was
//     placed here").
//
// Exposition snapshots under the lock and formats outside it: a slow or hung
// scrape client must never block the recording path, which runs inside gRPC
// handlers.
package metrics

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/status"
)

// bucketLe are the gRPC histogram upper bounds (seconds), cumulative per Prometheus.
var bucketLe = []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10}

// pluginBucketLe extends much further than the gRPC buckets on purpose. Backend
// work is legitimately long here -- `rbd sparsify` on a large image, a
// dm-integrity wipe, a full targetd copy -- and the client deliberately carries
// no flat timeout so those can run to completion. With a 10s top bucket every
// such call lands in +Inf, and histogram_quantile then reports the last finite
// bound: a p99 pinned at exactly 10s whether the real value is 11 seconds or
// eleven minutes.
var pluginBucketLe = []float64{.005, .025, .1, .25, .5, 1, 2.5, 5, 10, 30, 60, 180, 600}

// Plugin call outcomes. Bounded by construction so the label stays low
// cardinality. The mapped-sentinel outcomes are split out from the generic
// error on purpose: NotFound on a delete and AlreadyExists on a retried create
// are *normal* in CSI's idempotency flows, so folding them into "error" would
// leave a dashboard's error-rate panel permanently red.
const (
	ResultSuccess       = "success"
	ResultNotFound      = "not_found"
	ResultAlreadyExists = "already_exists"
	ResultInvalidArg    = "invalid_argument"
	ResultUnsupported   = "unsupported"
	// ResultFailedPrecondition is a well-formed request the backend refused
	// because of cluster state the operator must change (e.g. a Ceph too old for
	// group-member restore). Split out because, unlike plugin_error, retrying it
	// unchanged will never succeed.
	ResultFailedPrecondition = "failed_precondition"
	ResultPluginError        = "plugin_error"
	ResultTransport          = "transport_error"
	ResultTimeout            = "timeout"
	ResultCanceled           = "canceled"
	ResultDecodeError        = "decode_error"
)

// InstanceAll is the instance label for calls that are genuinely cross-instance
// (ListVolumes/ListSnapshots/ListVolumeGroups, and the startup /info probe). An
// empty label value would read as broken instrumentation rather than as scope.
const InstanceAll = "all"

// Placement outcomes and the labels used when dispatch never got far enough to
// name an instance.
const (
	PlacementError         = "error"          // an instance was chosen, the attempt then failed
	PlacementDispatchError = "dispatch_error" // dispatch could not choose an instance at all
	DecisionUnresolved     = "unresolved"
	InstanceNone           = "none"
	ZoneNone               = "none"
	// BackendUnknown keeps the backend label bounded when the requested type is
	// not a configured one. The StorageClass parameter is caller-controlled, so
	// feeding it straight into a label lets a typo mint a permanent new series.
	BackendUnknown = "unknown"
)

// histogram is a fixed-bucket cumulative histogram. Not safe for concurrent use;
// the collector's mutex guards every access.
type histogram struct {
	le      []float64 // upper bounds this histogram was built with
	buckets []uint64  // len(le); buckets[i] = #observations <= le[i]
	sum     float64   // total seconds
	count   uint64
}

func newHistogram(le []float64) *histogram {
	return &histogram{le: le, buckets: make([]uint64, len(le))}
}

func (h *histogram) observe(seconds float64) {
	h.sum += seconds
	h.count++
	for i, le := range h.le {
		if seconds <= le {
			h.buckets[i]++
		}
	}
}

func (h *histogram) clone() *histogram {
	return &histogram{le: h.le, buckets: append([]uint64(nil), h.buckets...), sum: h.sum, count: h.count}
}

type codeKey struct{ method, code string }
type pluginKey struct{ backend, instance, endpoint string }
type pluginResultKey struct{ backend, instance, endpoint, result string }
type placementKey struct{ backend, instance, zone, decision, result string }

type collector struct {
	mu       sync.Mutex
	byMethod map[string]*histogram
	byCode   map[codeKey]uint64

	pluginDur      map[pluginKey]*histogram
	pluginRes      map[pluginResultKey]uint64
	pluginInFlight map[string]int64

	placements map[placementKey]uint64

	inFlight atomic.Int64
}

func newCollector() *collector {
	return &collector{
		byMethod:       map[string]*histogram{},
		byCode:         map[codeKey]uint64{},
		pluginDur:      map[pluginKey]*histogram{},
		pluginRes:      map[pluginResultKey]uint64{},
		pluginInFlight: map[string]int64{},
		placements:     map[placementKey]uint64{},
	}
}

var def = newCollector()

func (c *collector) observe(method, code string, d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.byCode[codeKey{method, code}]++
	ms := c.byMethod[method]
	if ms == nil {
		ms = newHistogram(bucketLe)
		c.byMethod[method] = ms
	}
	ms.observe(d.Seconds())
}

// Interceptor is a grpc.UnaryServerInterceptor that records call count, latency and
// in-flight gauge for every RPC. Recording is in-memory and cheap, so it is always
// chained; the data is only exposed when Serve is started.
func Interceptor(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	def.inFlight.Add(1)
	// Deferred so a panicking handler cannot leak the gauge upward forever --
	// the panic propagates to grpc's recovery, but the counter must still fall.
	defer def.inFlight.Add(-1)
	start := time.Now()
	resp, err := handler(ctx, req)
	def.observe(info.FullMethod, status.Code(err).String(), time.Since(start))
	return resp, err
}

// ObservePluginCall records one completed core->plugin socket call. instance is
// the concrete backend instance the call targeted, or InstanceAll for the
// cross-instance list calls. endpoint must be a fixed contract route (a
// bardplugin.Path* constant), never an interpolated string.
func ObservePluginCall(backendType, instance, endpoint, result string, d time.Duration) {
	if instance == "" {
		instance = InstanceAll
	}
	def.mu.Lock()
	defer def.mu.Unlock()
	def.pluginRes[pluginResultKey{backendType, instance, endpoint, result}]++
	k := pluginKey{backendType, instance, endpoint}
	h := def.pluginDur[k]
	if h == nil {
		h = newHistogram(pluginBucketLe)
		def.pluginDur[k] = h
	}
	h.observe(d.Seconds())
}

// PluginCallStarted increments the per-backend in-flight gauge and returns the
// function that decrements it. Call as `defer metrics.PluginCallStarted(t)()`.
func PluginCallStarted(backendType string) func() {
	def.mu.Lock()
	def.pluginInFlight[backendType]++
	def.mu.Unlock()
	return func() {
		def.mu.Lock()
		def.pluginInFlight[backendType]--
		def.mu.Unlock()
	}
}

// ObservePlacement records one volume-placement ATTEMPT: exactly one call per
// CreateVolume RPC that got as far as dispatch, whatever happens afterwards.
//
// It counts attempts, not distinct volumes -- CSI CreateVolume is idempotent and
// the external-provisioner retries, so a single PVC can produce several
// successful attempts. Use it for rate and distribution, never as a volume census.
//
// decision is how dispatch chose the instance (dispatch.Decision*), so silent
// fallback concentration -- every volume landing on the default because topology
// never matched -- is visible instead of looking like healthy provisioning.
func ObservePlacement(backendType, instance, zone, decision, result string) {
	// An instance may legitimately serve no zone; label it explicitly rather
	// than emitting an empty string that reads as missing instrumentation.
	if zone == "" {
		zone = ZoneNone
	}
	if instance == "" {
		instance = InstanceNone
	}
	def.mu.Lock()
	defer def.mu.Unlock()
	def.placements[placementKey{backendType, instance, zone, decision, result}]++
}

// Serve runs the /metrics HTTP endpoint until ctx is cancelled. addr is a standard
// listen address such as ":9809".
func Serve(ctx context.Context, addr string) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", handle)
	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		// Without this a stalled scrape client holds the response open
		// indefinitely. Formatting no longer holds the collector lock, so this
		// is defence in depth rather than the fix, but an unbounded write is
		// still a free file descriptor + goroutine leak.
		WriteTimeout: 30 * time.Second,
	}
	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()
	return srv.ListenAndServe()
}

func handle(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	def.write(w)
}

// WriteTo renders the current metrics in Prometheus text exposition format --
// the same bytes /metrics serves. Useful for one-shot diagnostics on a driver
// running without the endpoint enabled, and for asserting on recorded metrics.
func WriteTo(w io.Writer) { def.write(w) }

// snapshot is a consistent copy of the collector taken under the lock, so
// formatting (which writes to a possibly slow network client) happens outside
// it. Holding the mutex across the write would let one stuck scrape block every
// observe() call -- and observe() runs on the gRPC serving path.
type snapshot struct {
	byMethod       map[string]*histogram
	byCode         map[codeKey]uint64
	pluginDur      map[pluginKey]*histogram
	pluginRes      map[pluginResultKey]uint64
	pluginInFlight map[string]int64
	placements     map[placementKey]uint64
	inFlight       int64
}

func (c *collector) snapshot() *snapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := &snapshot{
		byMethod:       make(map[string]*histogram, len(c.byMethod)),
		byCode:         make(map[codeKey]uint64, len(c.byCode)),
		pluginDur:      make(map[pluginKey]*histogram, len(c.pluginDur)),
		pluginRes:      make(map[pluginResultKey]uint64, len(c.pluginRes)),
		pluginInFlight: make(map[string]int64, len(c.pluginInFlight)),
		placements:     make(map[placementKey]uint64, len(c.placements)),
		inFlight:       c.inFlight.Load(),
	}
	for k, v := range c.byMethod {
		s.byMethod[k] = v.clone()
	}
	for k, v := range c.byCode {
		s.byCode[k] = v
	}
	for k, v := range c.pluginDur {
		s.pluginDur[k] = v.clone()
	}
	for k, v := range c.pluginRes {
		s.pluginRes[k] = v
	}
	for k, v := range c.pluginInFlight {
		s.pluginInFlight[k] = v
	}
	for k, v := range c.placements {
		s.placements[k] = v
	}
	return s
}

func (c *collector) write(w io.Writer) { c.snapshot().format(w) }

// escLabel escapes a label value per the Prometheus text exposition format,
// which defines exactly three escapes: backslash, double quote, and line feed.
func escLabel(s string) string {
	if !strings.ContainsAny(s, "\\\"\n") {
		return s
	}
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(s)
}

// hist writes one histogram's bucket/sum/count lines. labels is the pre-rendered
// label set without braces, e.g. `method="/csi.v1.Controller/CreateVolume"`.
func hist(w io.Writer, name, labels string, h *histogram) {
	sep := ","
	if labels == "" {
		sep = ""
	}
	for i, le := range h.le {
		fmt.Fprintf(w, "%s_bucket{%s%sle=%q} %d\n", name, labels, sep,
			strconv.FormatFloat(le, 'g', -1, 64), h.buckets[i])
	}
	fmt.Fprintf(w, "%s_bucket{%s%sle=\"+Inf\"} %d\n", name, labels, sep, h.count)
	fmt.Fprintf(w, "%s_sum{%s} %g\n", name, labels, h.sum)
	fmt.Fprintf(w, "%s_count{%s} %d\n", name, labels, h.count)
}

func (s *snapshot) format(w io.Writer) {
	s.formatGRPC(w)
	s.formatPlugin(w)
	s.formatPlacements(w)
}

func (s *snapshot) formatGRPC(w io.Writer) {
	fmt.Fprintln(w, "# HELP bard_csi_grpc_requests_total Total gRPC requests by method and status code.")
	fmt.Fprintln(w, "# TYPE bard_csi_grpc_requests_total counter")
	codeKeys := make([]codeKey, 0, len(s.byCode))
	for k := range s.byCode {
		codeKeys = append(codeKeys, k)
	}
	sort.Slice(codeKeys, func(i, j int) bool {
		if codeKeys[i].method != codeKeys[j].method {
			return codeKeys[i].method < codeKeys[j].method
		}
		return codeKeys[i].code < codeKeys[j].code
	})
	for _, k := range codeKeys {
		fmt.Fprintf(w, "bard_csi_grpc_requests_total{method=\"%s\",code=\"%s\"} %d\n",
			escLabel(k.method), escLabel(k.code), s.byCode[k])
	}

	fmt.Fprintln(w, "# HELP bard_csi_grpc_request_duration_seconds gRPC handler latency.")
	fmt.Fprintln(w, "# TYPE bard_csi_grpc_request_duration_seconds histogram")
	methods := make([]string, 0, len(s.byMethod))
	for m := range s.byMethod {
		methods = append(methods, m)
	}
	sort.Strings(methods)
	for _, m := range methods {
		hist(w, "bard_csi_grpc_request_duration_seconds",
			fmt.Sprintf(`method="%s"`, escLabel(m)), s.byMethod[m])
	}

	fmt.Fprintln(w, "# HELP bard_csi_grpc_requests_in_flight In-flight gRPC requests.")
	fmt.Fprintln(w, "# TYPE bard_csi_grpc_requests_in_flight gauge")
	fmt.Fprintf(w, "bard_csi_grpc_requests_in_flight %d\n", s.inFlight)
}

func (s *snapshot) formatPlugin(w io.Writer) {
	fmt.Fprintln(w, "# HELP bard_csi_plugin_requests_total Total core->plugin socket calls by backend, instance, endpoint and outcome.")
	fmt.Fprintln(w, "# TYPE bard_csi_plugin_requests_total counter")
	resKeys := make([]pluginResultKey, 0, len(s.pluginRes))
	for k := range s.pluginRes {
		resKeys = append(resKeys, k)
	}
	sort.Slice(resKeys, func(i, j int) bool {
		a, b := resKeys[i], resKeys[j]
		if a.backend != b.backend {
			return a.backend < b.backend
		}
		if a.instance != b.instance {
			return a.instance < b.instance
		}
		if a.endpoint != b.endpoint {
			return a.endpoint < b.endpoint
		}
		return a.result < b.result
	})
	for _, k := range resKeys {
		fmt.Fprintf(w, "bard_csi_plugin_requests_total{backend=\"%s\",instance=\"%s\",endpoint=\"%s\",result=\"%s\"} %d\n",
			escLabel(k.backend), escLabel(k.instance), escLabel(k.endpoint), escLabel(k.result), s.pluginRes[k])
	}

	fmt.Fprintln(w, "# HELP bard_csi_plugin_request_duration_seconds Core->plugin socket round-trip latency (not the backend command inside the plugin).")
	fmt.Fprintln(w, "# TYPE bard_csi_plugin_request_duration_seconds histogram")
	durKeys := make([]pluginKey, 0, len(s.pluginDur))
	for k := range s.pluginDur {
		durKeys = append(durKeys, k)
	}
	sort.Slice(durKeys, func(i, j int) bool {
		a, b := durKeys[i], durKeys[j]
		if a.backend != b.backend {
			return a.backend < b.backend
		}
		if a.instance != b.instance {
			return a.instance < b.instance
		}
		return a.endpoint < b.endpoint
	})
	for _, k := range durKeys {
		hist(w, "bard_csi_plugin_request_duration_seconds",
			fmt.Sprintf(`backend="%s",instance="%s",endpoint="%s"`,
				escLabel(k.backend), escLabel(k.instance), escLabel(k.endpoint)), s.pluginDur[k])
	}

	fmt.Fprintln(w, "# HELP bard_csi_plugin_requests_in_flight In-flight core->plugin socket calls by backend.")
	fmt.Fprintln(w, "# TYPE bard_csi_plugin_requests_in_flight gauge")
	backends := make([]string, 0, len(s.pluginInFlight))
	for b := range s.pluginInFlight {
		backends = append(backends, b)
	}
	sort.Strings(backends)
	for _, b := range backends {
		fmt.Fprintf(w, "bard_csi_plugin_requests_in_flight{backend=\"%s\"} %d\n", escLabel(b), s.pluginInFlight[b])
	}
}

func (s *snapshot) formatPlacements(w io.Writer) {
	fmt.Fprintln(w, "# HELP bard_csi_volume_placement_attempts_total CreateVolume attempts by backend, instance, zone and how dispatch chose it. Counts attempts (CSI creates are idempotent and retried), not distinct volumes.")
	fmt.Fprintln(w, "# TYPE bard_csi_volume_placement_attempts_total counter")
	keys := make([]placementKey, 0, len(s.placements))
	for k := range s.placements {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := keys[i], keys[j]
		if a.backend != b.backend {
			return a.backend < b.backend
		}
		if a.instance != b.instance {
			return a.instance < b.instance
		}
		if a.zone != b.zone {
			return a.zone < b.zone
		}
		if a.decision != b.decision {
			return a.decision < b.decision
		}
		return a.result < b.result
	})
	for _, k := range keys {
		fmt.Fprintf(w, "bard_csi_volume_placement_attempts_total{backend=\"%s\",instance=\"%s\",zone=\"%s\",decision=\"%s\",result=\"%s\"} %d\n",
			escLabel(k.backend), escLabel(k.instance), escLabel(k.zone),
			escLabel(k.decision), escLabel(k.result), s.placements[k])
	}
}
