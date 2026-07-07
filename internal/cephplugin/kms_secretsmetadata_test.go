package cephplugin

import (
	"context"
	"errors"
)

// metaRunner models `rbd image-meta get/set` over an in-memory map, so a KMS provider
// (secrets-metadata, aws-kms, kmip) round-trips its metadata like the real image
// metadata. Shared by the KMS and encryption-clone tests. The KMS providers themselves
// are unit-tested in internal/cephenc against a fakeHost; here they are exercised
// through the real Backend (which implements cephenc.Host).
type metaRunner struct {
	meta map[string]string
}

func (r *metaRunner) Run(_ context.Context, name string, args ...string) (string, error) {
	if name == "rbd" && has(args, "image-meta") {
		i := indexOf(args, "image-meta")
		op, spec, key := args[i+1], args[i+2], args[i+3]
		mk := spec + "|" + key
		switch op {
		case "set":
			r.meta[mk] = args[i+4]
			return "", nil
		case "get":
			if v, ok := r.meta[mk]; ok {
				return v + "\n", nil
			}
			return "", errors.New("rbd: image-meta get: (2) No such file or directory")
		}
	}
	return "", nil
}

func indexOf(ss []string, want string) int {
	for i, s := range ss {
		if s == want {
			return i
		}
	}
	return -1
}

// orDefault returns v, or def when v is empty (a small test helper; the production
// copy lives in internal/cephenc).
func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
