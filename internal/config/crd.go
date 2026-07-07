// This file sources the same Config from BackendCluster custom resources instead
// of a file. Bard lists BackendClusters from the API once at startup (no watch)
// and folds them into the in-memory Config that wire() already consumes, so the
// rest of the driver is unchanged.
//
// The API call is a single in-cluster REST GET (see internal/incluster) --
// deliberately no client-go dependency, to keep core's dependency surface (and
// its distroless binary) minimal. A watch-based reload would justify client-go;
// a one-shot startup list does not.
package config

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"time"

	"k8s.io/klog/v2"

	"github.com/kindacoolhamster/bard-csi/internal/incluster"
)

// backendClustersPath is the cluster-scoped list endpoint for the CRD.
const backendClustersPath = "/apis/bard.io/v1alpha1/backendclusters"

// BackendCluster is the decoded form of one BackendCluster custom resource. Only
// the fields core needs are modelled.
type BackendCluster struct {
	Metadata struct {
		Name string `json:"name"`
	} `json:"metadata"`
	Spec BackendClusterSpec `json:"spec"`
}

// BackendClusterSpec mirrors the CRD's spec.
type BackendClusterSpec struct {
	BackendType string     `json:"backendType"`
	Instance    string     `json:"instance,omitempty"` // defaults to metadata.name
	Zone        string     `json:"zone"`
	Default     bool       `json:"default,omitempty"`
	Plugin      PluginSpec `json:"plugin"`
}

// PluginSpec is the plugin endpoint carried by a BackendCluster.
type PluginSpec struct {
	Endpoint string `json:"endpoint"`
}

// instanceID returns the spec's instance id, defaulting to the resource name.
func (c BackendCluster) instanceID() string {
	if c.Spec.Instance != "" {
		return c.Spec.Instance
	}
	return c.Metadata.Name
}

// FromBackendClusters folds a set of BackendCluster CRs into a Config: one
// backend type per distinct spec.backendType, its instances and zones gathered
// from the CRs, and a default per type from the CR marked default. It is pure
// (no I/O) so it can be unit-tested without an API server.
func FromBackendClusters(clusters []BackendCluster) (*Config, error) {
	cfg := &Config{
		Backends: map[string]BackendConfig{},
		Defaults: map[string]string{},
	}
	for _, c := range clusters {
		bt := c.Spec.BackendType
		inst := c.instanceID()
		if bt == "" {
			return nil, fmt.Errorf("BackendCluster %q: empty backendType", c.Metadata.Name)
		}
		if c.Spec.Plugin.Endpoint == "" {
			return nil, fmt.Errorf("BackendCluster %q: empty plugin.endpoint", c.Metadata.Name)
		}

		bc, ok := cfg.Backends[bt]
		if !ok {
			bc = BackendConfig{
				Plugin:    &PluginConfig{Endpoint: c.Spec.Plugin.Endpoint},
				Instances: map[string]InstanceConfig{},
			}
		} else if bc.Plugin.Endpoint != c.Spec.Plugin.Endpoint {
			// All instances of one backend type are served by one plugin sidecar;
			// disagreeing endpoints are a config error, not a silent last-wins.
			return nil, fmt.Errorf("backend %q: conflicting plugin endpoints %q and %q", bt, bc.Plugin.Endpoint, c.Spec.Plugin.Endpoint)
		}
		if _, dup := bc.Instances[inst]; dup {
			return nil, fmt.Errorf("backend %q: duplicate instance %q", bt, inst)
		}
		bc.Instances[inst] = InstanceConfig{Zone: c.Spec.Zone}
		cfg.Backends[bt] = bc

		if c.Spec.Default {
			if prev, ok := cfg.Defaults[bt]; ok {
				return nil, fmt.Errorf("backend %q: more than one default instance (%q and %q)", bt, prev, inst)
			}
			cfg.Defaults[bt] = inst
		}
	}
	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("invalid BackendCluster set: %w", err)
	}
	return cfg, nil
}

// LoadFromAPI lists BackendClusters from the in-cluster API server using the
// pod's ServiceAccount credentials and builds a Config.
func LoadFromAPI(ctx context.Context) (*Config, error) {
	items, _, err := listClusters(ctx)
	if err != nil {
		return nil, err
	}
	return FromBackendClusters(items)
}

// listClusters lists BackendClusters and returns them plus the list's
// resourceVersion (the cursor a watch resumes from).
func listClusters(ctx context.Context) ([]BackendCluster, string, error) {
	if !incluster.InCluster() {
		return nil, "", fmt.Errorf("not running in a cluster; use --config-source=file")
	}
	body, err := incluster.Get(ctx, backendClustersPath)
	if err != nil {
		return nil, "", fmt.Errorf("list BackendClusters: %w", err)
	}
	var list struct {
		Metadata struct {
			ResourceVersion string `json:"resourceVersion"`
		} `json:"metadata"`
		Items []BackendCluster `json:"items"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, "", fmt.Errorf("decode BackendCluster list: %w", err)
	}
	return list.Items, list.Metadata.ResourceVersion, nil
}

// watchEvent is one entry of the K8s watch stream.
type watchEvent struct {
	Type   string `json:"type"` // ADDED | MODIFIED | DELETED | BOOKMARK | ERROR
	Object struct {
		Metadata struct {
			ResourceVersion string `json:"resourceVersion"`
		} `json:"metadata"`
	} `json:"object"`
}

// WatchFromAPI watches the BackendCluster collection and calls onChange with a
// freshly built Config on every change (and once on (re)connect, to reconcile
// anything missed). It blocks until ctx is cancelled, transparently resuming the
// watch after the server closes the stream and re-listing when the cursor
// expires. The caller (whose initial config came from LoadFromAPI) typically
// runs this in a goroutine.
func WatchFromAPI(ctx context.Context, onChange func(*Config)) error {
	backoff := time.Second
	rv := "" // "" => (re)list to get a fresh cursor
	for ctx.Err() == nil {
		if rv == "" {
			items, newRV, err := listClusters(ctx)
			if err != nil {
				klog.Warningf("backendcluster watch: list: %v (retry in %s)", err, backoff)
				if sleep(ctx, backoff) {
					backoff = grow(backoff)
				}
				continue
			}
			rv = newRV
			// Reconcile current state: catches changes that happened while the
			// previous cursor was expired. (Redundant with the caller's initial
			// load on first pass, which is harmless.)
			if cfg, err := FromBackendClusters(items); err == nil {
				onChange(cfg)
			}
		}
		lastRV, err := watchCycle(ctx, rv, onChange)
		switch {
		case ctx.Err() != nil:
			return ctx.Err()
		case err != nil:
			klog.Warningf("backendcluster watch: %v (re-list in %s)", err, backoff)
			rv = "" // force a fresh list+cursor
			if sleep(ctx, backoff) {
				backoff = grow(backoff)
			}
		default:
			rv = lastRV // clean server-side timeout: resume from where we left off
			backoff = time.Second
		}
	}
	return ctx.Err()
}

// watchCycle opens one watch from rv and processes events until the stream ends
// (server timeout -> nil err, resume from the returned rv) or fails.
func watchCycle(ctx context.Context, rv string, onChange func(*Config)) (string, error) {
	q := url.Values{
		"watch":               {"1"},
		"resourceVersion":     {rv},
		"timeoutSeconds":      {"290"}, // server closes the stream; we just re-watch
		"allowWatchBookmarks": {"true"},
	}
	body, err := incluster.GetStream(ctx, backendClustersPath+"?"+q.Encode())
	if err != nil {
		return rv, err
	}
	defer body.Close()

	dec := json.NewDecoder(body)
	last := rv
	for {
		var ev watchEvent
		if err := dec.Decode(&ev); err != nil {
			if ctx.Err() != nil || errors.Is(err, io.EOF) {
				return last, nil // clean end / shutdown
			}
			return last, err
		}
		if ev.Object.Metadata.ResourceVersion != "" {
			last = ev.Object.Metadata.ResourceVersion
		}
		switch ev.Type {
		case "ADDED", "MODIFIED", "DELETED":
			cfg, err := LoadFromAPI(ctx)
			if err != nil {
				klog.Warningf("backendcluster %s: reload failed: %v", ev.Type, err)
				continue
			}
			klog.Infof("backendcluster %s -> reloaded backend config (%d type(s))", ev.Type, len(cfg.Backends))
			onChange(cfg)
		case "ERROR":
			return last, fmt.Errorf("watch ERROR event (cursor %q likely expired)", rv)
		}
	}
}

// sleep waits for d or until ctx is done; returns true if it slept the full
// duration (i.e. ctx still live).
func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func grow(d time.Duration) time.Duration {
	if d *= 2; d > 30*time.Second {
		return 30 * time.Second
	}
	return d
}
