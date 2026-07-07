package cephplugin

import (
	"strings"
	"testing"
)

func TestCreateParams(t *testing.T) {
	cases := []struct {
		name   string
		params map[string]string
		want   string
	}{
		{"none", nil, ""},
		{"features", map[string]string{paramImageFeatures: "layering,exclusive-lock"}, "--image-feature layering,exclusive-lock"},
		{"stripe", map[string]string{paramStripeUnit: "65536", paramStripeCount: "16"}, "--stripe-unit 65536 --stripe-count 16"},
		{"objectSize", map[string]string{paramObjectSize: "4194304"}, "--object-size 4194304"},
		{"dataPool", map[string]string{paramDataPool: "ec-data"}, "--data-pool ec-data"},
		{
			"all",
			map[string]string{paramImageFeatures: "layering", paramStripeUnit: "4096", paramStripeCount: "8", paramObjectSize: "8388608", paramDataPool: "ec-data", "pool": "ignored"},
			"--image-feature layering --stripe-unit 4096 --stripe-count 8 --object-size 8388608 --data-pool ec-data",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := strings.Join(createParams(tc.params), " "); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}
