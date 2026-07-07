package cephplugin

import (
	"context"
	"fmt"
	"net"
	"os"
	"strings"

	"github.com/kindacoolhamster/bard-csi/internal/cephenc"
	"github.com/kindacoolhamster/bard-csi/pkg/bardplugin"
)

// NetworkFence (csi-addons) fences a failed/partitioned node's client network
// ranges at the Ceph layer with `osd blocklist range add`, so the node can no
// longer reach the cluster before its exclusive volumes are failed over -- the
// node-scoped complement to the per-volume single-writer fence (fenceStaleWatchers).
// A DR orchestrator (Ramen) drives it via the NetworkFence CR.
//
// Credentials come from the request Secrets (a user with `mon 'profile rbd, allow
// command "osd blocklist"'`), NOT the per-volume provisioning user: the scoped
// `profile rbd` user can `blocklist range add` but NOT `range rm` (verified
// against Ceph), so Unfence would fail with it. cephFenceConn therefore prefers
// the request secrets over the instance's mounted provisioning key.

func (b *Backend) FenceClusterNetwork(ctx context.Context, req *bardplugin.FenceClusterNetworkRequest) error {
	return b.blocklistRange(ctx, "add", req.Instance, req.CIDRs, req.Secrets)
}

func (b *Backend) UnfenceClusterNetwork(ctx context.Context, req *bardplugin.UnfenceClusterNetworkRequest) error {
	return b.blocklistRange(ctx, "rm", req.Instance, req.CIDRs, req.Secrets)
}

func (b *Backend) blocklistRange(ctx context.Context, op, instance string, cidrs []string, secrets map[string]string) error {
	if len(cidrs) == 0 {
		return nil
	}
	cc, err := b.cluster(instance)
	if err != nil {
		return err
	}
	conn, cleanup, err := b.cephFenceConn(cc, instance, secrets)
	if err != nil {
		return err
	}
	defer cleanup()
	for _, cidr := range cidrs {
		cidr = strings.TrimSpace(cidr)
		if cidr == "" {
			continue
		}
		// add/rm of a range are both idempotent in Ceph (a repeated add refreshes
		// the expiry; rm of an absent range succeeds), so retries are safe.
		if _, err := b.run.Run(ctx, "ceph", appendArgs(conn, "osd", "blocklist", "range", op, cidr)...); err != nil {
			return fmt.Errorf("ceph-rbd: osd blocklist range %s %s: %w", op, cidr, err)
		}
	}
	return nil
}

// ListClusterFence returns the CIDR ranges currently blocklisted on the instance's
// cluster. `osd blocklist ls` lists every entry; range entries are rendered as
// "cidr:<addr>:<nonce>/<prefix>" (e.g. "cidr:192.0.2.0:0/24"), which we normalise
// back to a plain CIDR ("192.0.2.0/24"). Single-IP watcher fences are not ranges
// and are skipped.
func (b *Backend) ListClusterFence(ctx context.Context, req *bardplugin.ListClusterFenceRequest) (*bardplugin.ListClusterFenceResponse, error) {
	cc, err := b.cluster(req.Instance)
	if err != nil {
		return nil, err
	}
	conn, cleanup, err := b.cephFenceConn(cc, req.Instance, req.Secrets)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	out, err := b.run.Run(ctx, "ceph", appendArgs(conn, "osd", "blocklist", "ls")...)
	if err != nil {
		return nil, fmt.Errorf("ceph-rbd: osd blocklist ls: %w", err)
	}
	var cidrs []string
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || !strings.HasPrefix(fields[0], "cidr:") {
			continue
		}
		if c := normalizeCIDR(fields[0]); c != "" {
			cidrs = append(cidrs, c)
		}
	}
	return &bardplugin.ListClusterFenceResponse{CIDRs: cidrs}, nil
}

// GetFenceClients returns the client a NetworkFence should target for the instance's
// Ceph cluster (the csi-addons GET_CLIENTS_TO_FENCE capability): the cluster FSID as
// the client id (matching ceph-csi) and this controller's local address toward the
// mon as a host CIDR. A DR orchestrator (Ramen) collects these and passes them to
// Fence/UnfenceClusterNetwork. A pure read -- it needs only `ceph fsid` (mon read,
// which the provisioning user has), so it works without the stronger fence user.
func (b *Backend) GetFenceClients(ctx context.Context, req *bardplugin.GetFenceClientsRequest) (*bardplugin.GetFenceClientsResponse, error) {
	cc, err := b.cluster(req.Instance)
	if err != nil {
		return nil, err
	}
	conn, cleanup, err := b.cephFenceConn(cc, req.Instance, req.Secrets)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	out, err := b.run.Run(ctx, "ceph", appendArgs(conn, "fsid")...)
	if err != nil {
		return nil, fmt.Errorf("ceph-rbd: ceph fsid: %w", err)
	}
	fsid := strings.TrimSpace(out)
	cidr, err := clientAddrCIDR(cc.Monitors)
	if err != nil {
		return nil, err
	}
	return &bardplugin.GetFenceClientsResponse{
		Clients: []bardplugin.FenceClient{{ID: fsid, CIDRs: []string{cidr}}},
	}, nil
}

// clientAddrCIDR returns this host's local address used to reach the first monitor,
// as a host CIDR (/32 IPv4, /128 IPv6). A UDP "connect" sends no packet but makes the
// kernel pick the egress source address -- the address Ceph sees for this client, the
// CLI analogue of librados' GetAddrs that ceph-csi returns.
func clientAddrCIDR(monitors []string) (string, error) {
	if len(monitors) == 0 {
		return "", fmt.Errorf("ceph-rbd: no monitors configured to derive client address")
	}
	host, port, err := net.SplitHostPort(monitors[0])
	if err != nil {
		host, port = monitors[0], "3300"
	}
	c, err := net.Dial("udp", net.JoinHostPort(host, port))
	if err != nil {
		return "", fmt.Errorf("ceph-rbd: derive client address toward mon %q: %w", monitors[0], err)
	}
	defer c.Close()
	ip := c.LocalAddr().(*net.UDPAddr).IP
	if ip4 := ip.To4(); ip4 != nil {
		return ip4.String() + "/32", nil
	}
	return ip.String() + "/128", nil
}

// normalizeCIDR turns a Ceph blocklist range entry "cidr:192.0.2.0:0/24" into the
// plain CIDR "192.0.2.0/24" (strips the "cidr:" tag and the ":<nonce>" before the
// prefix length). Returns "" if the shape is unexpected.
func normalizeCIDR(entry string) string {
	entry = strings.TrimPrefix(entry, "cidr:")
	slash := strings.LastIndex(entry, "/")
	if slash < 0 {
		return ""
	}
	addr, prefix := entry[:slash], entry[slash+1:]
	if colon := strings.LastIndex(addr, ":"); colon >= 0 {
		addr = addr[:colon] // drop the ":<nonce>" Ceph appends
	}
	if addr == "" || prefix == "" {
		return ""
	}
	return addr + "/" + prefix
}

// cephFenceConn builds ceph-CLI connection args for a NetworkFence op, preferring
// the request Secrets' userID/userKey (a blocklist-capable user) over the
// instance's mounted provisioning key -- unlike connArgs/cephCLIConn, which prefer
// the mounted key. See the package comment for why.
func (b *Backend) cephFenceConn(cc ClusterConfig, instance string, secrets map[string]string) ([]string, func(), error) {
	conf, err := os.CreateTemp("", "csi-fence-conf-")
	if err != nil {
		return nil, func() {}, fmt.Errorf("ceph-rbd: fence conf: %w", err)
	}
	fmt.Fprintf(conf, "[global]\nmon_host = %s\n", strings.Join(cc.Monitors, ","))
	conf.Close()
	files := []string{conf.Name()}
	cleanup := func() {
		for _, f := range files {
			os.Remove(f)
		}
	}

	user := secrets[secretUserID]
	if user == "" {
		user = cc.UserID
	}
	if user == "" {
		user = defaultUserID
	}
	args := []string{"--conf", conf.Name(), "--id", user}

	key := secrets[secretUserKey]
	if key == "" {
		// Fall back to the instance's mounted key (it can fence but typically not
		// unfence -- a deliberately weaker default; production supplies a fence user).
		k, err := b.keyFor(instance, nil)
		if err != nil {
			cleanup()
			return nil, func() {}, err
		}
		key = k
	}
	if key != "" {
		kf, err := cephenc.SecretTemp("csi-fence-key-")
		if err != nil {
			cleanup()
			return nil, func() {}, fmt.Errorf("ceph-rbd: fence keyfile: %w", err)
		}
		kf.WriteString(key)
		kf.Close()
		files = append(files, kf.Name())
		args = append(args, "--keyfile", kf.Name())
	}
	return args, cleanup, nil
}
