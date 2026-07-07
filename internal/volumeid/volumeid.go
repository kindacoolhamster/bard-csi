// Package volumeid encodes and decodes CSI volume handles.
//
// The CSI volume_id is the only piece of state the driver is guaranteed to
// receive back on DeleteVolume, ControllerPublish, NodeStage, etc. It is
// therefore self-describing: it carries enough to route an operation to the
// correct backend type and backend instance (zone/cluster) without consulting
// any external store. The CSI spec caps volume_id at 128 bytes, so the
// encoding is deliberately compact; anything that does not fit (extra
// parameters, secrets) travels in the VolumeContext instead.
package volumeid

import (
	"errors"
	"fmt"
	"strings"
)

const (
	magic = "swsk"
	ver   = "1"
	sep   = "|"

	// MaxLength is the CSI spec hard limit on volume_id, in bytes.
	MaxLength = 128
)

// Handle is the decoded form of a CSI volume_id.
//
//	swsk|1|ceph-rbd|east|replicapool|csi-vol-9f3a...
//	 |   |    |       |       |          |
//	 |   |    |       |       |          +-- Name:     backend-native object name
//	 |   |    |       |       +------------- Location: backend-defined locator (rbd pool)
//	 |   |    |       +--------------------- Instance: backend instance / zone id
//	 |   |    +----------------------------- Backend:  backend type
//	 |   +---------------------------------- encoding version
//	 +-------------------------------------- magic
type Handle struct {
	Backend  string // backend type, e.g. "ceph-rbd"
	Instance string // backend instance / zone id, e.g. "east"
	Location string // backend-defined locator, e.g. rbd pool name
	Name     string // backend-native object name, e.g. rbd image name
}

var errBadHandle = errors.New("volumeid: malformed volume handle")

// Validate reports whether the handle has all required fields and encodes
// within the CSI length limit.
func (h Handle) Validate() error {
	if h.Backend == "" || h.Instance == "" || h.Name == "" {
		return fmt.Errorf("%w: backend, instance and name are required", errBadHandle)
	}
	for _, f := range []string{h.Backend, h.Instance, h.Location, h.Name} {
		if strings.Contains(f, sep) {
			return fmt.Errorf("%w: field %q contains reserved separator %q", errBadHandle, f, sep)
		}
	}
	if n := len(h.String()); n > MaxLength {
		return fmt.Errorf("%w: encoded length %d exceeds CSI limit of %d bytes", errBadHandle, n, MaxLength)
	}
	return nil
}

// String renders the handle as a CSI volume_id.
func (h Handle) String() string {
	return strings.Join([]string{magic, ver, h.Backend, h.Instance, h.Location, h.Name}, sep)
}

// Parse decodes a CSI volume_id produced by String.
func Parse(s string) (Handle, error) {
	parts := strings.Split(s, sep)
	if len(parts) != 6 || parts[0] != magic || parts[1] != ver {
		return Handle{}, fmt.Errorf("%w: %q", errBadHandle, s)
	}
	h := Handle{
		Backend:  parts[2],
		Instance: parts[3],
		Location: parts[4],
		Name:     parts[5],
	}
	if err := h.Validate(); err != nil {
		return Handle{}, err
	}
	return h, nil
}
