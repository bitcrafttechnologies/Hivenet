package store

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/bitcrafttech/hivenet/internal/topology"
)

func oneHost() topology.Topology {
	return topology.Topology{Nodes: []topology.Node{{
		ID: "h1", Type: topology.TypeHost, Image: "alpine",
		Interfaces: []topology.Interface{{ID: "eth0", Name: "eth0"}},
	}}}
}

func TestReplaceBumpsVersion(t *testing.T) {
	s := New()
	if got := s.Get().Version; got != 0 {
		t.Fatalf("initial version = %d, want 0", got)
	}

	doc, changed, err := s.Replace(oneHost())
	if err != nil {
		t.Fatalf("Replace: %v", err)
	}
	if !changed {
		t.Error("changed = false, want true for a new topology")
	}
	if doc.Version != 1 {
		t.Errorf("version = %d, want 1", doc.Version)
	}
}

// A frontend that re-sends the same document should not cause a reconcile.
// Without this, a chatty client produces a reconcile storm.
func TestReplaceIsANoOpWhenNothingChanged(t *testing.T) {
	s := New()
	if _, _, err := s.Replace(oneHost()); err != nil {
		t.Fatalf("Replace: %v", err)
	}

	doc, changed, err := s.Replace(oneHost())
	if err != nil {
		t.Fatalf("Replace: %v", err)
	}
	if changed {
		t.Error("changed = true for an identical topology, want false")
	}
	if doc.Version != 1 {
		t.Errorf("version = %d, want it to stay at 1", doc.Version)
	}
}

func TestReplaceRejectsInvalidTopologyWithoutBumpingVersion(t *testing.T) {
	s := New()
	broken := topology.Topology{Nodes: []topology.Node{{ID: "h1", Type: "nonsense"}}}

	if _, _, err := s.Replace(broken); err == nil {
		t.Fatal("expected a validation error")
	}
	if got := s.Get().Version; got != 0 {
		t.Errorf("version = %d after a rejected write, want 0", got)
	}
}

func TestReplaceNormalizesBeforeStoring(t *testing.T) {
	s := New()
	if _, _, err := s.Replace(oneHost()); err != nil {
		t.Fatalf("Replace: %v", err)
	}
	if got := s.Get().Topology.Nodes[0].Owner; got != topology.DefaultOwner {
		t.Errorf("owner = %q, want it defaulted to %q", got, topology.DefaultOwner)
	}
}

// Every read hands out a copy. A caller mutating what it got back must not be
// able to corrupt the authoritative desired state.
func TestGetReturnsAnIsolatedCopy(t *testing.T) {
	s := New()
	if _, _, err := s.Replace(oneHost()); err != nil {
		t.Fatalf("Replace: %v", err)
	}

	doc := s.Get()
	doc.Topology.Nodes[0].Image = "tampered"
	doc.Topology.Nodes[0].Interfaces[0].Name = "tampered"

	stored := s.Get()
	if stored.Topology.Nodes[0].Image != "alpine" {
		t.Error("mutating a returned document changed stored state")
	}
	if stored.Topology.Nodes[0].Interfaces[0].Name != "eth0" {
		t.Error("mutating a returned interface changed stored state")
	}
}

func TestSubscribeSignalsOnChange(t *testing.T) {
	s := New()
	changes, unsubscribe := s.Subscribe()
	defer unsubscribe()

	if _, _, err := s.Replace(oneHost()); err != nil {
		t.Fatalf("Replace: %v", err)
	}

	select {
	case <-changes:
	case <-time.After(time.Second):
		t.Fatal("subscriber was not notified of a topology change")
	}
}

func TestSubscribeDoesNotSignalOnNoOpWrite(t *testing.T) {
	s := New()
	if _, _, err := s.Replace(oneHost()); err != nil {
		t.Fatalf("Replace: %v", err)
	}

	changes, unsubscribe := s.Subscribe()
	defer unsubscribe()

	if _, _, err := s.Replace(oneHost()); err != nil {
		t.Fatalf("Replace: %v", err)
	}

	select {
	case <-changes:
		t.Error("an identical write must not wake the reconciler")
	case <-time.After(50 * time.Millisecond):
	}
}

func TestLayoutStoreRoundTrip(t *testing.T) {
	l := NewLayout()
	if got := string(l.Get()); got != "{}" {
		t.Errorf("initial layout = %s, want {}", got)
	}

	payload := json.RawMessage(`{"nodes":{"h1":{"x":10,"y":20}},"zoom":1.5}`)
	if err := l.Set(payload); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if got := string(l.Get()); got != string(payload) {
		t.Errorf("layout = %s, want %s", got, payload)
	}
}

func TestLayoutStoreRejectsInvalidJSON(t *testing.T) {
	l := NewLayout()
	if err := l.Set(json.RawMessage(`{not json`)); err == nil {
		t.Fatal("expected an error for a malformed payload")
	}
	if got := string(l.Get()); got != "{}" {
		t.Errorf("a rejected write changed stored layout: %s", got)
	}
}
