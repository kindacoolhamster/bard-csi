package cephplugin

import (
	"os"
	"path/filepath"
	"testing"
)

func TestKeyFor(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "east"), []byte("FILEKEY\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	withDir := &Backend{keyDir: dir}
	noDir := &Backend{keyDir: ""}

	cases := []struct {
		name     string
		be       *Backend
		instance string
		secrets  map[string]string
		want     string
		wantErr  bool
	}{
		{"file wins over secret", withDir, "east", map[string]string{"userKey": "SECRET"}, "FILEKEY", false},
		{"secret fallback when file missing", withDir, "west", map[string]string{"userKey": "SECRET"}, "SECRET", false},
		{"keyDir set, no file, no secret => error", withDir, "west", nil, "", true},
		{"no keyDir, no secret => empty (tests)", noDir, "x", nil, "", false},
		{"no keyDir, secret => secret", noDir, "x", map[string]string{"userKey": "S"}, "S", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.be.keyFor(tc.instance, tc.secrets)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err=%v wantErr=%v", err, tc.wantErr)
			}
			if got != tc.want {
				t.Fatalf("got %q want %q", got, tc.want)
			}
		})
	}
}
