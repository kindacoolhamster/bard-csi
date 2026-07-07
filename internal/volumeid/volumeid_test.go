package volumeid

import "testing"

func TestRoundTrip(t *testing.T) {
	in := Handle{Backend: "ceph-rbd", Instance: "east", Location: "replicapool", Name: "pvc-9f3a"}
	out, err := Parse(in.String())
	if err != nil {
		t.Fatalf("Parse(%q): %v", in.String(), err)
	}
	if out != in {
		t.Fatalf("round trip mismatch: got %+v want %+v", out, in)
	}
}

func TestParseRejectsGarbage(t *testing.T) {
	for _, s := range []string{"", "not-a-handle", "swsk|9|a|b|c|d", "swsk|1|a|b|c"} {
		if _, err := Parse(s); err == nil {
			t.Errorf("Parse(%q) = nil err, want error", s)
		}
	}
}

func TestValidateLength(t *testing.T) {
	long := Handle{Backend: "ceph-rbd", Instance: "east", Location: "pool"}
	for len(long.Name) <= MaxLength {
		long.Name += "x"
	}
	if err := long.Validate(); err == nil {
		t.Fatal("expected length validation error")
	}
}

func TestValidateRequiresFields(t *testing.T) {
	if err := (Handle{Backend: "ceph-rbd", Name: "x"}).Validate(); err == nil {
		t.Fatal("expected error when instance is empty")
	}
}
