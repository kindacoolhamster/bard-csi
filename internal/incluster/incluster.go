// Package incluster does minimal authenticated reads against the Kubernetes API
// from inside a pod, using the ServiceAccount token + the API server CA the
// kubelet projects into every container. It deliberately avoids a client-go
// dependency: Bard core only needs a couple of one-shot GETs (its BackendCluster
// config and its node's zone label), not informers, so a ~60-line REST helper
// keeps the distroless binary tiny.
package incluster

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// serviceAccountDir is where the kubelet projects the pod's ServiceAccount token
// and the API server's CA bundle.
const serviceAccountDir = "/var/run/secrets/kubernetes.io/serviceaccount"

// Get performs an authenticated in-cluster API GET against apiPath (e.g.
// "/apis/bard.io/v1alpha1/backendclusters" or "/api/v1/nodes/<name>") and
// returns the response body.
func Get(ctx context.Context, apiPath string) ([]byte, error) {
	code, body, err := GetCode(ctx, apiPath)
	if err != nil {
		return nil, err
	}
	if code != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %d %s: %s", apiPath, code, http.StatusText(code), string(body))
	}
	return body, nil
}

// GetCode is Get without the non-200-is-an-error policy: it returns the HTTP
// status code and body so callers can branch on 403/404 (RBAC denied / API not
// installed) instead of parsing error text. err is transport-level only.
func GetCode(ctx context.Context, apiPath string) (int, []byte, error) {
	resp, err := doGet(ctx, apiPath, 30*time.Second)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, body, nil
}

// GetStream issues a GET with no client-side timeout and returns the response
// body for streaming reads (used for the K8s watch API, whose response is a
// long-lived chunked stream of JSON events). The caller MUST close the body.
// The request still honours ctx for cancellation.
func GetStream(ctx context.Context, apiPath string) (io.ReadCloser, error) {
	resp, err := doGet(ctx, apiPath, 0) // 0 = no client timeout; ctx + server timeout bound it
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, fmt.Errorf("GET %s: %s: %s", apiPath, resp.Status, string(body))
	}
	return resp.Body, nil
}

// doGet builds and sends an authenticated in-cluster GET. timeout==0 disables
// the client timeout (for streaming).
func doGet(ctx context.Context, apiPath string, timeout time.Duration) (*http.Response, error) {
	host, port := os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT")
	if host == "" || port == "" {
		return nil, fmt.Errorf("not running in a cluster (KUBERNETES_SERVICE_HOST/PORT unset)")
	}
	token, err := os.ReadFile(serviceAccountDir + "/token")
	if err != nil {
		return nil, fmt.Errorf("read ServiceAccount token: %w", err)
	}
	caPEM, err := os.ReadFile(serviceAccountDir + "/ca.crt")
	if err != nil {
		return nil, fmt.Errorf("read API CA bundle: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("parse API CA bundle")
	}
	client := &http.Client{
		Timeout:   timeout,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}},
	}
	url := fmt.Sprintf("https://%s:%s%s", host, port, apiPath)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+string(token))
	req.Header.Set("Accept", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", apiPath, err)
	}
	return resp, nil
}

// InCluster reports whether the process is running inside a Kubernetes pod.
func InCluster() bool {
	return os.Getenv("KUBERNETES_SERVICE_HOST") != ""
}

// NodeZone reads labelKey from the named Node's labels (e.g.
// "topology.kubernetes.io/zone"), returning "" if the label is absent. This is
// how a node plugin learns the topology zone it serves without a hardcoded env.
func NodeZone(ctx context.Context, nodeName, labelKey string) (string, error) {
	body, err := Get(ctx, "/api/v1/nodes/"+nodeName)
	if err != nil {
		return "", err
	}
	return nodeZoneFromJSON(body, labelKey)
}

// nodeZoneFromJSON extracts a label value from a Node object's JSON. Split out
// from NodeZone so it can be unit-tested without an API server.
func nodeZoneFromJSON(body []byte, labelKey string) (string, error) {
	var node struct {
		Metadata struct {
			Labels map[string]string `json:"labels"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(body, &node); err != nil {
		return "", fmt.Errorf("decode Node: %w", err)
	}
	return node.Metadata.Labels[labelKey], nil
}

// NodeCrushLocation reads labelKeys from the named Node and formats the present
// ones as a Ceph CRUSH location ("region:r1|zone:z1"). It lets a node plugin
// place reads near the data (RBD read-affinity) using the node's own topology,
// with no client-go and no per-backend Kubernetes access.
func NodeCrushLocation(ctx context.Context, nodeName string, labelKeys []string) (string, error) {
	body, err := Get(ctx, "/api/v1/nodes/"+nodeName)
	if err != nil {
		return "", err
	}
	return crushLocationFromJSON(body, labelKeys)
}

// crushLocationFromJSON builds "<type>:<val>|..." from a Node object's labels,
// where each CRUSH bucket type is the label key's last path segment
// ("topology.kubernetes.io/zone" -> "zone"). Absent labels are skipped, so an
// empty result means none of labelKeys are set. Split out for unit testing.
func crushLocationFromJSON(body []byte, labelKeys []string) (string, error) {
	var node struct {
		Metadata struct {
			Labels map[string]string `json:"labels"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(body, &node); err != nil {
		return "", fmt.Errorf("decode Node: %w", err)
	}
	var parts []string
	for _, key := range labelKeys {
		if val := node.Metadata.Labels[key]; val != "" {
			parts = append(parts, crushBucketType(key)+":"+val)
		}
	}
	return strings.Join(parts, "|"), nil
}

// crushBucketType maps a node label key to a CRUSH bucket type: its last path
// segment (after the final '/'), or the whole key if it has none.
func crushBucketType(labelKey string) string {
	if i := strings.LastIndex(labelKey, "/"); i >= 0 {
		return labelKey[i+1:]
	}
	return labelKey
}
