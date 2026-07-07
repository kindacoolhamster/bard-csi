package cephenc

import (
	"os"
	"path/filepath"
	"testing"
)

// Credentials resolve from a mounted shared-credentials file, taking precedence over
// inline/env, and parse the standard ini keys.
func TestAWSCredentialsFile(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "creds")
	os.WriteFile(f, []byte("[default]\naws_access_key_id = AKIAEXAMPLE\naws_secret_access_key = secret/+key\naws_session_token = tok123\n"), 0o600)
	c, err := awsCredsSource{file: f}.resolve()
	if err != nil {
		t.Fatal(err)
	}
	if c.accessKeyID != "AKIAEXAMPLE" || c.secretAccessKey != "secret/+key" || c.sessionToken != "tok123" {
		t.Fatalf("parsed creds wrong: %+v", c)
	}
	// A file missing the secret key is an error, not silent empty creds.
	bad := filepath.Join(dir, "bad")
	os.WriteFile(bad, []byte("[default]\naws_access_key_id = X\n"), 0o600)
	if _, err := (awsCredsSource{file: bad}).resolve(); err == nil {
		t.Fatal("a credentials file without a secret key must error")
	}
}
