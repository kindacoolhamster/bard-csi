package metrics

import (
	"context"
	"errors"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// The interceptor records a request (by method + code) and its latency, and the
// exposition renders valid Prometheus text for the counter, histogram and gauge.
func TestInterceptorAndExposition(t *testing.T) {
	const method = "/csi.v1.Controller/CreateVolume"

	// One OK call.
	if _, err := Interceptor(context.Background(), nil,
		&grpc.UnaryServerInfo{FullMethod: method},
		func(context.Context, any) (any, error) { return nil, nil },
	); err != nil {
		t.Fatalf("ok call: %v", err)
	}
	// One failing call (NotFound).
	_, _ = Interceptor(context.Background(), nil,
		&grpc.UnaryServerInfo{FullMethod: method},
		func(context.Context, any) (any, error) { return nil, status.Error(codes.NotFound, "x") },
	)

	var sb strings.Builder
	def.write(&sb)
	out := sb.String()

	for _, want := range []string{
		`bard_csi_grpc_requests_total{method="/csi.v1.Controller/CreateVolume",code="OK"} 1`,
		`bard_csi_grpc_requests_total{method="/csi.v1.Controller/CreateVolume",code="NotFound"} 1`,
		`bard_csi_grpc_request_duration_seconds_count{method="/csi.v1.Controller/CreateVolume"} 2`,
		`bard_csi_grpc_request_duration_seconds_bucket{method="/csi.v1.Controller/CreateVolume",le="+Inf"} 2`,
		"# TYPE bard_csi_grpc_request_duration_seconds histogram",
		"bard_csi_grpc_requests_in_flight 0",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("exposition missing %q\n---\n%s", want, out)
		}
	}
}

// A panicking/erroring handler still records the in-flight decrement and a code.
func TestInterceptorPropagatesError(t *testing.T) {
	want := errors.New("boom")
	_, got := Interceptor(context.Background(), nil,
		&grpc.UnaryServerInfo{FullMethod: "/m/E"},
		func(context.Context, any) (any, error) { return nil, want },
	)
	if got != want {
		t.Fatalf("interceptor must propagate the handler error, got %v", got)
	}
	if n := def.inFlight.Load(); n != 0 {
		t.Fatalf("in-flight must return to 0, got %d", n)
	}
}
