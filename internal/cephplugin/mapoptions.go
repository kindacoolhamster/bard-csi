package cephplugin

import "strings"

// resolveMounterOptions selects the map/unmap options that apply to the given
// mounter from a possibly mounter-scoped value. Segments are separated by ';';
// a segment may be prefixed "krbd:" or "nbd:" to apply to only that mounter, and
// an unprefixed segment applies to whichever mounter is in use. The selected
// segments are rejoined with ',' (the form `rbd map --options` expects).
//
//	resolveMounterOptions("krbd:notrim;nbd:try-netlink", "krbd") == "notrim"
//	resolveMounterOptions("ms_mode=secure;krbd:notrim", "krbd")  == "ms_mode=secure,notrim"
//	resolveMounterOptions("nbd:try-netlink", "krbd")             == ""
func resolveMounterOptions(raw, mounter string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	// The default (empty) mounter is krbd; only rbd-nbd is non-empty.
	want := "krbd"
	if mounter == mounterNBD {
		want = "nbd"
	}
	var out []string
	for _, seg := range strings.Split(raw, ";") {
		seg = strings.TrimSpace(seg)
		if seg == "" {
			continue
		}
		switch {
		case strings.HasPrefix(seg, "krbd:"):
			if want == "krbd" {
				out = append(out, strings.TrimSpace(seg[len("krbd:"):]))
			}
		case strings.HasPrefix(seg, "nbd:"):
			if want == "nbd" {
				out = append(out, strings.TrimSpace(seg[len("nbd:"):]))
			}
		default:
			out = append(out, seg)
		}
	}
	return strings.Join(out, ",")
}

// readAffinityOptions returns the krbd map options that place reads near the
// node, or "" when read-affinity is off, the node's CRUSH location is unknown,
// or the mounter is rbd-nbd (read_from_replica is a kernel-rbd feature).
func readAffinityOptions(cc ClusterConfig, crushLocation string) string {
	if !cc.ReadAffinity || crushLocation == "" || cc.Mounter == mounterNBD {
		return ""
	}
	return "read_from_replica=localize,crush_location=" + crushLocation
}

// combineOptions joins the non-empty option strings with ',' (the comma-separated
// form `rbd map --options` expects).
func combineOptions(opts ...string) string {
	var nonEmpty []string
	for _, o := range opts {
		if o != "" {
			nonEmpty = append(nonEmpty, o)
		}
	}
	return strings.Join(nonEmpty, ",")
}
