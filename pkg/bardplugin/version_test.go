package bardplugin

import "testing"

func TestParseContractVersion(t *testing.T) {
	cases := []struct {
		in           string
		major, minor int
		wantErr      bool
	}{
		{"", 1, 0, false}, // pre-versioning plugins parse as 1.0
		{"1.0", 1, 0, false},
		{"1.7", 1, 7, false},
		{"2.0", 2, 0, false},
		{"1", 0, 0, true},
		{"v1.0", 0, 0, true},
		{"1.0.1", 0, 0, true},
		{"-1.0", 0, 0, true},
		{"1.-2", 0, 0, true},
		{"1. 0", 0, 0, true},
		{"one.zero", 0, 0, true},
	}
	for _, c := range cases {
		major, minor, err := ParseContractVersion(c.in)
		if c.wantErr != (err != nil) {
			t.Errorf("ParseContractVersion(%q): err = %v, wantErr = %v", c.in, err, c.wantErr)
			continue
		}
		if !c.wantErr && (major != c.major || minor != c.minor) {
			t.Errorf("ParseContractVersion(%q) = %d.%d, want %d.%d", c.in, major, minor, c.major, c.minor)
		}
	}
}

// TestCurrentVersionParses pins the package's own constants together.
func TestCurrentVersionParses(t *testing.T) {
	major, minor, err := ParseContractVersion(ContractVersion)
	if err != nil {
		t.Fatalf("ContractVersion %q does not parse: %v", ContractVersion, err)
	}
	if major != ContractMajor {
		t.Fatalf("ContractVersion %q major = %d, but ContractMajor = %d", ContractVersion, major, ContractMajor)
	}
	// The version the SDK stamps on /info must be exactly the version core can
	// interpret. If they drift, every SDK-built plugin advertises a minor its
	// own core refuses (see ContractMinor's asymmetric gate) and nothing dials.
	if minor != ContractMinor {
		t.Fatalf("ContractVersion %q minor = %d, but ContractMinor = %d", ContractVersion, minor, ContractMinor)
	}
}
