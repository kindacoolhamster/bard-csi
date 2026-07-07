package incluster

import "testing"

func TestNodeZoneFromJSON(t *testing.T) {
	body := []byte(`{
		"metadata": {
			"name": "bard-worker",
			"labels": {
				"kubernetes.io/hostname": "bard-worker",
				"topology.kubernetes.io/zone": "galileo"
			}
		}
	}`)
	z, err := nodeZoneFromJSON(body, "topology.kubernetes.io/zone")
	if err != nil {
		t.Fatal(err)
	}
	if z != "galileo" {
		t.Fatalf("want galileo, got %q", z)
	}
}

func TestNodeZoneFromJSONAbsent(t *testing.T) {
	body := []byte(`{"metadata":{"labels":{"kubernetes.io/hostname":"n1"}}}`)
	z, err := nodeZoneFromJSON(body, "topology.kubernetes.io/zone")
	if err != nil {
		t.Fatal(err)
	}
	if z != "" {
		t.Fatalf("absent label should yield empty, got %q", z)
	}
}

func TestNodeZoneFromJSONNoLabels(t *testing.T) {
	body := []byte(`{"metadata":{"name":"n1"}}`)
	z, err := nodeZoneFromJSON(body, "topology.kubernetes.io/zone")
	if err != nil {
		t.Fatal(err)
	}
	if z != "" {
		t.Fatalf("no labels should yield empty, got %q", z)
	}
}

func TestNodeZoneFromJSONBad(t *testing.T) {
	if _, err := nodeZoneFromJSON([]byte("not json"), "x"); err == nil {
		t.Fatal("expected decode error")
	}
}

func TestCrushLocationFromJSON(t *testing.T) {
	body := []byte(`{"metadata":{"labels":{
		"topology.kubernetes.io/region":"r1",
		"topology.kubernetes.io/zone":"z1",
		"topology.rook.io/rack":"rack3",
		"kubernetes.io/hostname":"n1"
	}}}`)
	// Bucket type = the label key's last path segment; absent labels are skipped;
	// order follows the requested keys.
	loc, err := crushLocationFromJSON(body, []string{
		"topology.kubernetes.io/region",
		"topology.kubernetes.io/zone",
		"topology.rook.io/rack",
		"topology.kubernetes.io/notset",
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := "region:r1|zone:z1|rack:rack3"; loc != want {
		t.Fatalf("got %q, want %q", loc, want)
	}
}

func TestCrushLocationFromJSONNonePresent(t *testing.T) {
	body := []byte(`{"metadata":{"labels":{"kubernetes.io/hostname":"n1"}}}`)
	loc, err := crushLocationFromJSON(body, []string{"topology.kubernetes.io/zone"})
	if err != nil {
		t.Fatal(err)
	}
	if loc != "" {
		t.Fatalf("no matching labels should yield empty, got %q", loc)
	}
}

func TestCrushBucketType(t *testing.T) {
	for in, want := range map[string]string{
		"topology.kubernetes.io/zone": "zone",
		"topology.rook.io/rack":       "rack",
		"rack":                        "rack", // no path separator
	} {
		if got := crushBucketType(in); got != want {
			t.Errorf("crushBucketType(%q) = %q, want %q", in, got, want)
		}
	}
}
