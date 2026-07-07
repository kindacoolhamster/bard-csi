package cephenc

import "testing"

func TestIsFsCrypt(t *testing.T) {
	cases := []struct {
		name string
		ctx  map[string]string
		want bool
	}{
		{"unencrypted", map[string]string{}, false},
		{"block default", map[string]string{ParamEncrypted: "true"}, false},
		{"block explicit", map[string]string{ParamEncrypted: "true", ParamEncryptionType: "block"}, false},
		{"file", map[string]string{ParamEncrypted: "true", ParamEncryptionType: "file"}, true},
		{"file but not encrypted", map[string]string{ParamEncryptionType: "file"}, false},
	}
	for _, c := range cases {
		if got := IsFsCrypt(c.ctx); got != c.want {
			t.Errorf("%s: IsFsCrypt=%v want %v", c.name, got, c.want)
		}
	}
}

func TestDeriveFscryptKey(t *testing.T) {
	a, err := deriveFscryptKey("passphrase-A")
	if err != nil {
		t.Fatal(err)
	}
	if len(a) != 64 {
		t.Fatalf("fscrypt master key must be 64 bytes, got %d", len(a))
	}
	again, _ := deriveFscryptKey("passphrase-A")
	if string(a) != string(again) {
		t.Fatal("derivation must be deterministic (same passphrase -> same key across restages)")
	}
	b, _ := deriveFscryptKey("passphrase-B")
	if string(a) == string(b) {
		t.Fatal("different passphrases must derive different keys")
	}
}
