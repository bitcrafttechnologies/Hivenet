package reconcile

import "time"

// State is the lifecycle state of a node or link as the canvas renders it
// (spec §9): pending is a dashed outline, applying a spinner, up a solid
// border, error a red border with the message attached.
type State string

const (
	StatePending  State = "pending"
	StateApplying State = "applying"
	StateUp       State = "up"
	StateError    State = "error"
)

// EntityStatus is the reported state of one node or one link.
type EntityStatus struct {
	ID      string `json:"id"`
	State   State  `json:"state"`
	Message string `json:"message,omitempty"`
	// Drift marks state that diverged from what Hivenet asked for — someone
	// poked the VM by hand, or an apply half-landed.
	//
	// It is a separate flag rather than a fifth State because a drifted node is
	// still running: v1 surfaces drift and deliberately does not auto-correct
	// it (spec §5.6).
	Drift bool `json:"drift,omitempty"`
}

// Status is the payload of a topology_status WebSocket push: the full per-node
// and per-link picture after a reconcile cycle.
type Status struct {
	Version   int                     `json:"version"`
	Nodes     map[string]EntityStatus `json:"nodes"`
	Links     map[string]EntityStatus `json:"links"`
	UpdatedAt time.Time               `json:"updatedAt"`
	// Error is a cycle-level failure — one that prevented the reconcile from
	// running at all, such as an unreachable podman socket. Per-entity failures
	// live on the entity instead.
	Error string `json:"error,omitempty"`
}

func newStatus(version int) Status {
	return Status{
		Version:   version,
		Nodes:     make(map[string]EntityStatus),
		Links:     make(map[string]EntityStatus),
		UpdatedAt: time.Now(),
	}
}

// Clone returns a deep copy, so a status handed to a subscriber can never be
// mutated by the next cycle.
func (s Status) Clone() Status {
	out := s
	out.Nodes = make(map[string]EntityStatus, len(s.Nodes))
	for k, v := range s.Nodes {
		out.Nodes[k] = v
	}
	out.Links = make(map[string]EntityStatus, len(s.Links))
	for k, v := range s.Links {
		out.Links[k] = v
	}
	return out
}
