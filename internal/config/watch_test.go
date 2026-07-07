package config

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestWatchEventParse covers decoding the K8s watch stream entries the watcher
// reacts to: the event type drives a reload, the object's resourceVersion is the
// cursor a resumed watch starts from.
func TestWatchEventParse(t *testing.T) {
	line := `{"type":"MODIFIED","object":{"apiVersion":"bard.io/v1alpha1","kind":"BackendCluster","metadata":{"name":"kepler-rbd","resourceVersion":"4242"},"spec":{"backendType":"ceph-rbd"}}}`
	var ev watchEvent
	if err := json.Unmarshal([]byte(line), &ev); err != nil {
		t.Fatal(err)
	}
	if ev.Type != "MODIFIED" {
		t.Fatalf("type: %q", ev.Type)
	}
	if ev.Object.Metadata.ResourceVersion != "4242" {
		t.Fatalf("resourceVersion: %q", ev.Object.Metadata.ResourceVersion)
	}
}

// TestWatchStreamDecodes confirms successive newline-delimited events decode off
// one stream the way the watch loop reads them.
func TestWatchStreamDecodes(t *testing.T) {
	stream := strings.Join([]string{
		`{"type":"ADDED","object":{"metadata":{"resourceVersion":"1"}}}`,
		`{"type":"BOOKMARK","object":{"metadata":{"resourceVersion":"2"}}}`,
		`{"type":"DELETED","object":{"metadata":{"resourceVersion":"3"}}}`,
	}, "\n")
	dec := json.NewDecoder(strings.NewReader(stream))
	var types []string
	var last string
	for {
		var ev watchEvent
		if err := dec.Decode(&ev); err != nil {
			break
		}
		types = append(types, ev.Type)
		last = ev.Object.Metadata.ResourceVersion
	}
	if strings.Join(types, ",") != "ADDED,BOOKMARK,DELETED" {
		t.Fatalf("event order: %v", types)
	}
	if last != "3" {
		t.Fatalf("final cursor: %q", last)
	}
}
