// Package metrics exposes Prometheus-format gRPC operation metrics for the Bard
// CSI driver over a stdlib HTTP server -- deliberately NO prometheus/client_golang
// dependency, the same lean-image discipline as the rest of the driver (hand-rolled
// SigV4, KMIP as the only sanctioned dep). It records, via a gRPC unary
// interceptor, per-method request counts (labelled by gRPC status code), handler
// latency as a histogram, and in-flight requests, and serves them in the Prometheus
// text exposition format at /metrics.
package metrics

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/status"
)

// bucketLe are the histogram upper bounds (seconds), cumulative per Prometheus.
var bucketLe = []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10}

type methodStat struct {
	buckets []uint64 // len(bucketLe); buckets[i] = #observations <= bucketLe[i]
	sum     float64  // total seconds
	count   uint64
}

type collector struct {
	mu       sync.Mutex
	byMethod map[string]*methodStat
	byCode   map[codeKey]uint64
	inFlight atomic.Int64
}

type codeKey struct{ method, code string }

var def = &collector{byMethod: map[string]*methodStat{}, byCode: map[codeKey]uint64{}}

func (c *collector) observe(method, code string, d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.byCode[codeKey{method, code}]++
	ms := c.byMethod[method]
	if ms == nil {
		ms = &methodStat{buckets: make([]uint64, len(bucketLe))}
		c.byMethod[method] = ms
	}
	s := d.Seconds()
	ms.sum += s
	ms.count++
	for i, le := range bucketLe {
		if s <= le {
			ms.buckets[i]++
		}
	}
}

// Interceptor is a grpc.UnaryServerInterceptor that records call count, latency and
// in-flight gauge for every RPC. Recording is in-memory and cheap, so it is always
// chained; the data is only exposed when Serve is started.
func Interceptor(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	def.inFlight.Add(1)
	start := time.Now()
	resp, err := handler(ctx, req)
	def.inFlight.Add(-1)
	def.observe(info.FullMethod, status.Code(err).String(), time.Since(start))
	return resp, err
}

// Serve runs the /metrics HTTP endpoint until ctx is cancelled. addr is a standard
// listen address such as ":9809".
func Serve(ctx context.Context, addr string) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", handle)
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
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

func (c *collector) write(w io.Writer) {
	c.mu.Lock()
	defer c.mu.Unlock()

	fmt.Fprintln(w, "# HELP bard_csi_grpc_requests_total Total gRPC requests by method and status code.")
	fmt.Fprintln(w, "# TYPE bard_csi_grpc_requests_total counter")
	codeKeys := make([]codeKey, 0, len(c.byCode))
	for k := range c.byCode {
		codeKeys = append(codeKeys, k)
	}
	sort.Slice(codeKeys, func(i, j int) bool {
		if codeKeys[i].method != codeKeys[j].method {
			return codeKeys[i].method < codeKeys[j].method
		}
		return codeKeys[i].code < codeKeys[j].code
	})
	for _, k := range codeKeys {
		fmt.Fprintf(w, "bard_csi_grpc_requests_total{method=%q,code=%q} %d\n", k.method, k.code, c.byCode[k])
	}

	fmt.Fprintln(w, "# HELP bard_csi_grpc_request_duration_seconds gRPC handler latency.")
	fmt.Fprintln(w, "# TYPE bard_csi_grpc_request_duration_seconds histogram")
	methods := make([]string, 0, len(c.byMethod))
	for m := range c.byMethod {
		methods = append(methods, m)
	}
	sort.Strings(methods)
	for _, m := range methods {
		ms := c.byMethod[m]
		for i, le := range bucketLe {
			fmt.Fprintf(w, "bard_csi_grpc_request_duration_seconds_bucket{method=%q,le=%q} %d\n",
				m, strconv.FormatFloat(le, 'g', -1, 64), ms.buckets[i])
		}
		fmt.Fprintf(w, "bard_csi_grpc_request_duration_seconds_bucket{method=%q,le=\"+Inf\"} %d\n", m, ms.count)
		fmt.Fprintf(w, "bard_csi_grpc_request_duration_seconds_sum{method=%q} %g\n", m, ms.sum)
		fmt.Fprintf(w, "bard_csi_grpc_request_duration_seconds_count{method=%q} %d\n", m, ms.count)
	}

	fmt.Fprintln(w, "# HELP bard_csi_grpc_requests_in_flight In-flight gRPC requests.")
	fmt.Fprintln(w, "# TYPE bard_csi_grpc_requests_in_flight gauge")
	fmt.Fprintf(w, "bard_csi_grpc_requests_in_flight %d\n", c.inFlight.Load())
}
